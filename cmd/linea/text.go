package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// writeText prints a run's report: the verdict, warnings, inputs,
// outputs and assertions first, then what each stage did, the analyses and
// recommendations, and the event log last.
func writeText(w io.Writer, rep *platform.Report) error {
	t := newTextOut(w)
	printf := t.printf
	r := rep.Result

	verdict, detail := runVerdict(rep)
	if detail != "" {
		detail = ": " + detail
	}
	t.line(0, "%s  run %s%s", t.status(verdict, 0), r.RunID, detail)
	t.field(1, "scenario", rep.Scenario)
	if p := rep.Provenance; p != nil {
		t.field(1, "collector", p.CollectorVersion)
		t.field(1, "debugger", p.ProductVersion)
		t.field(1, "config sha256", fmt.Sprintf("%.12s", p.Config.SHA256))
	}

	if warnings := runWarnings(rep); len(warnings) > 0 {
		t.heading("warnings")
		for _, w := range warnings {
			t.line(1, "%s", w)
		}
	}

	t.heading("inputs")
	for i, in := range r.Inputs {
		status := "accepted"
		switch {
		case !in.Injected:
			status = "not injected"
		case in.Error != "":
			status = "rejected: " + in.Error
		}
		source := ""
		if in.Mock != "" {
			source = " (read from the " + in.Mock + " mock)"
		}
		if in.ClientAddress != "" {
			source += " from client " + in.ClientAddress
		}
		if in.After != "" {
			source += " after " + in.After
		}
		t.line(1, "%d  %s at receiver %s%s, %s, %s", i+1, in.Signal, in.Receiver, source, plural(in.Counts.Items, "item"), status)
	}
	if len(r.Inputs) == 0 {
		t.line(1, "none")
	}
	if s := r.Settle; s != nil {
		t.field(1, "settled", fmt.Sprintf("%s after the last input (%s)", s.Duration, s.Reason))
	}
	if ts := rep.Timestamps; ts != nil && ts.Mode == platform.TimestampsRebased {
		if ts.Offset != "" {
			t.field(1, "timestamps", "rebased by "+ts.Offset+" to the run's start")
		}
		for _, c := range ts.WallClock {
			t.line(1, "%s reads the wall clock: `%s`", c.Component, c.Expression)
		}
	}

	if len(r.Mocks) > 0 {
		t.heading("mocks")
	}
	for _, d := range r.Mocks {
		t.line(1, "%s (%s, %s): %d fixture(s) supplied", d.Ref, d.Provider, d.Mode, len(d.Supplied))
		for _, s := range d.Supplied {
			t.field(2, "supplied", s)
		}
		for _, l := range d.Limitations {
			t.field(2, "limitation", l)
		}
	}

	if len(r.Outputs) > 0 || len(r.EndpointOutputs) > 0 || rep.Comparison != nil {
		t.heading("outputs")
		var rows [][]string
		for _, o := range r.Outputs {
			rows = append(rows, []string{"exporter " + o.Exporter, string(o.Signal), plural(o.Batches, "batch"), plural(o.Counts.Items, "item")})
		}
		for _, o := range r.EndpointOutputs {
			rows = append(rows, []string{"endpoint " + o.Component + " " + o.Endpoint, string(o.Signal), plural(o.Batches, "batch"), plural(o.Counts.Items, "item")})
		}
		t.table(1, nil, rows)
		if c := rep.Comparison; c != nil {
			switch {
			case c.Incomplete != "":
				t.line(1, "reference comparison: %s (%s)", t.status("incomplete", 0), c.Incomplete)
			case c.Equal:
				t.line(1, "reference comparison: %s", t.status("equal", 0))
			default:
				t.line(1, "reference comparison: %s", t.status("different", 0))
				for _, d := range c.Differences {
					t.line(2, "- %s", d)
				}
			}
			if c.Incomplete != "" || c.Equal {
				for _, n := range c.Notes {
					t.field(2, "note", n)
				}
			}
		}
	}

	if a := rep.Assertions; a != nil {
		t.heading("assertions")
		t.line(1, "overall: %s (run exits 0 on assertions; the test command sets exit codes)", t.status(string(a.Outcome), 0))
		writeAssertionResults(t, 1, a.Results, false)
	}

	if len(rep.Stages) > 0 {
		t.heading("stages")
	}
	for _, s := range rep.Stages {
		where := "in " + s.Pipeline
		if s.Pipeline == "" {
			where = s.Kind // a connector joins pipelines rather than running in one
		}
		t.line(1, "%s %s (%s)", s.Component, where, s.Emission)
		for _, in := range s.Inputs {
			printf("    input seq %d: %s", in.Seq, in.Outcome)
			if in.Error != "" {
				printf(", error: %s", in.Error)
			}
			if d := in.Diff; d != nil {
				printf(", %s, %d removed, %d added", plural(len(d.Changes), "change"), len(d.Removed), len(d.Added))
				if m := d.Matching; m != nil {
					if matched := matchingLine(m.Object, m.Exact, m.High, m.Medium); matched != "" {
						printf(" (matched: %s)", matched)
					}
				}
				for _, rm := range d.Removed {
					printf("\n      removed %s", recordLabel(rm))
				}
				for _, u := range d.Unresolved {
					printf("\n      unresolved: %s", u)
				}
			}
			printf("\n")
		}
		if rc := s.Reconciliation; rc != nil {
			t.line(2, "reconciliation over %d emission(s): %d removed, %d added, %s",
				len(s.Emissions), len(rc.Removed), len(rc.Added), plural(len(rc.Changes), "change"))
		}
	}

	if len(rep.OTTL) > 0 {
		t.heading("ottl replay")
	}
	for _, o := range rep.OTTL {
		printf("  %s input seq %d: %s\n", o.Processor, o.Input, o.Verification)
		for _, g := range o.Groups {
			printf("    group %d (%s, %s): %s\n", g.Index, g.Context, g.ErrorMode, g.Status)
			for _, st := range g.Statements {
				printf("      [%d] matched %d, not matched %d, %d change(s), %d error(s): %s\n",
					st.Index, st.Matched, st.NotMatched, len(st.Changes), len(st.Errors), st.Text)
			}
		}
	}

	if len(rep.Aggregations) > 0 {
		t.heading("aggregations")
	}
	for _, a := range rep.Aggregations {
		printf("  %s input seq %d: %d data point(s), %s\n", a.Connector, a.Input, len(a.Points), a.Verification)
		for _, p := range a.Points {
			printf("    %s = %d", p.Metric, p.Value)
			keys := slices.Sorted(maps.Keys(p.Attributes))
			for _, k := range keys {
				printf(" %s=%s", k, p.Attributes[k])
			}
			if len(p.Records) > 0 {
				printf(" <- %s", strings.Join(p.Records, ", "))
			}
			printf("\n")
		}
		if len(a.Uncounted) > 0 {
			printf("    uncounted: %s\n", strings.Join(a.Uncounted, ", "))
		}
		for _, n := range a.Notes {
			printf("    note: %s\n", n)
		}
	}

	if len(rep.Routing) > 0 {
		t.heading("routing")
	}
	for _, r := range rep.Routing {
		printf("  %s input seq %d: %d rule(s), %s\n", r.Connector, r.Input, len(r.Rules), r.Verification)
		for _, rule := range r.Rules {
			printf("    rule %d (%s): %s -> %s\n", rule.Index, rule.Action, rule.Condition, strings.Join(rule.Pipelines, ", "))
		}
		if len(r.DefaultPipelines) > 0 {
			printf("    default -> %s\n", strings.Join(r.DefaultPipelines, ", "))
		}
		for _, rec := range r.Records {
			var to []string
			for _, i := range rec.Rules {
				to = append(to, fmt.Sprintf("rule %d", i))
			}
			if rec.Default {
				to = append(to, "default")
			}
			if len(to) == 0 {
				to = append(to, "no pipeline")
			}
			printf("    %s -> %s\n", rec.Ref, strings.Join(to, ", "))
		}
		for _, n := range r.Notes {
			printf("    note: %s\n", n)
		}
	}

	if len(rep.Sampling) > 0 {
		t.heading("sampling decisions")
	}
	for _, s := range rep.Sampling {
		printf("  %s (pipeline %s, decision_wait %s)\n", s.Processor, s.Pipeline, s.DecisionWait)
		for _, p := range s.Policies {
			printf("    policy %s (%s): %s\n", p.Name, p.Type, p.Rule)
		}
		for _, t := range s.Traces {
			printf("    trace %s %q: %s", t.TraceID, t.Name, t.Decision)
			if t.Policy != "" {
				printf(" by %s", t.Policy)
			}
			printf(", %d span(s) over %s", t.Spans, t.Duration)
			if t.LateSpans > 0 {
				printf(", %d late span(s) %s", t.LateSpans, t.Late)
			}
			if t.Verification != "" {
				printf(", %s", t.Verification)
			}
			printf("\n")
			for _, p := range t.Policies {
				printf("      %s: %s", p.Policy, p.Result)
				if p.Evidence != "" {
					printf(" (%s)", p.Evidence)
				}
				printf("\n")
			}
		}
		for _, n := range s.Notes {
			printf("    note: %s\n", n)
		}
	}

	if len(rep.Batching) > 0 {
		t.heading("batching")
	}
	for _, b := range rep.Batching {
		writeBatching(printf, b)
	}
	if len(rep.Grouping) > 0 {
		t.heading("grouping")
	}
	for _, g := range rep.Grouping {
		writeGrouping(printf, g)
	}
	if len(rep.Filtering) > 0 {
		t.heading("filtering")
	}
	for _, f := range rep.Filtering {
		writeFiltering(printf, f)
	}
	if len(rep.Recommendations) > 0 {
		t.heading("recommendations")
		writeRecommendations(printf, rep.Recommendations)
	}

	if len(r.Events) > 0 {
		t.heading("events")
		rows := make([][]string, 0, len(r.Events))
		for _, e := range r.Events {
			from, items := "", ""
			if e.InputSeq != 0 {
				from = fmt.Sprint(e.InputSeq)
			}
			if e.Counts != nil {
				items = fmt.Sprint(e.Counts.Items)
			}
			var notes []string
			if e.Error != "" {
				notes = append(notes, "error: "+e.Error)
			}
			if e.Log != nil {
				notes = append(notes, fmt.Sprintf("%s: %s", e.Log.Level, oneLine(e.Log.Message)))
			}
			if e.Endpoint != "" {
				notes = append(notes, "endpoint "+e.Endpoint)
			}
			rows = append(rows, []string{fmt.Sprint(e.Seq), string(e.Kind), e.Component, e.Pipeline, from, items, strings.Join(notes, "; ")})
		}
		t.table(1, []string{"seq", "kind", "component", "pipeline", "from", "items", "detail"}, rows)
	}
	return t.err()
}

