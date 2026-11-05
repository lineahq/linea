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
