package comparison

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/evidence"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

type engineReport = engine.Report

// side reads one run's recorded telemetry as comparable payloads: the rebase
// shift undone and, for runs with mocks, mocked values normalized, as
// Report.Normalize does. The report itself is never changed; every payload
// is decoded afresh.
type side struct {
	report   *engine.Report
	shift    time.Duration
	mocked   bool
	bySeq    map[int]instrument.Event
	payloads map[string]*telemetry.Payload
}

func newSide(s Side) *side {
	r := s.Run.Report
	if r == nil || r.Result == nil {
		return &side{}
	}
	out := &side{report: r, shift: r.Timestamps.RebaseShift(), mocked: len(r.Result.Mocks) > 0,
		bySeq: map[int]instrument.Event{}, payloads: map[string]*telemetry.Payload{}}
	for _, e := range r.Result.Events {
		out.bySeq[e.Seq] = e
	}
	return out
}

// normalize prepares a freshly decoded payload for comparison.
func (s *side) normalize(p telemetry.Payload) telemetry.Payload {
	if s.shift != 0 {
		p.Shift(-s.shift)
	}
	if s.mocked {
		runresult.NormalizeMocked(p)
	}
	return p
}

// snapshot returns the event's recorded payload, or nil when the run did not
// keep it.
func (s *side) snapshot(seq int) *telemetry.Payload {
	e, ok := s.bySeq[seq]
	if !ok || e.Snapshot == "" {
		return nil
	}
	if p, ok := s.payloads[e.Snapshot]; ok {
		return p
	}
	var out *telemetry.Payload
	if raw, ok := s.report.Result.Snapshots[e.Snapshot]; ok {
		if p, err := telemetry.Decode(raw); err == nil {
			p = s.normalize(p)
			out = &p
		}
	}
	s.payloads[e.Snapshot] = out
	return out
}