// runVerdict returns the run's verdict word and what follows it on the
// first line. The verdict is the assertions' outcome when the scenario
// declares any, otherwise the run's status, or "different" when the
// reference comparison differs.
func runVerdict(rep *platform.Report) (string, string) {
	r := rep.Result
	verdict := string(r.Status)
	var parts []string
	if a := rep.Assertions; a != nil {
		verdict = string(a.Outcome)
		passed := 0
		for _, res := range a.Results {
			if res.Outcome == assertion.Pass {
				passed++
			}
		}
		parts = append(parts, string(r.Status), fmt.Sprintf("%d of %d assertions passed", passed, len(a.Results)))
	}
	if c := rep.Comparison; c != nil && c.Incomplete == "" && !c.Equal {
		if rep.Assertions == nil {
			verdict = "different"
			parts = append(parts, string(r.Status))
		}
		parts = append(parts, "reference comparison different")
	}
	detail := strings.Join(parts, ", ")
	switch {
	case r.StatusReason == "":
	case detail == "":
		detail = r.StatusReason
	default:
		detail += " (" + r.StatusReason + ")"
	}
	return verdict, detail
}

// runWarnings collects what makes a run differ from the deployed Collector:
// stand-ins, unavailable references, wall clock reads that were not
// rebased, and mock warnings.
func runWarnings(rep *platform.Report) []string {
	var out []string
	if g := rep.Topology; g != nil {
		for _, c := range g.Components {
			if c.Mode == platform.ModeStandIn {
				out = append(out, fmt.Sprintf("%s ran as a stand-in: %s", c.Ref, c.Note))
			}
		}
	}
	for _, ref := range rep.References {
		if ref.Source == platform.ReferenceStandIn {
			out = append(out, fmt.Sprintf("${%s:%s} was unavailable (%s); the stand-in %q was used", ref.Scheme, ref.Target, ref.Reason, ref.StandIn))
		}
	}
	if ts := rep.Timestamps; ts != nil && ts.Mode != platform.TimestampsRebased {
		for _, c := range ts.WallClock {
			age := ""
			if ts.Offset != "" {
				age = fmt.Sprintf("; the latest input record is %s old", ts.Offset)
			}
			out = append(out, fmt.Sprintf("%s reads the wall clock: `%s`%s (declare execution.timestamps: rebase)", c.Component, c.Expression, age))
		}
	}
	for _, d := range rep.Result.Mocks {
		for _, w := range d.Warnings {
			out = append(out, fmt.Sprintf("mock %s: %s", d.Ref, w))
		}
	}
	return out
}

