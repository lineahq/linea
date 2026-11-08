package engine

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// perRecord are the processor types that act on each record on its own and
// emit every input at once, so a filter can move across them when its
// verdict does not depend on what they change.
var perRecord = map[string]bool{
	"transform": true, "attributes": true, "resource": true, "redaction": true,
	"filter": true, "span": true, "probabilistic_sampler": true,
}

// fromEnvironment are processors that add data from the environment, which
// a debug run does not provide.
var fromEnvironment = map[string]bool{"k8sattributes": true, "resourcedetection": true}

// placeFilters checks, for every filter that dropped records in a logs or
// traces pipeline, which processors before it it could run before: the
// processors must work record by record, must not have failed on this
// input, and the filter's conditions must give every record the same
// verdict at their input as at its own (verdict invariance). Only requests
// whose filter replay is verified are compared. See
// docs/internals/recommendations.md.
func placeFilters(ctx context.Context, resolved *config.Resolved, g *topology.Graph, stages []analysis.Stage, lineage []analysis.RecordLineage, filtering []*analysis.Filtering, result *runresult.Result) ([]recommend.Placement, error) {
	verdict, reads := features.FilterVerdict(), features.FilterReads()
	if verdict == nil || reads == nil || g == nil {
		return nil, nil
	}
	p := placer{ctx: ctx, resolved: resolved, graph: g, stages: stages, lineage: lineage, result: result, verdict: verdict,
		reads: reads, bySeq: map[int]instrument.Event{}}
	for _, e := range result.Events {
		p.bySeq[e.Seq] = e
	}
	var out []recommend.Placement
	for _, pipe := range g.Pipelines {
		if pipe.Signal != "logs" && pipe.Signal != "traces" {
			continue
		}
		ids := processorIDs(pipe)
		for k, id := range ids {
			if typeOf(id) != "filter" || k == 0 {
				continue
			}
			pl := recommend.Placement{Filter: id, Pipeline: pipe.ID, Signal: pipe.Signal}
			var requests []*analysis.Filtering
			for _, f := range filtering {
				if f.Processor != id || f.Pipeline != pipe.ID {
					continue
				}
				if f.Verification != analysis.FilterVerified {
					pl.Skipped++
					continue
				}
				requests = append(requests, f)
				pl.Requests++
				pl.Records += f.Records
				pl.Dropped += f.Dropped
			}
			if pl.Dropped == 0 {
				continue
			}
			fields, b, err := p.filterReads(id, pipe.Signal)
			if err != nil {
				return nil, err
			}
			if b == "" {
				b = filterBlocker(id, requests)
			}
			if b != "" {
				pl.Checks = []recommend.PlacementCheck{{Processor: ids[k-1], Blocker: b}}
				out = append(out, pl)
				continue
			}
			for j := k - 1; j >= 0; j-- {
				b, err := p.check(pipe.ID, pipe.Signal, ids[j], id, fields, requests)
				if err != nil {
					return nil, err
				}
				pl.Checks = append(pl.Checks, recommend.PlacementCheck{Processor: ids[j], Blocker: b})
				if b != "" {
					break
				}
			}
			out = append(out, pl)
		}
	}
	return out, nil
}

// filterReads lists the fields the filter's conditions read; the blocker
// is set when they cannot be listed.
func (p *placer) filterReads(filter, signal string) ([]features.FieldRead, string, error) {
	var id component.ID
	if err := id.UnmarshalText([]byte(filter)); err != nil {
		return nil, "", err
	}
	var sig pipeline.Signal
	if err := sig.UnmarshalText([]byte(signal)); err != nil {
		return nil, "", err
	}
	fields, reason := p.reads(p.resolved.Config.Processors[id], sig)
	if reason != "" {
		return nil, fmt.Sprintf("The fields %s's conditions read could not be listed (%s), so whether the processors before it change them is not known.", filter, reason), nil
	}
	return fields, "", nil
}

// filterBlocker is why the filter cannot move at all: it runs under
// error_mode propagate, or failed on this input.
func filterBlocker(id string, requests []*analysis.Filtering) string {
	for _, f := range requests {
		if f.Error != "" {
			return fmt.Sprintf("%s returned an error on this input, which dropped the whole request; moved earlier, it would fail on records the processors before it have not changed yet.", id)
		}
		for _, c := range f.Conditions {
			if c.ErrorMode == "propagate" {
				return fmt.Sprintf("%s runs under error_mode propagate: moved earlier, a condition failing on a record the processors before it have not changed yet would drop the whole request.", id)
			}
		}
	}
	return ""
}

