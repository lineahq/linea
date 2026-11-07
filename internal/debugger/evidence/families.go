package evidence

import (
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Item refs name what an item's data is:
//   - stage_changes: "change:<n>" (ChangeItem), "removed:<n>" and "added:<n>"
//     (analysis.RecordRef), "unresolved:<n>" (a description).
//   - records: "<seq>:<record position>" (RecordItem), the ID lineage uses
//     for a record injected at that event.
//   - journey: "record" (JourneyRecord), "hop:<n>" (analysis.Hop),
//     "delivery:<n>" (analysis.Delivery), "stop:<n>" (analysis.Stop).
//   - statements: "replay:<input seq>" (ReplayItem), "group:<input
//     seq>:<index>" (ottldebug.Group).
//   - events: "event:<seq>" (instrument.Event; snapshots by reference).

// ChangeItem is one (path, op) of a stage with every entity it changed, the
// first change's values and the lowest match confidence among them.
type ChangeItem struct {
	Path       string              `json:"path"`
	Op         analysis.Op         `json:"op"`
	Count      int                 `json:"count"`
	Entities   []string            `json:"entities"`
	Before     any                 `json:"before,omitempty"`
	After      any                 `json:"after,omitempty"`
	BeforeType string              `json:"before_type,omitempty"`
	AfterType  string              `json:"after_type,omitempty"`
	Confidence analysis.Confidence `json:"confidence,omitempty"`
}

// RecordItem is one recorded record with its resource and scope.
type RecordItem struct {
	Seq      int            `json:"seq"`
	Record   string         `json:"record"`
	Signal   string         `json:"signal"`
	Resource ResourceView   `json:"resource"`
	Scope    ScopeView      `json:"scope"`
	Fields   map[string]any `json:"fields"`
}

type ResourceView struct {
	Attributes map[string]any `json:"attributes"`
	SchemaURL  string         `json:"schema_url,omitempty"`
}

type ScopeView struct {
	Name       string         `json:"name,omitempty"`
	Version    string         `json:"version,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	SchemaURL  string         `json:"schema_url,omitempty"`
}

// JourneyRecord is a lineage's record, without its hops, deliveries and
// stops (their own items).
type JourneyRecord struct {
	ID        string   `json:"id"`
	Signal    string   `json:"signal"`
	Receiver  string   `json:"receiver,omitempty"`
	Origin    string   `json:"origin,omitempty"`
	Connector string   `json:"connector,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Name      string   `json:"name"`
	Span      string   `json:"span,omitempty"`
	Hops      int      `json:"hops"`
}

// ReplayItem is one OTTL replay of a processor input, without its groups
// (their own items).
type ReplayItem struct {
	Processor    string                 `json:"processor"`
	Pipeline     string                 `json:"pipeline,omitempty"`
	Input        int                    `json:"input"`
	Output       int                    `json:"output,omitempty"`
	Signal       string                 `json:"signal"`
	Groups       int                    `json:"groups"`
	Verification ottldebug.Verification `json:"verification"`
	Error        string                 `json:"error,omitempty"`
	Mismatch     *analysis.Diff         `json:"mismatch,omitempty"`
	Notes        []string               `json:"notes,omitempty"`
}

func project(rep *engine.Report, q Query) ([]entry, error) {
	if cut := rep.Result.Truncation; cut != nil && len(cut.Omitted) > 0 && slices.Contains(analysisFamilies, q.Family) {
		return nil, nil
	}
	switch q.Family {
	case FamilyStageChanges:
		stage, err := findStage(rep, q)
		if err != nil {
			return nil, err
		}
		return stageChanges(stage), nil
	case FamilyRecords:
		stage, err := findStage(rep, q)
		if err != nil {
			return nil, err
		}
		return records(rep, stage, q.Side)
	case FamilyJourney:
		return journey(rep, q.Entity)
	case FamilyStatements:
		return statements(rep, q)
	case FamilyRecommendations:
		return recommendations(rep, q), nil
	}
	return events(rep, q), nil
}

func findStage(rep *engine.Report, q Query) (*analysis.Stage, error) {
	var found []*analysis.Stage
	for i := range rep.Stages {
		s := &rep.Stages[i]
		if s.Component == q.Component && s.Pipeline == q.Pipeline && (q.Signal == "" || s.Signal == q.Signal) {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%w: no stage %q in pipeline %q", ErrNotFound, q.Component, q.Pipeline)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%w: stage %q in pipeline %q has several signals; name the signal", ErrInvalidQuery, q.Component, q.Pipeline)
}

// stageDiffs are the diffs a stage's changes come from: its reconciliation
// when it buffers, otherwise its per-input diffs (as the summary groups them).
func stageDiffs(stage *analysis.Stage) []*analysis.Diff {
	if stage.Reconciliation != nil {
		return []*analysis.Diff{stage.Reconciliation}
	}
	var diffs []*analysis.Diff
	for _, input := range stage.Inputs {
		if input.Diff != nil {
			diffs = append(diffs, input.Diff)
		}
	}
	return diffs
}

func stageChanges(stage *analysis.Stage) []entry {
	type key struct {
		path string
		op   analysis.Op
	}
	var groups []*ChangeItem
	index := map[key]*ChangeItem{}
	var removed, added []analysis.RecordRef
	var unresolved []string
	for _, diff := range stageDiffs(stage) {
		removed = append(removed, diff.Removed...)
		added = append(added, diff.Added...)
		unresolved = append(unresolved, diff.Unresolved...)
		for _, c := range diff.Changes {
			g, ok := index[key{c.Path, c.Op}]
			if !ok {
				g = &ChangeItem{Path: c.Path, Op: c.Op, Entities: []string{}, Before: c.Before, After: c.After,
					BeforeType: c.BeforeType, AfterType: c.AfterType}
				index[key{c.Path, c.Op}] = g
				groups = append(groups, g)
			}
			g.Count++
			if c.Entity != "" {
				g.Entities = append(g.Entities, c.Entity)
			}
			if confidenceRank(c.Confidence) < confidenceRank(g.Confidence) {
				g.Confidence = c.Confidence
			}
		}
	}
	var out []entry
	for i, g := range groups {
		out = append(out, entry{fmt.Sprintf("change:%d", i), g})
	}
	for i, r := range removed {
		out = append(out, entry{fmt.Sprintf("removed:%d", i), r})
	}
	for i, r := range added {
		out = append(out, entry{fmt.Sprintf("added:%d", i), r})
	}
	for i, u := range unresolved {
		out = append(out, entry{fmt.Sprintf("unresolved:%d", i), u})
	}
	return out
}

// records flattens the stage's input payloads (before) or its emissions
// (after: correlated outputs and later emissions), in event order.
func records(rep *engine.Report, stage *analysis.Stage, side string) ([]entry, error) {
	var seqs []int
	for _, input := range stage.Inputs {
		if side == SideBefore {
			seqs = append(seqs, input.Seq)
		} else {
			seqs = append(seqs, input.Outputs...)
		}
	}
	if side == SideAfter {
		seqs = append(seqs, stage.Emissions...)
	}
	slices.Sort(seqs)
	seqs = slices.Compact(seqs)
	bySeq := eventsBySeq(rep)
	var out []entry
	for _, seq := range seqs {
		e, ok := bySeq[seq]
		if !ok || e.Snapshot == "" {
			continue
		}
		p, err := rep.Result.Snapshots.Payload(e.Snapshot)
		if err != nil {
			return nil, err
		}
		for _, r := range telemetry.Records(p) {
			resource, resourceURL := p.ResourceAt(r.Resource)
			scope, scopeURL := p.ScopeAt(r.Resource, r.Scope)
			ref := r.Ref(p.Signal)
			out = append(out, entry{fmt.Sprintf("%d:%s", seq, ref), RecordItem{
				Seq: seq, Record: ref, Signal: p.Signal.String(),
				Resource: ResourceView{Attributes: raw(resource.Attributes()), SchemaURL: resourceURL},
				Scope: ScopeView{Name: scope.Name(), Version: scope.Version(), Attributes: optionalRaw(scope.Attributes()),
					SchemaURL: scopeURL},
				Fields: raw(r.Fields),
			}})
		}
	}
	return out, nil
}

func raw(m pcommon.Map) map[string]any {
	out := m.AsRaw()
	if out == nil {
		return map[string]any{}
	}
	return out
}

func optionalRaw(m pcommon.Map) map[string]any {
	if m.Len() == 0 {
		return nil
	}
	return m.AsRaw()
}

func journey(rep *engine.Report, entity string) ([]entry, error) {
	for _, l := range rep.Lineage {
		if l.ID != entity {
			continue
		}
		out := []entry{{"record", JourneyRecord{ID: l.ID, Signal: l.Signal, Receiver: l.Receiver, Origin: l.Origin,
			Connector: l.Connector, Sources: l.Sources, Name: l.Name, Span: l.Span, Hops: len(l.Hops)}}}
		for i, h := range l.Hops {
			out = append(out, entry{fmt.Sprintf("hop:%d", i), h})
		}
		for i, d := range l.Deliveries {
			out = append(out, entry{fmt.Sprintf("delivery:%d", i), d})
		}
		for i, s := range l.Stops {
			out = append(out, entry{fmt.Sprintf("stop:%d", i), s})
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: no record %q", ErrNotFound, entity)
}

func statements(rep *engine.Report, q Query) ([]entry, error) {
	bySeq := eventsBySeq(rep)
	// A transform stage whose inputs were not replayable (failed, or fanned
	// out into several outputs) answers no items rather than not_found.
	known := slices.ContainsFunc(rep.Stages, func(s analysis.Stage) bool {
		return s.Component == q.Component && s.Kind == "processor" && s.Type == "transform"
	})
	var out []entry
	for _, r := range rep.OTTL {
		if r == nil || r.Processor != q.Component {
			continue
		}
		known = true
		pipeline := bySeq[r.Input].Pipeline
		if q.Pipeline != "" && pipeline != q.Pipeline {
			continue
		}
		out = append(out, entry{fmt.Sprintf("replay:%d", r.Input), ReplayItem{Processor: r.Processor, Pipeline: pipeline,
			Input: r.Input, Output: r.Output, Signal: r.Signal, Groups: len(r.Groups), Verification: r.Verification,
			Error: r.Error, Mismatch: r.Mismatch, Notes: r.Notes}})
		for _, g := range r.Groups {
			out = append(out, entry{fmt.Sprintf("group:%d:%d", r.Input, g.Index), g})
		}
	}
	if !known {
		return nil, fmt.Errorf("%w: no OTTL replay of processor %q", ErrNotFound, q.Component)
	}
	return out, nil
}

func events(rep *engine.Report, q Query) []entry {
	var out []entry
	for _, e := range rep.Result.Events {
		if e.Seq < q.FromSeq || q.ToSeq != 0 && e.Seq > q.ToSeq || q.Component != "" && e.Component != q.Component {
			continue
		}
		out = append(out, entry{fmt.Sprintf("event:%d", e.Seq), e})
	}
	return out
}

func eventsBySeq(rep *engine.Report) map[int]instrument.Event {
	m := make(map[int]instrument.Event, len(rep.Result.Events))
	for _, e := range rep.Result.Events {
		m[e.Seq] = e
	}
	return m
}

// recommendations are the run's recommendations; item refs are their
// positions in the report ("recommendation:<n>"), so a filtered page still
// names the report entry. A pipeline filter keeps recommendations about the
// whole configuration, which apply to every pipeline.
func recommendations(rep *engine.Report, q Query) []entry {
	var out []entry
	for i, r := range rep.Recommendations {
		if q.Pipeline != "" && r.Pipeline != "" && r.Pipeline != q.Pipeline {
			continue
		}
		if q.Component != "" && !slices.ContainsFunc(r.Components, func(ref string) bool {
			_, id, _ := strings.Cut(ref, "/")
			return id == q.Component
		}) {
			continue
		}
		out = append(out, entry{fmt.Sprintf("recommendation:%d", i), r})
	}
	return out
}