// matchingLine names the non-zero record matches of a stage diff.
func matchingLine(object, exact, high, medium int) string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{object, "same record"}, {exact, "exact"}, {high, "high confidence"}, {medium, "medium confidence"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	return strings.Join(parts, ", ")
}

// writeRecommendations prints each recommendation with its kind and
// confidence, the suggested change, what it saves, and what limits it.
func writeRecommendations(printf func(string, ...any), recs []platform.Recommendation) {
	if len(recs) == 0 {
		return
	}
	for _, r := range recs {
		printf("  [%s, %s] %s", r.Kind, r.Confidence, r.Rule)
		if r.Pipeline != "" {
			printf(" (pipeline %s)", r.Pipeline)
		}
		printf(": %s\n", r.Summary)
		if c := r.Change; c != nil {
			printf("      change: %s\n", changeLine(c))
		}
		if b := r.Benefit; b != nil {
			printf("      saves: %d %s\n", b.Count, strings.ReplaceAll(b.Metric, "_", " "))
		}
		for _, b := range r.Blockers {
			printf("      limit: %s\n", b)
		}
		for _, c := range r.Coverage {
			printf("      coverage: %s\n", c)
		}
	}
}

// changeLine describes a suggested change in one line.
func changeLine(c *platform.RecommendationChange) string {
	switch c.Op {
	case platform.ChangeSwap:
		return "processors: [" + strings.Join(c.Processors, ", ") + "]"
	case platform.ChangeRemoveStatement:
		where := ""
		if c.Index != nil {
			where = fmt.Sprintf(" %d", *c.Index)
		}
		if c.Group != nil {
			return fmt.Sprintf("remove statement%s of group %d of %s: %s", where, *c.Group, c.Component, c.Text)
		}
		return fmt.Sprintf("remove condition%s of %s: %s", where, c.Component, c.Text)
	case platform.ChangeSetOption:
		if c.Value == "" {
			return fmt.Sprintf("set %s %s (the value is yours to choose)", c.Component, c.Option)
		}
		return fmt.Sprintf("set %s %s: %s", c.Component, c.Option, c.Value)
	}
	return string(c.Op)
}