type placer struct {
	ctx      context.Context
	resolved *config.Resolved
	graph    *topology.Graph
	stages   []analysis.Stage
	lineage  []analysis.RecordLineage
	result   *runresult.Result
	verdict  features.FilterVerdictFunc
	reads    features.FilterReadsFunc
	bySeq    map[int]instrument.Event
}

// check returns why filter cannot run before processor in pipeline, or ""
// when it can on this input.
func (p *placer) check(pipeline, signal, processor, filter string, fields []features.FieldRead, requests []*analysis.Filtering) (string, error) {
	typ := typeOf(processor)
	if c, ok := p.graph.Component(string(topology.KindProcessor) + "/" + processor); !ok || c.Mode != topology.ModeReal {
		return fmt.Sprintf("%s did not run as the real component in this run, so what it changes is not known.", processor), nil
	}
	switch {
	case typ == "memory_limiter":
		return fmt.Sprintf("%s guards the pipeline and stays in front of it.", processor), nil
	case fromEnvironment[typ]:
		return fmt.Sprintf("%s adds data from the environment, which a debug run does not provide, so whether %s reads it is not verified.", processor, filter), nil
	case !perRecord[typ]:
		return fmt.Sprintf("%s is not known to work record by record (it may buffer, regroup or decide on several records), so %s is not moved across it.", processor, filter), nil
	}
	// What the configuration may write decides before this input does: a
	// field the input already held, or a record the processor's conditions
	// skipped, shows no change here and may on the next input.
	if b, err := p.configured(signal, processor, filter, fields); b != "" || err != nil {
		return b, err
	}
	for _, s := range p.stages {
		if s.Component != processor || s.Pipeline != pipeline {
			continue
		}
		for _, in := range s.Inputs {
			if in.Error != "" {
				return fmt.Sprintf("%s returned an error on this input, so whether moving %s before it changes the output is not verified.", processor, filter), nil
			}
		}
	}
	return p.invariant(pipeline, processor, filter, fields, requests)
}

// configured is why filter cannot run before processor by what the
// processor's configuration may write; "" when none of it is read.
func (p *placer) configured(signal, processor, filter string, fields []features.FieldRead) (string, error) {
	var id component.ID
	if err := id.UnmarshalText([]byte(processor)); err != nil {
		return "", err
	}
	var sig pipeline.Signal
	if err := sig.UnmarshalText([]byte(signal)); err != nil {
		return "", err
	}
	writes, reason := configuredWrites(typeOf(processor), p.resolved.Config.Processors[id], sig)
	if reason != "" {
		return fmt.Sprintf("What %s's configuration may change could not be listed (%s), so whether %s reads it is not known.", processor, reason, filter), nil
	}
	return writeConflict(processor, filter, fields, writes), nil
}

// record is one record that reached the filter, with its position at the
// earlier processor's input.
type record struct {
	name    string
	earlier int               // seq of the earlier processor's input
	ref     string            // position there
	dropped bool              // the filter dropped it
	changes []analysis.Change // what the earlier processor changed on it
}

// invariant checks that processor changed no field the filter reads on the
// records the filter evaluated, then compares the filter's verdict on each
// of them at processor's input with its recorded verdict.
func (p *placer) invariant(pipeline, processor, filter string, fields []features.FieldRead, requests []*analysis.Filtering) (string, error) {
	var records []record
	unfollowed := 0
	for _, f := range requests {
		for _, l := range p.lineage {
			for i, h := range l.Hops {
				if h.Seq != f.Input || h.Kind != instrument.KindIngress {
					continue
				}
				at, egress, ok := ancestor(l.Hops, i, processor, pipeline)
				if !ok {
					unfollowed++
					continue
				}
				records = append(records, record{name: l.Name, earlier: l.Hops[at].Seq, ref: l.Hops[at].Ref, dropped: under(h.Ref, f.Drops),
					changes: l.Hops[egress].Changes})
			}
		}
	}
	if unfollowed > 0 {
		return fmt.Sprintf("%s reaching %s could not be followed back to the input of %s with confidence, so their verdict there is not known.", plural(unfollowed, "record"), filter, processor), nil
	}
	if b := conflict(processor, filter, fields, records); b != "" {
		return b, nil
	}
	var id component.ID
	if err := id.UnmarshalText([]byte(filter)); err != nil {
		return "", err
	}
	cfg := p.resolved.Config.Processors[id]
	drops := map[int][]string{}
	for _, r := range records {
		if _, ok := drops[r.earlier]; ok {
			continue
		}
		payload, err := p.result.Snapshots.Payload(p.bySeq[r.earlier].Snapshot)
		if err != nil {
			return "", err
		}
		d, reason := p.verdict(p.ctx, cfg, payload)
		if reason != "" {
			return fmt.Sprintf("%s's conditions could not be evaluated at the input of %s: %s", filter, processor, reason), nil
		}
		drops[r.earlier] = d
	}
	for _, r := range records {
		before := underRefs(r.ref, drops[r.earlier])
		if before == r.dropped {
			continue
		}
		verdicts := [2]string{"kept", "dropped"}
		if before {
			verdicts = [2]string{"dropped", "kept"}
		}
		return fmt.Sprintf("%s's conditions read what %s changes: %s would be %s at the input of %s but was %s by %s.",
			filter, processor, describe(r.name), verdicts[0], processor, verdicts[1], filter), nil
	}
	return "", nil
}

