package recommend

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// results builds a trace's per-policy results from name=result pairs.
func results(pairs ...string) []analysis.PolicyResult {
	var out []analysis.PolicyResult
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, analysis.PolicyResult{Policy: pairs[i], Result: pairs[i+1]})
	}
	return out
}

func sampledTrace(id, decision, verification string, policies ...string) analysis.SampledTrace {
	return analysis.SampledTrace{TraceID: id, Decision: decision, Verification: verification, Policies: results(policies...)}
}

func policy(name, typ string) analysis.SamplingPolicy {
	return analysis.SamplingPolicy{Name: name, Type: typ, Rule: "rule of " + name}
}

func samplingOf(policies []analysis.SamplingPolicy, traces ...analysis.SampledTrace) []*analysis.Sampling {
	return []*analysis.Sampling{{Processor: "tail_sampling", Pipeline: "traces", DecisionWait: "1s", Policies: policies, Traces: traces}}
}

const (
	sampled     = analysis.TraceSampled
	notSampled  = analysis.TraceNotSampled
	dropped     = analysis.TraceDropped
	verifiedRun = analysis.ReplayVerified
)

// TestPolicyNeverDecidesCountsVerifiedTracesOnly: a policy fires only when
// its result alone was never sampled or dropped on a verified trace; traces
// without a verified replay are named in coverage and do not count, and a
// failed or missing result is not evidence.
func TestPolicyNeverDecidesCountsVerifiedTracesOnly(t *testing.T) {
	errors := policy("errors", "status_code")
	slow := policy("slow", "latency")
	cases := []struct {
		name     string
		policies []analysis.SamplingPolicy
		traces   []analysis.SampledTrace
		want     int // benefit of the one expected recommendation; 0 for none
		coverage int
	}{
		{"never sampled", []analysis.SamplingPolicy{errors, slow}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "errors", notSampled, "slow", sampled),
			sampledTrace("b", notSampled, verifiedRun, "errors", notSampled, "slow", notSampled),
		}, 2, 1},
		{"sampled on one trace", []analysis.SamplingPolicy{errors}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "errors", sampled),
			sampledTrace("b", notSampled, verifiedRun, "errors", notSampled),
		}, 0, 0},
		{"unverified traces are not counted", []analysis.SamplingPolicy{errors}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, verifiedRun, "errors", notSampled),
			sampledTrace("b", sampled, analysis.ReplayDiffers, "errors", sampled),
			{TraceID: "c", Decision: analysis.TracePending},
		}, 1, 2},
		{"only unverified traces", []analysis.SamplingPolicy{errors}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, analysis.ReplayFailed, "errors", notSampled),
		}, 0, 0},
		{"a failed evaluation", []analysis.SamplingPolicy{errors}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, verifiedRun, "errors", notSampled),
			sampledTrace("b", notSampled, verifiedRun, "errors", "error"),
		}, 0, 0},
		{"a missing result", []analysis.SamplingPolicy{errors}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, verifiedRun),
		}, 0, 0},
		{"a drop policy that dropped", []analysis.SamplingPolicy{policy("noise", "drop")}, []analysis.SampledTrace{
			sampledTrace("a", dropped, verifiedRun, "noise", dropped),
		}, 0, 0},
		{"a drop policy that never matched", []analysis.SamplingPolicy{policy("noise", "drop")}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, verifiedRun, "noise", notSampled),
		}, 1, 1},
		{"policies sharing a name", []analysis.SamplingPolicy{errors, errors}, []analysis.SampledTrace{
			sampledTrace("a", notSampled, verifiedRun, "errors", notSampled),
		}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := policyNeverDecides(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}), Sampling: samplingOf(c.policies, c.traces...)})
			if c.want == 0 {
				if len(recs) != 0 {
					t.Fatalf("recs = %+v, want none", recs)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("recs = %+v, want one", recs)
			}
			r := recs[0]
			if r.Rule != "" || r.Kind != KindRedundancy || r.Confidence != ConfidenceObserved || r.Change != nil || r.Pipeline != "traces" ||
				!slices.Equal(r.Components, []string{"processor/tail_sampling"}) {
				t.Fatalf("recommendation = %+v", r)
			}
			if r.Benefit == nil || r.Benefit.Metric != MetricEvaluationsWithoutEffect || r.Benefit.Count != c.want {
				t.Fatalf("benefit = %+v, want %d", r.Benefit, c.want)
			}
			if !strings.Contains(r.Summary, "its rule may be wrong rather than unneeded") || !strings.Contains(r.Summary, "nothing in the "+plural(c.want, "trace")) {
				t.Fatalf("summary = %q", r.Summary)
			}
			if len(r.Coverage) != c.coverage {
				t.Fatalf("coverage = %q, want %d lines", r.Coverage, c.coverage)
			}
		})
	}
}