// writeBatching prints one batching processor's settings and its sends.
func writeBatching(printf func(string, ...any), b *analysis.Batching) {
	st := b.Settings
	unit := ""
	if st.Bytes() {
		unit = " bytes"
	}
	if st.Unbatched {
		printf("  %s (pipeline %s, %s): no batch section, every request is sent as it is", b.Processor, b.Pipeline, b.Signal)
	} else {
		printf("  %s (pipeline %s, %s): %s %d%s, %s %s, %s %d%s", b.Processor, b.Pipeline, b.Signal,
			st.SizeName(), st.SendBatchSize, unit, st.TimeoutName(), st.Timeout, st.MaxSizeName(), st.SendBatchMaxSize, unit)
	}
	if len(st.MetadataKeys) > 0 {
		printf(", %s %s", st.KeysName(), strings.Join(st.MetadataKeys, ", "))
	}
	printf("\n    %d request(s) in, %d batch(es) out, %s\n", len(b.Arrivals), len(b.Flushes), b.Verification)
	for _, f := range b.Flushes {
		printf("    send seq %d: %d item(s), %s", f.Seq, f.Items, f.Trigger)
		if f.Split {
			printf(", split")
		}
		if len(b.Batchers) > 0 {
			printf(", batcher %d", f.Batcher+1)
		}
		printf("\n")
	}
	for _, n := range b.Notes {
		printf("    note: %s\n", n)
	}
}

// filterDropLines bounds the drops printed per filter input; the report
// holds them all.
const filterDropLines = 20

