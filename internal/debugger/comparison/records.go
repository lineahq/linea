package comparison

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/evidence"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// injected is one injected record's lineage, keyed by its input and position.
type injected struct {
	input   int
	ref     string
	lineage *analysis.RecordLineage
}

// injectedRecords keys a run's injected lineages by input (from 1) and
// position. The k-th input.injected event of the replay receiver is the k-th
// input: inputs are injected one after another in order. Records a receiver
// produced from a mock and data points an aggregating connector emitted are
// not keyed; they are compared at the exporters only.
func (s *side) injectedRecords() map[[2]string]injected {
	inputOf := map[int]int{}
	for _, e := range s.report.Result.Events {
		if e.Kind == instrument.KindInput && e.Source == instrument.SourceReplayReceiver {
			inputOf[e.Seq] = len(inputOf) + 1
		}
	}
	out := map[[2]string]injected{}
	for i := range s.report.Lineage {
		l := &s.report.Lineage[i]
		if l.Origin != "" {
			continue
		}
		seq, ref, ok := strings.Cut(l.ID, ":")
		if !ok {
			continue
		}
		var n int
		if _, err := fmt.Sscan(seq, &n); err != nil || inputOf[n] == 0 {
			continue
		}
		out[[2]string{fmt.Sprint(inputOf[n]), ref}] = injected{input: inputOf[n], ref: ref, lineage: l}
	}
	return out
}