// TestPolicyNeverDecidesCitesTenTracesAndNamesRateLimits: the evidence is the
// processor, the policy and at most ten traces; a rate limit adds the replay's caveat.
func TestPolicyNeverDecidesCitesTenTracesAndNamesRateLimits(t *testing.T) {
	var traces []analysis.SampledTrace
	for i := range 12 {
		traces = append(traces, sampledTrace(fmt.Sprintf("t%d", i), notSampled, verifiedRun, "cap", notSampled))
	}
	recs := policyNeverDecides(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}),
		Sampling: samplingOf([]analysis.SamplingPolicy{policy("cap", "rate_limiting")}, traces...)})
	if len(recs) != 1 || len(recs[0].Evidence) != 12 || recs[0].Evidence[0] != (Evidence{SectionSampling, "processor/tail_sampling"}) ||
		recs[0].Evidence[1] != (Evidence{SectionSampling, "processor/tail_sampling#policy[cap]"}) ||
		recs[0].Evidence[2] != (Evidence{SectionSampling, "processor/tail_sampling#trace[t0]"}) {
		t.Fatalf("recs = %+v", recs)
	}
	if recs[0].Benefit.Count != 12 || len(recs[0].Coverage) != 2 || !strings.Contains(recs[0].Coverage[1], "rate_limiting") {
		t.Fatalf("benefit = %+v, coverage = %q", recs[0].Benefit, recs[0].Coverage)
	}
}