// ancestor walks a record's hops back from hop i to its input at processor
// in pipeline, and returns that hop and the processor's output hop for the
// record. Every hop on the way must be matched by more than content
// similarity.
func ancestor(hops []analysis.Hop, i int, processor, pipeline string) (at, egress int, ok bool) {
	child := -1
	for i >= 0 {
		h := hops[i]
		if h.Confidence == analysis.ConfidenceMedium {
			return 0, 0, false
		}
		if h.Kind == instrument.KindIngress && h.Component == processor && h.Pipeline == pipeline {
			if child < 0 || hops[child].Kind != instrument.KindEgress || hops[child].Component != processor {
				return 0, 0, false
			}
			return i, child, true
		}
		if h.Parent >= i {
			return 0, 0, false
		}
		child, i = i, h.Parent
	}
	return 0, 0, false
}

// conflict names a field the filter reads that processor changed on a
// record the filter evaluated; "" when there is none.
func conflict(processor, filter string, fields []features.FieldRead, records []record) string {
	for _, f := range fields {
		n := 0
		for _, r := range records {
			if slices.ContainsFunc(r.changes, func(c analysis.Change) bool { return touches(f, c) }) {
				n++
			}
		}
		if n > 0 {
			return fmt.Sprintf("%s reads %s, which %s changed on %s it evaluated; placed before %s, it would decide on the value before the change.",
				filter, readName(f), processor, plural(n, "record"), processor)
		}
	}
	return ""
}

// aliases are OTTL field names whose recorded field has another name.
var aliases = map[string]string{
	"time": "time_unix_nano", "observed_time": "observed_time_unix_nano",
	"start_time": "start_time_unix_nano", "end_time": "end_time_unix_nano",
}

// touches reports whether a recorded change is to the field f reads. A
// change is at the resource, the scope or the record by its entity; its
// path names the field ("attributes", "status.code") and map keys below it
// (attributes["k"]).
func touches(f features.FieldRead, c analysis.Change) bool {
	level := features.LevelRecord
	switch strings.Count(c.Entity, ".") {
	case 0:
		level = features.LevelResource
	case 1:
		level = features.LevelScope
	}
	if level != f.Level {
		return false
	}
	field, rest, _ := strings.Cut(c.Path, "[")
	field, _, _ = strings.Cut(field, ".")
	want := f.Field
	if a, ok := aliases[want]; ok {
		want = a
	}
	if field != want {
		return false
	}
	if f.Key == "" || rest == "" {
		return true
	}
	key, _, _ := strings.Cut(rest, "]")
	k, err := strconv.Unquote(key)
	return err != nil || k == f.Key
}

// readName names a read field as OTTL writes it.
func readName(f features.FieldRead) string {
	name := f.Field
	if f.Key != "" {
		name += "[" + strconv.Quote(f.Key) + "]"
	}
	if f.Event != "" {
		name = "span event " + f.Event
	}
	if f.Level != features.LevelRecord {
		name = string(f.Level) + "." + name
	}
	return name
}

func under(ref string, drops []analysis.FilterDrop) bool {
	for _, d := range drops {
		if ref == d.Ref || strings.HasPrefix(ref, d.Ref+".") {
			return true
		}
	}
	return false
}

func underRefs(ref string, drops []string) bool {
	for _, d := range drops {
		if ref == d || strings.HasPrefix(ref, d+".") {
			return true
		}
	}
	return false
}

func describe(name string) string {
	if name == "" {
		return "a record"
	}
	return "record " + name
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// processorIDs are a pipeline's processors as configuration IDs.
func processorIDs(p topology.Pipeline) []string {
	out := make([]string, len(p.Processors))
	for i, ref := range p.Processors {
		out[i] = strings.TrimPrefix(ref, string(topology.KindProcessor)+"/")
	}
	return out
}

// typeOf is the type part of a component ID ("filter/health" is "filter").
func typeOf(id string) string {
	t, _, _ := strings.Cut(id, "/")
	return t
}