// compareRecords follows each injected record through both runs. It runs
// only when the inputs are proven identical, so the same input and position
// name the same record in both runs.
func (c *Comparison) compareRecords(before, after *side, rules []*rule) {
	if before.report.Result.Truncation != nil || after.report.Result.Truncation != nil {
		c.Notes = append(c.Notes, "a run did not keep all of its evidence: injected records are not compared one by one")
		return
	}
	b, a := before.injectedRecords(), after.injectedRecords()
	out := &Records{Items: []Record{}}
	// Change groups by exporter and signal, then by (path, op).
	groups := map[[2]string]map[[2]string]*evidence.ChangeGroup{}
	keys := map[[2]string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	sorted := slices.SortedFunc(func(yield func([2]string) bool) {
		for k := range keys {
			if !yield(k) {
				return
			}
		}
	}, func(x, y [2]string) int {
		if c := len(x[0]) - len(y[0]); c != 0 {
			return c
		}
		if c := strings.Compare(x[0], y[0]); c != 0 {
			return c
		}
		return compareRefs(x[1], y[1])
	})
	for _, k := range sorted {
		bi, bok := b[k]
		ai, aok := a[k]
		if !bok || !aok {
			out.Unaligned++
			continue
		}
		out.Aligned++
		bf, bsure := before.fate(bi.lineage)
		af, asure := after.fate(ai.lineage)
		if !bsure || !asure {
			out.Uncertain++
			continue
		}
		r := Record{Input: bi.input, Ref: bi.ref, Signal: bi.lineage.Signal, Name: bi.lineage.Name, Before: bf, After: af,
			FateChanged: !slices.Equal(bf.Delivered, af.Delivered) || !slices.Equal(bf.Stopped, af.Stopped), DivergesAt: divergence(bf.Path, af.Path)}
		if confidence(bi.lineage) == analysis.ConfidenceMedium || confidence(ai.lineage) == analysis.ConfidenceMedium {
			r.Confidence = analysis.ConfidenceMedium
		}
		changes := deliveredChanges(before, after, bi.lineage, ai.lineage, rules)
		if !r.FateChanged && len(changes) == 0 {
			continue
		}
		out.Changed++
		for _, ch := range changes {
			ek := [2]string{ch.exporter, r.Signal}
			if groups[ek] == nil {
				groups[ek] = map[[2]string]*evidence.ChangeGroup{}
			}
			gk := [2]string{ch.Path, string(ch.Op)}
			g := groups[ek][gk]
			if g == nil {
				g = &evidence.ChangeGroup{Path: ch.Path, Op: ch.Op, Before: ch.Before, After: ch.After}
				groups[ek][gk] = g
			}
			g.Count++
			if r.Confidence == analysis.ConfidenceMedium {
				g.Confidence = analysis.ConfidenceMedium
			}
		}
		if len(changes) > maxRecordChanges {
			changes, r.ChangesTruncated = changes[:maxRecordChanges], true
		}
		r.Changes = changes
		if len(out.Items) < maxRecords {
			out.Items = append(out.Items, r)
		} else {
			out.Truncated = true
		}
	}
	c.Records = out
	for i := range c.Exporters {
		e := &c.Exporters[i]
		gs := groups[[2]string{e.Exporter, e.Signal}]
		if len(gs) == 0 {
			continue
		}
		// Followed records replace the content pairing of compareExporters.
		e.Changes, e.ChangesTruncated = sortedGroups(gs)
		if e.Note == pairedNote {
			e.Note = ""
		}
	}
}

// sortedGroups lists change groups by descending count, then path and op,
// and says whether it kept only the first maxGroups.
func sortedGroups(gs map[[2]string]*evidence.ChangeGroup) ([]evidence.ChangeGroup, bool) {
	list := make([]evidence.ChangeGroup, 0, len(gs))
	for _, g := range gs {
		list = append(list, *g)
	}
	slices.SortFunc(list, func(x, y evidence.ChangeGroup) int {
		if x.Count != y.Count {
			return y.Count - x.Count
		}
		return strings.Compare(x.Path+"\x00"+string(x.Op), y.Path+"\x00"+string(y.Op))
	})
	if len(list) > maxGroups {
		return list[:maxGroups], true
	}
	return list, false
}

// recordChanges diffs two records with their resources and scopes by OTTL
// path, leaving out the differences an ignore rule matches.
func recordChanges(bp *telemetry.Payload, br telemetry.Record, ap *telemetry.Payload, ar telemetry.Record, rules []*rule) []FieldChange {
	var changes []analysis.Change
	bres, _ := bp.ResourceAt(br.Resource)
	ares, _ := ap.ResourceAt(ar.Resource)
	changes = append(changes, analysis.DiffResource("resource", bres, ares)...)
	bscope, _ := bp.ScopeAt(br.Resource, br.Scope)
	ascope, _ := ap.ScopeAt(ar.Resource, ar.Scope)
	changes = append(changes, analysis.DiffScope("scope", bscope, ascope)...)
	changes = append(changes, analysis.DiffFields("record", br.Fields, ar.Fields)...)
	var out []FieldChange
	for _, ch := range changes {
		path := changePath(bp, ch)
		if matchRules(rules, path) != nil {
			continue
		}
		out = append(out, FieldChange{Path: path, Op: ch.Op,
			Before: evidence.SummarizeValue(ch.Before, ch.BeforeType), After: evidence.SummarizeValue(ch.After, ch.AfterType)})
	}
	return out
}

// fate says where a record ended, and whether every path of it was followed
// with confidence.
func (s *side) fate(l *analysis.RecordLineage) (Fate, bool) {
	f := Fate{Delivered: []string{}, Stopped: []string{}, LineageID: l.ID, Path: path(l)}
	for _, d := range l.Deliveries {
		f.Delivered = append(f.Delivered, where(d.Exporter, s.deliveryPipeline(d, l.Signal)))
	}
	sure := true
	for _, s := range l.Stops {
		if s.End == analysis.EndUnresolved {
			sure = false
		}
		f.Stopped = append(f.Stopped, string(s.End)+" at "+where(s.Component, s.Pipeline))
	}
	slices.Sort(f.Delivered)
	slices.Sort(f.Stopped)
	return f, sure
}

// path lists the components a record's hops pass through, depth first. A
// fan-out's branches follow in component and pipeline order, since their hop
// order is the order concurrent calls happened to be recorded in. A stage's
// input and output hops are one step, carrying the output's changes; the end
// is the last hop's.
func path(l *analysis.RecordLineage) []Step {
	children := map[int][]int{}
	for i, h := range l.Hops {
		children[h.Parent] = append(children[h.Parent], i)
	}
	for _, c := range children {
		slices.SortStableFunc(c, func(x, y int) int {
			a, b := l.Hops[x], l.Hops[y]
			return strings.Compare(a.Component+"\x00"+a.Pipeline, b.Component+"\x00"+b.Pipeline)
		})
	}
	steps := []Step{}
	at := map[[2]string]int{}
	var visit func(int)
	visit = func(i int) {
		h := l.Hops[i]
		if h.Component != "" {
			key := [2]string{h.Component, h.Pipeline}
			n, ok := at[key]
			if !ok {
				n = len(steps)
				at[key] = n
				steps = append(steps, Step{Component: h.Component, Pipeline: h.Pipeline})
			}
			steps[n].Changes += len(h.Changes)
			if h.End != "" {
				steps[n].End = h.End
			}
		}
		for _, c := range children[i] {
			visit(c)
		}
	}
	for _, root := range children[-1] {
		visit(root)
	}
	return steps
}

// divergence names the first step where two paths of the same record differ:
// in the before path's order, a step the after path lacks or has with other
// changes or end, else the first step only the after path has.
func divergence(before, after []Step) string {
	key := func(s Step) [2]string { return [2]string{s.Component, s.Pipeline} }
	index := func(steps []Step) map[[2]string]Step {
		out := map[[2]string]Step{}
		for _, s := range steps {
			out[key(s)] = s
		}
		return out
	}
	b, a := index(before), index(after)
	for _, s := range before {
		if t, ok := a[key(s)]; !ok || t != s {
			return where(s.Component, s.Pipeline)
		}
	}
	for _, s := range after {
		if _, ok := b[key(s)]; !ok {
			return where(s.Component, s.Pipeline)
		}
	}
	return ""
}

// deliveryPipeline names the pipeline that delivered a record. Only
// processors record their pipeline, so when a record reached the exporter
// straight from a receiver or connector, the pipeline is the one pipeline of
// that signal the exporter belongs to; with several it stays unknown.
func (s *side) deliveryPipeline(d analysis.Delivery, signal string) string {
	if d.Pipeline != "" || s.report == nil || s.report.Topology == nil {
		return d.Pipeline
	}
	found := ""
	for _, p := range s.report.Topology.Pipelines {
		if p.Signal == signal && slices.Contains(p.Exporters, "exporter/"+d.Exporter) {
			if found != "" {
				return ""
			}
			found = p.ID
		}
	}
	return found
}

func where(component, pipeline string) string {
	if pipeline == "" {
		return component
	}
	return component + " (" + pipeline + ")"
}

// confidence is the lowest content-match confidence on the record's path.
func confidence(l *analysis.RecordLineage) analysis.Confidence {
	for _, h := range l.Hops {
		if h.Confidence == analysis.ConfidenceMedium {
			return analysis.ConfidenceMedium
		}
	}
	return ""
}

// deliveredChanges diffs what each exporter both runs delivered the record
// to received, by OTTL path. A delivery reached in one run only is a fate
// change, not a field change. Differences an ignore rule matches are left
// out; they are counted at the exporters.
func deliveredChanges(before, after *side, b, a *analysis.RecordLineage, rules []*rule) []FieldChange {
	var out []FieldChange
	used := map[int]bool{}
	for _, bd := range b.Deliveries {
		pipeline := before.deliveryPipeline(bd, b.Signal)
		for ai, ad := range a.Deliveries {
			if used[ai] || ad.Exporter != bd.Exporter || after.deliveryPipeline(ad, a.Signal) != pipeline {
				continue
			}
			used[ai] = true
			bp, br, bok := delivered(before, b, bd)
			ap, ar, aok := delivered(after, a, ad)
			if !bok || !aok {
				break
			}
			exporter := where(bd.Exporter, pipeline)
			for _, ch := range recordChanges(bp, br, ap, ar, rules) {
				ch.Exporter, ch.exporter = exporter, bd.Exporter
				out = append(out, ch)
			}
			break
		}
	}
	return out
}

// changePath names a change by its OTTL path.
func changePath(p *telemetry.Payload, ch analysis.Change) string {
	switch ch.Entity {
	case "resource":
		return "resource." + ch.Path
	case "scope":
		return "instrumentation_scope." + ch.Path
	}
	field, _, _ := strings.Cut(ch.Path, "[")
	rest := strings.TrimPrefix(ch.Path, field)
	return recordPath(p.Signal, field, ch.AfterType+ch.BeforeType, true) + rest
}

// delivered finds the record a delivery hop holds.
func delivered(s *side, l *analysis.RecordLineage, d analysis.Delivery) (*telemetry.Payload, telemetry.Record, bool) {
	if d.Hop < 0 || d.Hop >= len(l.Hops) {
		return nil, telemetry.Record{}, false
	}
	h := l.Hops[d.Hop]
	p := s.snapshot(h.Seq)
	if p == nil {
		return nil, telemetry.Record{}, false
	}
	if s.records == nil {
		s.records = map[*telemetry.Payload]map[string]telemetry.Record{}
	}
	byRef, ok := s.records[p]
	if !ok {
		byRef = map[string]telemetry.Record{}
		for _, r := range telemetry.Records(*p) {
			// The first record at a position wins, as the scan did.
			ref := r.Ref(p.Signal)
			if _, dup := byRef[ref]; !dup {
				byRef[ref] = r
			}
		}
		s.records[p] = byRef
	}
	r, ok := byRef[h.Ref]
	if !ok {
		return nil, telemetry.Record{}, false
	}
	return p, r, true
}

// compareRefs orders positions like "resource[0].scope[1].log_record[10]"
// numerically.
func compareRefs(x, y string) int {
	nums := func(s string) []int {
		var out []int
		for part := range strings.SplitSeq(s, "[") {
			var n int
			if _, err := fmt.Sscanf(part, "%d]", &n); err == nil {
				out = append(out, n)
			}
		}
		return out
	}
	if c := slices.Compare(nums(x), nums(y)); c != 0 {
		return c
	}
	return strings.Compare(x, y)
}