// TestPolicyShadowedNeedsAPolicyThatSamplesEveryVerifiedTrace: a policy is
// shadowed when another policy of a self-contained type sampled every verified
// trace on its own and it sampled at least one; drop policies are never
// shadowed and a limiter or a partial sampler shadows nothing.
func TestPolicyShadowedNeedsAPolicyThatSamplesEveryVerifiedTrace(t *testing.T) {
	always := policy("always", "always_sample")
	slow := policy("slow", "latency")
	errors := policy("errors", "status_code")
	traces := func(policies ...string) []analysis.SampledTrace {
		return []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, policies...),
		}
	}
	cases := []struct {
		name     string
		policies []analysis.SamplingPolicy
		traces   []analysis.SampledTrace
		want     []string // shadowed policy names, in order
		by       string
	}{
		{"always_sample shadows a policy that sampled", []analysis.SamplingPolicy{slow, always}, traces("slow", sampled, "always", sampled),
			[]string{"slow"}, "always"},
		{"a policy that sampled nothing is not this rule's", []analysis.SamplingPolicy{slow, always}, traces("slow", notSampled, "always", sampled),
			nil, ""},
		{"two policies that sample everything keep the first", []analysis.SamplingPolicy{always, policy("also", "always_sample")}, traces("always", sampled, "also", sampled),
			[]string{"also"}, "always"},
		{"a conditional policy that sampled every trace", []analysis.SamplingPolicy{slow, errors}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "slow", sampled, "errors", sampled),
			sampledTrace("b", sampled, verifiedRun, "slow", sampled, "errors", notSampled),
		}, []string{"errors"}, "slow"},
		{"no policy sampled every trace", []analysis.SamplingPolicy{slow, errors}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "slow", sampled, "errors", notSampled),
			sampledTrace("b", sampled, verifiedRun, "slow", notSampled, "errors", sampled),
		}, nil, ""},
		{"a drop policy is never shadowed", []analysis.SamplingPolicy{policy("noise", "drop"), always}, traces("noise", notSampled, "always", sampled),
			nil, ""},
		{"a drop policy that dropped is not a shadowed sampler", []analysis.SamplingPolicy{policy("noise", "drop"), slow, always}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "noise", notSampled, "slow", sampled, "always", sampled),
			sampledTrace("b", dropped, verifiedRun, "noise", dropped, "slow", sampled, "always", sampled),
		}, []string{"slow"}, "always"},
		{"a rate limit does not shadow", []analysis.SamplingPolicy{slow, policy("cap", "rate_limiting")}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "slow", sampled, "cap", sampled),
			sampledTrace("b", sampled, verifiedRun, "slow", notSampled, "cap", sampled),
		}, nil, ""},
		{"a nested policy does not shadow", []analysis.SamplingPolicy{slow, policy("both", "and")}, []analysis.SampledTrace{
			sampledTrace("a", sampled, verifiedRun, "slow", sampled, "both", sampled),
			sampledTrace("b", sampled, verifiedRun, "slow", notSampled, "both", sampled),
		}, nil, ""},
		{"a rate limit is shadowed like any other policy", []analysis.SamplingPolicy{policy("cap", "rate_limiting"), always}, traces("cap", sampled, "always", sampled),
			[]string{"cap"}, "always"},
		{"a failed evaluation of the shadower", []analysis.SamplingPolicy{slow, always}, traces("slow", sampled, "always", "error"),
			nil, ""},
		{"only unverified traces", []analysis.SamplingPolicy{slow, always}, []analysis.SampledTrace{
			sampledTrace("a", sampled, analysis.ReplayDiffers, "slow", sampled, "always", sampled),
		}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := policyShadowed(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}), Sampling: samplingOf(c.policies, c.traces...)})
			if len(recs) != len(c.want) {
				t.Fatalf("recs = %+v, want shadowed %q", recs, c.want)
			}
			for i, r := range recs {
				if r.Rule != "" || r.Kind != KindRedundancy || r.Confidence != ConfidenceObserved || r.Change != nil || r.Pipeline != "traces" {
					t.Fatalf("recommendation = %+v", r)
				}
				if !strings.HasPrefix(r.Summary, "Policy "+c.want[i]+" (rule of "+c.want[i]+") of tail_sampling sampled") ||
					!strings.Contains(r.Summary, "but policy "+c.by+" (rule of "+c.by+") sampled every one of them on its own") {
					t.Fatalf("summary = %q", r.Summary)
				}
				if r.Benefit == nil || r.Benefit.Metric != MetricEvaluationsWithoutEffect || r.Benefit.Count != len(c.traces) {
					t.Fatalf("benefit = %+v", r.Benefit)
				}
				if !strings.Contains(strings.Join(r.Coverage, "\n"), "record_policy") {
					t.Fatalf("coverage = %q", r.Coverage)
				}
			}
		})
	}
}