// writeFiltering prints what one filter processor input did: each
// condition's counts, and which condition removed each item when the
// replay reproduces the processor's output.
func writeFiltering(printf func(string, ...any), f *analysis.Filtering) {
	printf("  %s (pipeline %s, %s): input seq %d", f.Processor, f.Pipeline, f.Signal, f.Input)
	if f.Output != nil {
		printf(" -> seq %d", *f.Output)
	} else {
		printf(", nothing emitted")
	}
	printf(": %d record(s) in, %d dropped, %s\n", f.Records, f.Dropped, f.Verification)
	for _, c := range f.Conditions {
		printf("    [%d] %s (%s, %s): %s\n", c.Index, c.Source, c.Context, c.ErrorMode, c.Condition)
		if f.Verification == analysis.FilterUnsupported {
			continue
		}
		printf("        evaluated %d, matched %d (%d record(s)), %d error(s)", c.Evaluated, c.Matched, c.Records, c.Errors)
		if c.Error != "" {
			printf(": %s", c.Error)
		}
		printf("\n")
	}
	for i, d := range f.Drops {
		if i == filterDropLines {
			printf("    ... %d more drop(s) in the report\n", len(f.Drops)-i)
			break
		}
		printf("    %s dropped by [%d]", d.Ref, d.Condition)
		if d.Records > 1 {
			printf(" with %d record(s)", d.Records)
		}
		var names []string
		for _, r := range d.Removed {
			if len(names) == 3 {
				names = append(names, fmt.Sprintf("+%d more", len(d.Removed)-3))
				break
			}
			names = append(names, fmt.Sprintf("%q", r.Name))
		}
		if len(names) > 0 {
			printf(": %s", strings.Join(names, ", "))
		}
		printf("\n")
	}
	if f.Error != "" {
		printf("    error: %s\n", f.Error)
	}
	for _, n := range f.Notes {
		printf("    note: %s\n", n)
	}
}

// writeGrouping prints where one groupbyattrs processor put each record.
// A * marks a resource attribute moved up from the records.
func writeGrouping(printf func(string, ...any), g *analysis.Grouping) {
	printf("  %s (pipeline %s, %s): keys %s\n", g.Processor, g.Pipeline, g.Signal, strings.Join(g.Keys, ", "))
	moved := false
	for _, r := range g.Requests {
		if r.Output == nil {
			printf("    input seq %d: %d resource block(s) in, nothing emitted\n", r.Input, len(r.Before))
			continue
		}
		printf("    input seq %d -> seq %d: %d resource block(s) in, %d out; %d record(s) moved attributes up, %d kept their resource", r.Input, *r.Output, len(r.Before), len(r.After), r.Grouped, r.Kept)
		if r.Unfollowed > 0 {
			printf(", %d not followed", r.Unfollowed)
		}
		printf("\n")
		for i, b := range r.After {
			var attrs, from, records []string
			for _, a := range b.Attributes {
				if a.Moved {
					moved = true
					attrs = append(attrs, a.Key+"="+a.Value+"*")
				} else {
					attrs = append(attrs, a.Key+"="+a.Value)
				}
			}
			for _, f := range b.From {
				from = append(from, fmt.Sprintf("in %d", f.Block+1))
			}
			for _, rec := range b.Records {
				records = append(records, rec.Name)
			}
			printf("      out %d {%s}", i+1, strings.Join(attrs, ", "))
			if len(from) > 1 {
				printf(" merged")
			}
			if len(from) > 0 {
				printf(" from %s", strings.Join(from, ", "))
			}
			if len(records) > 0 {
				printf(": %s", strings.Join(records, ", "))
			}
			printf("\n")
		}
	}
	if moved {
		printf("    * moved up from the records\n")
	}
	for _, n := range g.Notes {
		printf("    note: %s\n", n)
	}
}

// recordLabel names a removed or added record: spans by name and span ID,
// log records and data points by name and position.
// oneLine keeps the first line of a logged message.
func oneLine(s string) string {
	if first, _, cut := strings.Cut(s, "\n"); cut {
		return first + " …"
	}
	return s
}

func recordLabel(r analysis.RecordRef) string {
	if r.SpanID != "" {
		return fmt.Sprintf("span %q (%s)", r.Name, r.SpanID)
	}
	return fmt.Sprintf("record %q (%s)", r.Name, r.Ref)
}