// compareExporters compares each exporter's received records as multisets,
// as runresult.Compare does for the reference run: by content with resource
// and scope, in any order or grouping. Completely empty outputs (no
// resources) are not output.
func (c *Comparison) compareExporters(before, after *side, rules []*rule) {
	type output struct {
		counts  int
		records []fingerprint
	}
	outputs := func(s *side) map[[2]string]output {
		out := map[[2]string]output{}
		for _, o := range s.report.Result.Outputs {
			p, err := telemetry.Decode(o.Data)
			if err != nil || p.Counts().Resources == 0 {
				continue
			}
			p = s.normalize(p)
			out[[2]string{o.Exporter, o.Signal}] = output{counts: o.Counts.Items, records: fingerprints(p, rules)}
		}
		return out
	}
	b, a := outputs(before), outputs(after)
	keys := map[[2]string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	for k := range keys {
		bo, bok := b[k]
		ao, aok := a[k]
		e := Exporter{Exporter: k[0], Signal: k[1], Before: bo.counts, After: ao.counts, Changes: []evidence.ChangeGroup{}}
		switch {
		case !bok:
			e.OnlyIn = "after"
		case !aok:
			e.OnlyIn = "before"
		default:
			onlyB, onlyA := difference(bo.records, ao.records)
			switch {
			case len(onlyB) == 0 && len(onlyA) == 0:
				e.Equal = true
				if !slices.EqualFunc(bo.records, ao.records, func(x, y fingerprint) bool { return x.raw == y.raw }) {
					e.Note = "the same records, in a different order or grouping"
				}
			case countIgnored(onlyB, onlyA):
				e.EqualAfterIgnore = true
				e.Note = "the records differ only in ignored paths"
			case c.InputsIdentical:
				e.Changes, e.ChangesTruncated = unmatchedChanges(onlyB, onlyA, rules)
				if len(e.Changes) > 0 {
					e.Note = pairedNote
				}
			}
		}
		c.Exporters = append(c.Exporters, e)
	}
	slices.SortFunc(c.Exporters, func(x, y Exporter) int {
		return strings.Compare(x.Exporter+"\x00"+x.Signal, y.Exporter+"\x00"+y.Signal)
	})
}

// fingerprint is one record with its resource and scope: its leaves, the
// leaves joined (raw) and the leaves no ignore rule covers joined.
type fingerprint struct {
	leaves       []string
	raw, ignored string
	rules        []*rule // The rule covering each leaf, nil when none.
	payload      *telemetry.Payload
	record       telemetry.Record
	pos          int // The record's position in its exporter's output.
}

// fingerprints returns each output record's fingerprint in output order.
func fingerprints(p telemetry.Payload, rules []*rule) []fingerprint {
	var out []fingerprint
	for i, r := range telemetry.Records(p) {
		f := fingerprint{leaves: recordLeaves(p, r), payload: &p, record: r, pos: i}
		f.raw = strings.Join(f.leaves, "\x00")
		var kept []string
		for _, l := range f.leaves {
			path, _, _ := strings.Cut(l, "=")
			m := matchRules(rules, path)
			f.rules = append(f.rules, m)
			if m == nil {
				kept = append(kept, l)
			}
		}
		f.ignored = strings.Join(kept, "\x00")
		out = append(out, f)
	}
	return out
}

// difference returns the records of each multiset the other lacks, sorted.
func difference(b, a []fingerprint) (onlyB, onlyA []fingerprint) {
	left := map[string]int{}
	for _, f := range a {
		left[f.raw]++
	}
	for _, f := range b {
		if left[f.raw] > 0 {
			left[f.raw]--
			continue
		}
		onlyB = append(onlyB, f)
	}
	right := map[string]int{}
	for _, f := range b {
		right[f.raw]++
	}
	for _, f := range a {
		if right[f.raw] > 0 {
			right[f.raw]--
			continue
		}
		onlyA = append(onlyA, f)
	}
	byRaw := func(x, y fingerprint) int { return strings.Compare(x.raw, y.raw) }
	slices.SortFunc(onlyB, byRaw)
	slices.SortFunc(onlyA, byRaw)
	return onlyB, onlyA
}

// countIgnored pairs the unmatched records that are equal once ignored paths
// are removed, counts each pair's differing paths per rule and reports
// whether every unmatched record paired. Such records are paired in a fixed
// order; which of several pairs with which does not change the outcome.
func countIgnored(onlyB, onlyA []fingerprint) bool {
	groups := map[string][]fingerprint{}
	for _, f := range onlyA {
		groups[f.ignored] = append(groups[f.ignored], f)
	}
	type pair struct{ b, a fingerprint }
	var pairs []pair
	for _, f := range onlyB {
		g := groups[f.ignored]
		if len(g) == 0 {
			continue
		}
		pairs = append(pairs, pair{f, g[0]})
		groups[f.ignored] = g[1:]
	}
	for _, p := range pairs {
		counted := map[string]bool{}
		count := func(f, other fingerprint) {
			for i, l := range f.leaves {
				path, _, _ := strings.Cut(l, "=")
				if f.rules[i] == nil || counted[path] || slices.Contains(other.leaves, l) {
					continue
				}
				counted[path] = true
				f.rules[i].count++
			}
		}
		count(p.b, p.a)
		count(p.a, p.b)
	}
	return len(pairs) == len(onlyB) && len(pairs) == len(onlyA)
}

// maxPairings bounds the content pairing of unmatched records (before
// times after); larger sets pair in output order.
const maxPairings = 250_000

// pairedNote marks changes from the content pairing.
const pairedNote = "field changes pair the differing records by shared content"

// unmatchedChanges groups by (path, op) the field differences of the records
// only one run with identical inputs delivered to an exporter. Records
// without a lineage to follow, such as those a receiver produced from a
// mock, are only comparable this way. Each before record pairs with the unpaired after record sharing the
// most leaves, the earliest on a tie; records left over were added or
// removed, which the counts show. The pairing is a best match, so every
// group has medium confidence. The per-record comparison replaces these
// groups with its own when it follows records to the exporter.
func unmatchedChanges(onlyB, onlyA []fingerprint, rules []*rule) ([]evidence.ChangeGroup, bool) {
	byPos := func(x, y fingerprint) int { return x.pos - y.pos }
	onlyB, onlyA = slices.SortedFunc(slices.Values(onlyB), byPos), slices.SortedFunc(slices.Values(onlyA), byPos)
	type pair struct{ b, a fingerprint }
	var pairs []pair
	if len(onlyB)*len(onlyA) > maxPairings {
		for i := range min(len(onlyB), len(onlyA)) {
			pairs = append(pairs, pair{onlyB[i], onlyA[i]})
		}
	} else {
		sets := make([]map[string]bool, len(onlyA))
		for i, f := range onlyA {
			sets[i] = map[string]bool{}
			for _, l := range f.leaves {
				sets[i][l] = true
			}
		}
		used := make([]bool, len(onlyA))
		for _, b := range onlyB {
			best, shared := -1, -1
			for i := range onlyA {
				if used[i] {
					continue
				}
				n := 0
				for _, l := range b.leaves {
					if sets[i][l] {
						n++
					}
				}
				if n > shared {
					best, shared = i, n
				}
			}
			if best < 0 {
				break
			}
			used[best] = true
			pairs = append(pairs, pair{b, onlyA[best]})
		}
	}
	groups := map[[2]string]*evidence.ChangeGroup{}
	for _, p := range pairs {
		for _, ch := range recordChanges(p.b.payload, p.b.record, p.a.payload, p.a.record, rules) {
			k := [2]string{ch.Path, string(ch.Op)}
			g := groups[k]
			if g == nil {
				g = &evidence.ChangeGroup{Path: ch.Path, Op: ch.Op, Before: ch.Before, After: ch.After, Confidence: analysis.ConfidenceMedium}
				groups[k] = g
			}
			g.Count++
		}
	}
	return sortedGroups(groups)
}

// recordLeaves flattens a record with its resource and scope into sorted
// "path=type:value" leaves, named by OTTL paths.
func recordLeaves(p telemetry.Payload, r telemetry.Record) []string {
	var out []string
	res, resURL := p.ResourceAt(r.Resource)
	scope, scopeURL := p.ScopeAt(r.Resource, r.Scope)
	out = append(out, "resource.schema_url=str:"+strconv.Quote(resURL))
	out = appendLeaves(out, "resource.attributes", res.Attributes())
	out = append(out, "instrumentation_scope.schema_url=str:"+strconv.Quote(scopeURL),
		"instrumentation_scope.name=str:"+strconv.Quote(scope.Name()),
		"instrumentation_scope.version=str:"+strconv.Quote(scope.Version()))
	out = appendLeaves(out, "instrumentation_scope.attributes", scope.Attributes())
	for k, v := range r.Fields.All() {
		path := recordPath(p.Signal, k, v.Type().String(), r.Metric >= 0)
		if v.Type() == pcommon.ValueTypeMap {
			out = appendLeaves(out, path, v.Map())
			continue
		}
		out = append(out, leaf(path, v))
	}
	slices.Sort(out)
	return out
}

func appendLeaves(out []string, path string, m pcommon.Map) []string {
	for k, v := range m.All() {
		p := path + "[" + strconv.Quote(k) + "]"
		if v.Type() == pcommon.ValueTypeMap {
			out = appendLeaves(out, p, v.Map())
			continue
		}
		out = append(out, leaf(p, v))
	}
	return out
}

func leaf(path string, v pcommon.Value) string {
	return fmt.Sprintf("%s=%s:%s", path, v.Type(), strconv.Quote(v.AsString()))
}

// recordPath names a record field (a telemetry.Records field name, possibly
// followed by map keys) by its OTTL path, as the web client's ottlPath does.
func recordPath(signal pipeline.Signal, field, valueType string, point bool) string {
	switch signal {
	case pipeline.SignalTraces:
		return "span." + field
	case pipeline.SignalLogs:
		return "log." + field
	}
	switch {
	case strings.HasPrefix(field, "metric."):
		return field
	case field == "value":
		if strings.HasPrefix(strings.ToLower(valueType), "int") {
			return "datapoint.value_int"
		}
		return "datapoint.value_double"
	case point:
		return "datapoint." + field
	}
	return "metric." + field
}