// TestPolicyShadowedSaysWhatTheShadowerCovers: always_sample holds on every
// input; another policy's coverage holds on this input only. The evidence
// cites the traces the shadowed policy sampled, and unverified traces are
// named in coverage.
func TestPolicyShadowedSaysWhatTheShadowerCovers(t *testing.T) {
	policies := []analysis.SamplingPolicy{policy("slow", "latency"), policy("always", "always_sample"), policy("errors", "status_code")}
	traces := []analysis.SampledTrace{
		sampledTrace("a", sampled, verifiedRun, "slow", sampled, "always", sampled, "errors", notSampled),
		sampledTrace("b", sampled, verifiedRun, "slow", notSampled, "always", sampled, "errors", sampled),
		sampledTrace("c", sampled, analysis.ReplayFailed),
	}
	recs := policyShadowed(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}), Sampling: samplingOf(policies, traces...)})
	if len(recs) != 2 {
		t.Fatalf("recs = %+v", recs)
	}
	slow, errors := recs[0], recs[1]
	if !slices.Equal(slow.Evidence, []Evidence{{SectionSampling, "processor/tail_sampling"}, {SectionSampling, "processor/tail_sampling#policy[slow]"},
		{SectionSampling, "processor/tail_sampling#policy[always]"}, {SectionSampling, "processor/tail_sampling#trace[a]"}}) {
		t.Fatalf("evidence = %+v", slow.Evidence)
	}
	if !strings.Contains(slow.Summary, "sampled 1 of the 2 traces") || !strings.Contains(slow.Summary, "A policy that samples every trace keeps each one") {
		t.Fatalf("summary = %q", slow.Summary)
	}
	if len(slow.Coverage) != 3 || !strings.Contains(slow.Coverage[1], "Not counted: 1 trace whose replay was not verified") {
		t.Fatalf("coverage = %q", slow.Coverage)
	}
	if !strings.Contains(errors.Summary, "Policy errors") || !strings.Contains(errors.Summary, "A policy that samples every trace") {
		t.Fatalf("summary = %q", errors.Summary)
	}

	// With a conditional shadower the wording is limited to this input.
	traces = []analysis.SampledTrace{sampledTrace("a", sampled, verifiedRun, "slow", sampled, "errors", sampled)}
	recs = policyShadowed(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}), Sampling: samplingOf(policies[:1:1], traces...)})
	if len(recs) != 0 {
		t.Fatalf("a single policy cannot be shadowed: %+v", recs)
	}
	recs = policyShadowed(Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}),
		Sampling: samplingOf([]analysis.SamplingPolicy{policy("slow", "latency"), policy("errors", "status_code")}, traces...)})
	if len(recs) != 1 || !strings.Contains(recs[0].Summary, "If another input holds traces errors samples that slow does not") ||
		!strings.Contains(recs[0].Summary, "also sampled every one of them, so either policy alone would do") {
		t.Fatalf("recs = %+v", recs)
	}
}

// stageWithDiffs is a processor stage whose inputs have the given diffs.
func stageWithDiffs(id string, diffs ...*analysis.Diff) analysis.Stage {
	s := analysis.Stage{Component: id, Pipeline: "traces", Kind: analysis.KindProcessor, Type: componentType(id), Signal: "traces", Emission: analysis.EmissionSynchronous}
	for i, d := range diffs {
		s.Inputs = append(s.Inputs, analysis.StageInput{Seq: i + 1, Outcome: analysis.OutcomeEmitted, Diff: d})
	}
	return s
}

// unchanged is a diff of n spans that changed nothing.
func unchanged(n int) *analysis.Diff {
	c := telemetry.Counts{Items: n}
	return &analysis.Diff{Identity: analysis.IdentitySpan, Before: c, After: c}
}

// removing is a diff that removed one span of each given trace from n.
func removing(n int, traceIDs ...string) *analysis.Diff {
	d := &analysis.Diff{Identity: analysis.IdentitySpan, Before: telemetry.Counts{Items: n}, After: telemetry.Counts{Items: n - len(traceIDs)}}
	for _, id := range traceIDs {
		d.Removed = append(d.Removed, analysis.RecordRef{TraceID: id, SpanID: "s", Name: "span"})
	}
	return d
}

func traceOf(id, decision string, spans int) analysis.SampledTrace {
	t := analysis.SampledTrace{TraceID: id, Decision: decision, Spans: spans}
	for i := range spans {
		t.Records = append(t.Records, fmt.Sprintf("1:resource[0].scope[0].span[%d]", i))
	}
	return t
}

