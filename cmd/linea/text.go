package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func writeText(w io.Writer, rep *platform.Report) error {
	r := rep.Result
	var errs []error
	printf := func(format string, a ...any) {
		_, err := fmt.Fprintf(w, format, a...)
		errs = append(errs, err)
	}
	if rep.Scenario != "" {
		printf("scenario %s\n", rep.Scenario)
	}
	if p := rep.Provenance; p != nil {
		printf("debugger %s, collector %s, config sha256 %.12s\n", p.ProductVersion, p.CollectorVersion, p.Config.SHA256)
	}
	printf("run %s: %s", r.RunID, r.Status)
	if r.StatusReason != "" {
		printf(" (%s)", r.StatusReason)
	}
	printf("\n")
	if g := rep.Topology; g != nil {
		for _, c := range g.Components {
			if c.Mode == platform.ModeStandIn {
				printf("WARNING: %s ran as a stand-in: %s\n", c.Ref, c.Note)
			}
		}
	}
	for _, ref := range rep.References {
		if ref.Source == platform.ReferenceStandIn {
			printf("WARNING: ${%s:%s} was unavailable (%s); the stand-in %q was used\n", ref.Scheme, ref.Target, ref.Reason, ref.StandIn)
		}
	}
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
		printf("input %d: %s at receiver %s%s, %d item(s), %s\n", i+1, in.Signal, in.Receiver, source, in.Counts.Items, status)
	}
	if s := r.Settle; s != nil {
		printf("settled %s after the last input (%s)\n", s.Duration, s.Reason)
	}
	if ts := rep.Timestamps; ts != nil {
		if ts.Mode == platform.TimestampsRebased && ts.Offset != "" {
			printf("input timestamps rebased by %s to the run's start\n", ts.Offset)
		}
		for _, c := range ts.WallClock {
			if ts.Mode == platform.TimestampsRebased {
				printf("%s reads the wall clock: `%s`\n", c.Component, c.Expression)
				continue
			}
			age := ""
			if ts.Offset != "" {
				age = fmt.Sprintf("; the latest input record is %s old", ts.Offset)
			}
			printf("WARNING: %s reads the wall clock: `%s`%s (declare execution.timestamps: rebase)\n", c.Component, c.Expression, age)
		}
	}
	for _, d := range r.Mocks {
		printf("mocked %s (%s, %s): %d fixture(s) supplied\n", d.Ref, d.Provider, d.Mode, len(d.Supplied))
		for _, s := range d.Supplied {
			printf("    supplied: %s\n", s)
		}
		for _, l := range d.Limitations {
			printf("    limitation: %s\n", l)
		}
		for _, w := range d.Warnings {
			printf("    WARNING: %s\n", w)
		}
	}

	printf("\n%4s  %-18s %-20s %-12s %5s %6s\n", "SEQ", "KIND", "COMPONENT", "PIPELINE", "FROM", "ITEMS")
	for _, e := range r.Events {
		from, items := "", ""
		if e.InputSeq != 0 {
			from = fmt.Sprint(e.InputSeq)
		}
		if e.Counts != nil {
			items = fmt.Sprint(e.Counts.Items)
		}
		printf("%4d  %-18s %-20s %-12s %5s %6s", e.Seq, e.Kind, e.Component, e.Pipeline, from, items)
		if e.Error != "" {
			printf("  error: %s", e.Error)
		}
		if e.Log != nil {
			printf("  %s: %s", e.Log.Level, oneLine(e.Log.Message))
		}
		if e.Endpoint != "" {
			printf("  endpoint %s", e.Endpoint)
		}
		printf("\n")
	}

	printf("\nstages:\n")
	for _, s := range rep.Stages {
		where := "in " + s.Pipeline
		if s.Pipeline == "" {
			where = s.Kind // a connector joins pipelines rather than running in one
		}
		printf("  %s %s (%s)\n", s.Component, where, s.Emission)
		for _, in := range s.Inputs {
			printf("    input seq %d: %s", in.Seq, in.Outcome)
			if in.Error != "" {
				printf(", error: %s", in.Error)
			}
			if d := in.Diff; d != nil {
				printf(", %d change(s), %d removed, %d added", len(d.Changes), len(d.Removed), len(d.Added))
				if m := d.Matching; m != nil {
					printf(" (matched: %d same record, %d exact, %d high, %d medium confidence)", m.Object, m.Exact, m.High, m.Medium)
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
			printf("    reconciliation over %d emission(s): %d removed, %d added, %d change(s)\n",
				len(s.Emissions), len(rc.Removed), len(rc.Added), len(rc.Changes))
		}
	}

	if len(rep.OTTL) > 0 {
		printf("\nottl replay:\n")
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
		printf("\naggregations:\n")
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
		printf("\nrouting:\n")
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
		printf("\nsampling decisions:\n")
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
		printf("\nbatching:\n")
	}
	for _, b := range rep.Batching {
		writeBatching(printf, b)
	}
	if len(rep.Grouping) > 0 {
		printf("\ngrouping:\n")
	}
	for _, g := range rep.Grouping {
		writeGrouping(printf, g)
	}
	if len(rep.Filtering) > 0 {
		printf("\nfiltering:\n")
	}
	for _, f := range rep.Filtering {
		writeFiltering(printf, f)
	}
	writeRecommendations(printf, rep.Recommendations)

	printf("\n")
	for _, o := range r.Outputs {
		printf("exporter %s (%s): %d batch(es), %d item(s)\n", o.Exporter, o.Signal, o.Batches, o.Counts.Items)
	}
	for _, o := range r.EndpointOutputs {
		printf("endpoint %s %s (%s): %d batch(es), %d item(s)\n", o.Component, o.Endpoint, o.Signal, o.Batches, o.Counts.Items)
	}
	if c := rep.Comparison; c != nil {
		switch {
		case c.Incomplete != "":
			printf("reference comparison: incomplete (%s)\n", c.Incomplete)
			for _, n := range c.Notes {
				printf("  note: %s\n", n)
			}
		case c.Equal:
			printf("reference comparison: equal\n")
			for _, n := range c.Notes {
				printf("  note: %s\n", n)
			}
		default:
			printf("reference comparison: DIFFERENT\n")
			for _, d := range c.Differences {
				printf("  - %s\n", d)
			}
		}
	}
	if a := rep.Assertions; a != nil {
		printf("assertions: %s (use the test command for exit codes)\n", label(a.Outcome))
		for _, res := range a.Results {
			printf("  %-12s %s: %s\n", label(res.Outcome), res.Name, res.Target)
			for _, c := range res.Checks {
				if c.Outcome != assertion.Pass {
					printf("      %s\n", c.Line())
				}
			}
		}
	}
	return errors.Join(errs...)
}

// writeRecommendations prints each recommendation with its kind and
// confidence, the suggested change, what it saves, and what limits it.
func writeRecommendations(printf func(string, ...any), recs []platform.Recommendation) {
	if len(recs) == 0 {
		return
	}
	printf("\nrecommendations:\n")
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

// writeBatching prints one batch processor's settings and its sends.
func writeBatching(printf func(string, ...any), b *analysis.Batching) {
	st := b.Settings
	printf("  %s (pipeline %s, %s): send_batch_size %d, timeout %s, send_batch_max_size %d", b.Processor, b.Pipeline, b.Signal, st.SendBatchSize, st.Timeout, st.SendBatchMaxSize)
	if len(st.MetadataKeys) > 0 {
		printf(", metadata_keys %s", strings.Join(st.MetadataKeys, ", "))
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