// TestSamplingLateInPipelineCountsSpansOfTracesNotKept: each processor before
// tail_sampling is one recommendation counting the spans of not-kept traces
// it handled, with the spans removed on the way added back; memory_limiter is
// exempt and nothing is suggested.
func TestSamplingLateInPipelineCountsSpansOfTracesNotKept(t *testing.T) {
	traces := []analysis.SampledTrace{
		traceOf("a", analysis.TraceNotSampled, 3), traceOf("b", analysis.TraceDropped, 2),
		traceOf("c", analysis.TraceSampled, 4), traceOf("d", analysis.TracePending, 1),
	}
	cases := []struct {
		name       string
		processors []string
		stages     []analysis.Stage
		want       map[string]int // processor -> spans handled
		inexact    bool
		received   map[string]string
	}{
		{"memory_limiter is exempt", []string{"memory_limiter", "transform", "tail_sampling"},
			[]analysis.Stage{stageWithDiffs("memory_limiter", unchanged(10)), stageWithDiffs("transform", unchanged(10))},
			map[string]int{"transform": 5}, false, map[string]string{"transform": "of the 10 spans it received"}},
		{"spans removed on the way are added back", []string{"filter", "transform", "tail_sampling"},
			[]analysis.Stage{stageWithDiffs("filter", removing(11, "a", "c")), stageWithDiffs("transform", removing(10, "b"))},
			map[string]int{"filter": 7, "transform": 6}, false, nil},
		{"a batch processor in between gives a lower bound", []string{"transform", "batch", "tail_sampling"},
			[]analysis.Stage{stageWithDiffs("transform", unchanged(10)), {Component: "batch", Pipeline: "traces", Kind: analysis.KindProcessor, Type: "batch",
				Emission: analysis.EmissionAsynchronous, Inputs: []analysis.StageInput{{Seq: 1, Outcome: analysis.OutcomeDeferred}}}},
			map[string]int{"transform": 5, "batch": 5}, true, nil},
		{"a diff that does not match its removals gives a lower bound", []string{"filter", "tail_sampling"},
			[]analysis.Stage{stageWithDiffs("filter", &analysis.Diff{Before: telemetry.Counts{Items: 10}, After: telemetry.Counts{Items: 8}, Removed: []analysis.RecordRef{{TraceID: "a"}}})},
			map[string]int{"filter": 6}, true, nil},
		{"a stage that added spans cannot be attributed", []string{"transform", "tail_sampling"},
			[]analysis.Stage{stageWithDiffs("transform", &analysis.Diff{Before: telemetry.Counts{Items: 10}, After: telemetry.Counts{Items: 11}, Added: []analysis.RecordRef{{TraceID: "a"}}})},
			nil, false, nil},
		{"no stage for the processor", []string{"transform", "tail_sampling"}, nil, nil, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := samplingLateInPipeline(Input{Topology: graph(map[string][]string{"traces": c.processors}), Stages: c.stages, Sampling: samplingOf(nil, traces...)})
			got := map[string]int{}
			for _, r := range recs {
				if r.Rule != "" || r.Kind != KindInefficiency || r.Confidence != ConfidenceObserved || r.Change != nil || r.Pipeline != "traces" {
					t.Fatalf("recommendation = %+v", r)
				}
				if len(r.Components) != 2 || r.Components[1] != "processor/tail_sampling" || r.Benefit == nil || r.Benefit.Metric != MetricRecordsNotProcessed {
					t.Fatalf("recommendation = %+v", r)
				}
				id := strings.TrimPrefix(r.Components[0], "processor/")
				got[id] = r.Benefit.Count
				if !strings.Contains(r.Summary, "of 2 traces that tail_sampling did not keep") || !strings.Contains(r.Summary, "No swap is suggested") {
					t.Fatalf("summary = %q", r.Summary)
				}
				if exact := !strings.Contains(r.Summary, "handled at least"); exact == c.inexact && id != "batch" {
					t.Fatalf("%s summary = %q, inexact = %t", id, r.Summary, c.inexact)
				}
				if want := c.received[id]; want != "" && !strings.Contains(r.Summary, want) {
					t.Fatalf("summary = %q, want %q", r.Summary, want)
				}
				all := strings.Join(r.Coverage, "\n")
				if !strings.Contains(all, "Policies that read attributes set by "+id) || !strings.Contains(all, "Not counted: 1 trace with no sampling decision") ||
					!strings.Contains(all, "Observed on 2 traces that tail_sampling did not keep, of the 3 traces it decided") {
					t.Fatalf("coverage = %q", r.Coverage)
				}
			}
			if !maps_equal(got, c.want) {
				t.Fatalf("handled = %v, want %v", got, c.want)
			}
		})
	}
}

func maps_equal(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestSamplingLateInPipelineNeedsProcessorsThatRanBeforeDroppedTraces: it says
// nothing without a trace tail_sampling did not keep, without a processor
// before it, or for a stand-in processor.
func TestSamplingLateInPipelineNeedsProcessorsThatRanBeforeDroppedTraces(t *testing.T) {
	stages := []analysis.Stage{stageWithDiffs("transform", unchanged(4))}
	kept := samplingOf(nil, traceOf("a", analysis.TraceSampled, 4))
	notKept := samplingOf(nil, traceOf("a", analysis.TraceNotSampled, 4))
	standIn := graph(map[string][]string{"traces": {"transform", "tail_sampling"}})
	for i := range standIn.Components {
		if standIn.Components[i].ID == "transform" {
			standIn.Components[i].Mode = topology.ModeStandIn
		}
	}
	cases := []struct {
		name     string
		topology *topology.Graph
		sampling []*analysis.Sampling
		want     int
	}{
		{"every trace kept", graph(map[string][]string{"traces": {"transform", "tail_sampling"}}), kept, 0},
		{"tail_sampling is first", graph(map[string][]string{"traces": {"tail_sampling", "transform"}}), notKept, 0},
		{"a stand-in processor", standIn, notKept, 0},
		{"another pipeline", graph(map[string][]string{"traces/other": {"transform", "tail_sampling"}}), notKept, 0},
		{"a processor before it", graph(map[string][]string{"traces": {"transform", "tail_sampling"}}), notKept, 1},
		{"a trace with no spans on record", graph(map[string][]string{"traces": {"transform", "tail_sampling"}}),
			samplingOf(nil, analysis.SampledTrace{TraceID: "a", Decision: analysis.TraceNotSampled}), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := samplingLateInPipeline(Input{Topology: c.topology, Stages: stages, Sampling: c.sampling})
			if len(recs) != c.want {
				t.Fatalf("recs = %+v, want %d", recs, c.want)
			}
		})
	}
}

// TestSamplingLateInPipelineCountsLateSpansOfTraces: a trace's spans are its
// records, or the evaluated and late spans when the records were cut.
func TestSamplingLateInPipelineCountsLateSpansOfTraces(t *testing.T) {
	cut := analysis.SampledTrace{TraceID: "a", Decision: analysis.TraceDropped, Spans: 3, LateSpans: 2}
	recs := samplingLateInPipeline(Input{Topology: graph(map[string][]string{"traces": {"transform", "tail_sampling"}}),
		Stages: []analysis.Stage{stageWithDiffs("transform", unchanged(5))}, Sampling: samplingOf(nil, cut)})
	if len(recs) != 1 || recs[0].Benefit.Count != 5 || !strings.Contains(recs[0].Summary, "handled 5 spans (of the 5 spans it received) of 1 trace that") {
		t.Fatalf("recs = %+v", recs)
	}
	if recs[0].Evidence[0] != (Evidence{SectionTopology, "pipeline/traces"}) || recs[0].Evidence[1] != (Evidence{SectionStages, "processor/transform"}) ||
		recs[0].Evidence[2] != (Evidence{SectionSampling, "processor/tail_sampling#trace[a]"}) {
		t.Fatalf("evidence = %+v", recs[0].Evidence)
	}
}
