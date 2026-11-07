package recommend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Sampling rules read analysis.Sampling: per trace, the recorded decision,
// each top-level policy's result on its own and whether the replay reproduced
// the recorded decision. Counts come only from verified traces; the others
// are named in coverage.
//
// The decision semantics the rules rely on are those of tail_sampling in
// contrib v0.162.0 (processor.go: evaluateDropPolicies, makeDecision; the
// evaluators in internal/sampling):
//   - drop policies are evaluated first, and a trace one of them drops is
//     dropped whatever the others decide;
//   - otherwise any other policy that samples keeps the trace;
//   - no evaluator returns the deprecated invert decisions, and only the
//     drop policy returns dropped, so no other policy can veto a sample;
//   - a policy that fails is skipped; with no sample the trace is not
//     sampled;
//   - sample_on_first_match only stops evaluating after the first sample.

// resultError is a policy result that evaluated nothing: the policy failed.
const resultError = "error"

// Top-level policy types (tailsamplingprocessor.PolicyType).
const (
	policyDrop         = "drop"
	policyAlwaysSample = "always_sample"
)

// rateDependentPolicies decide on the traffic the processor sees; the replay
// evaluates each trace alone, so their results there can differ from the
// run's.
var rateDependentPolicies = map[string]bool{"rate_limiting": true, "bytes_limiting": true, "composite": true}

// stablePolicies are the policy types whose result on a trace depends on that
// trace alone, with no nested policies whose types the report does not name
// (and, not), so their result evaluated alone is the result they have beside
// other policies.
var stablePolicies = map[string]bool{
	"always_sample": true, "latency": true, "status_code": true, "string_attribute": true,
	"numeric_attribute": true, "boolean_attribute": true, "probabilistic": true,
	"span_count": true, "trace_state": true, "trace_flags": true, "ottl_condition": true,
}

// verifiedTraces are the traces whose replay reproduced the recorded
// decision, and how many traces were left out.
func verifiedTraces(s *analysis.Sampling) (verified []analysis.SampledTrace, left int) {
	for _, t := range s.Traces {
		if t.Verification == analysis.ReplayVerified {
			verified = append(verified, t)
		}
	}
	return verified, len(s.Traces) - len(verified)
}

// policyResult is the result policy name had on its own on the trace.
func policyResult(t analysis.SampledTrace, name string) (string, bool) {
	for _, r := range t.Policies {
		if r.Policy == name {
			return r.Result, true
		}
	}
	return "", false
}

// uniquePolicies are the configured policies whose names no other policy
// shares, since results are matched to policies by name.
func uniquePolicies(s *analysis.Sampling) []analysis.SamplingPolicy {
	var out []analysis.SamplingPolicy
	for _, p := range s.Policies {
		same := 0
		for _, o := range s.Policies {
			if o.Name == p.Name {
				same++
			}
		}
		if p.Name != "" && same == 1 {
			out = append(out, p)
		}
	}
	return out
}

// policyLabel is "name (rule)", or the name alone when the rule is unknown.
func policyLabel(p analysis.SamplingPolicy) string {
	if p.Rule == "" {
		return p.Name
	}
	return fmt.Sprintf("%s (%s)", p.Name, p.Rule)
}

// traceEvidence cites at most ten traces of the processor.
func traceEvidence(processor string, traces []analysis.SampledTrace) []Evidence {
	out := []Evidence{{SectionSampling, processorRef(processor)}}
	for _, t := range traces {
		if len(out) > 10 {
			break
		}
		out = append(out, Evidence{SectionSampling, fmt.Sprintf("%s#trace[%s]", processorRef(processor), t.TraceID)})
	}
	return out
}

// withPolicies cites the named policies right after the processor, which
// traceEvidence lists first: "processor/<id>#policy[<name>]".
func withPolicies(evidence []Evidence, processor string, names ...string) []Evidence {
	var refs []Evidence
	for _, n := range names {
		refs = append(refs, Evidence{SectionSampling, fmt.Sprintf("%s#policy[%s]", processorRef(processor), n)})
	}
	return slices.Insert(evidence, 1, refs...)
}

// verifiedCoverage is the coverage lines shared by the rules that count
// verified traces.
func verifiedCoverage(verified, left int, other string) []string {
	out := []string{fmt.Sprintf("Observed on %s whose replay was verified; %s", plural(verified, "trace"), other)}
	if left > 0 {
		out = append(out, fmt.Sprintf("Not counted: %s whose replay was not verified (it differed, failed, was not replayed, or the trace had no decision).", plural(left, "trace")))
	}
	return out
}

// policyNeverDecides: a top-level policy that, evaluated on its own, neither
// sampled nor dropped any verified trace. One input says too little to remove
// a policy, so nothing is suggested; the summary says the rule may be wrong.
// A policy that failed on a trace is left out, since a failing policy is a
// different problem, and so is one with no result on a verified trace.
func policyNeverDecides(in Input) []Recommendation {
	var out []Recommendation
	for _, s := range in.Sampling {
		verified, left := verifiedTraces(s)
		if len(verified) == 0 {
			continue
		}
		for _, p := range uniquePolicies(s) {
			idle := true
			for _, t := range verified {
				if r, ok := policyResult(t, p.Name); !ok || r != analysis.TraceNotSampled {
					idle = false
					break
				}
			}
			if !idle {
				continue
			}
			coverage := verifiedCoverage(len(verified), left, "another input may hold traces it samples.")
			if rateDependentPolicies[p.Type] {
				coverage = append(coverage, fmt.Sprintf("A %s policy depends on all the traffic the processor sees; here each trace was evaluated alone.", p.Type))
			}
			out = append(out, Recommendation{
				Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: s.Pipeline,
				Components: []string{processorRef(s.Processor)},
				Summary: fmt.Sprintf("Policy %s of %s sampled or dropped nothing in the %s it was evaluated on in pipeline %s, each trace on its own. If this input holds traces the policy should sample, its rule may be wrong rather than unneeded.",
					policyLabel(p), s.Processor, plural(len(verified), "trace"), s.Pipeline),
				Evidence: withPolicies(traceEvidence(s.Processor, verified), s.Processor, p.Name),
				Benefit:  &Benefit{MetricEvaluationsWithoutEffect, len(verified)},
				Coverage: coverage,
			})
		}
	}
	return out
}

// policyShadowed: on every verified trace another policy sampled the trace on
// its own, so the policy's result changed no decision. It rests on the
// decision semantics above: a sample from any policy keeps a trace unless a
// drop policy drops it, and only a drop policy can drop. So a policy that is
// not a drop policy cannot change a decision on a trace another policy samples
// on its own.
//
// The other policy (the shadower) must be a policy type whose result depends
// on the trace alone, so that its result alone is its result beside the rest,
// and must have sampled every verified trace on its own; when several
// qualify, an always_sample policy is the one kept (it shadows on every
// input), else the first in configuration order. The
// shadowed policy must have sampled at least one verified trace, because a
// policy that sampled none is policy_never_decides' case. A drop policy is
// never shadowed: when it matches it overrides every other policy.
func policyShadowed(in Input) []Recommendation {
	var out []Recommendation
	for _, s := range in.Sampling {
		verified, left := verifiedTraces(s)
		if len(verified) == 0 {
			continue
		}
		policies := uniquePolicies(s)
		allSampled := func(name string) bool {
			for _, t := range verified {
				if r, ok := policyResult(t, name); !ok || r != analysis.TraceSampled {
					return false
				}
			}
			return true
		}
		// always_sample shadows on every input, so it is preferred; else the
		// first policy that qualifies.
		qualifies := func(p analysis.SamplingPolicy) bool { return stablePolicies[p.Type] && allSampled(p.Name) }
		shadower := slices.IndexFunc(policies, func(p analysis.SamplingPolicy) bool { return p.Type == policyAlwaysSample && qualifies(p) })
		if shadower < 0 {
			shadower = slices.IndexFunc(policies, qualifies)
		}
		if shadower < 0 {
			continue
		}
		by := policies[shadower]
		for _, p := range policies {
			if p.Name == by.Name || p.Type == policyDrop {
				continue
			}
			var sampled []analysis.SampledTrace
			for _, t := range verified {
				if r, _ := policyResult(t, p.Name); r == analysis.TraceSampled {
					sampled = append(sampled, t)
				}
			}
			if len(sampled) == 0 {
				continue
			}
			effect := "A policy that samples every trace keeps each one whatever the other policies decide, unless a drop policy drops it."
			cover := "it does not see traces another input may hold."
			if by.Type != policyAlwaysSample {
				effect = fmt.Sprintf("Any trace %s samples is kept whatever %s decides, unless a drop policy drops it. If another input holds traces %s samples that %s does not, %s is not shadowed there.",
					by.Name, p.Name, p.Name, by.Name, p.Name)
				cover = fmt.Sprintf("another input may hold traces %s samples and %s does not.", p.Name, by.Name)
			}
			both := ""
			if allSampled(p.Name) && stablePolicies[p.Type] {
				both = fmt.Sprintf(" %s also sampled every one of them, so either policy alone would do.", p.Name)
			}
			coverage := verifiedCoverage(len(verified), left, cover)
			coverage = append(coverage, "The decision does not change, but which policy is named as having sampled a trace (record_policy) and the sampling threshold written to the trace state (tracestate) can.")
			out = append(out, Recommendation{
				Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: s.Pipeline,
				Components: []string{processorRef(s.Processor)},
				Summary: fmt.Sprintf("Policy %s of %s sampled %d of the %s it was evaluated on in pipeline %s, but policy %s sampled every one of them on its own, so it changed no decision on this input.%s %s",
					policyLabel(p), s.Processor, len(sampled), plural(len(verified), "trace"), s.Pipeline, policyLabel(by), both, effect),
				Evidence: withPolicies(traceEvidence(s.Processor, sampled), s.Processor, p.Name, by.Name),
				Benefit:  &Benefit{MetricEvaluationsWithoutEffect, len(verified)},
				Coverage: coverage,
			})
		}
	}
	return out
}

// samplingLateInPipeline: processors before tail_sampling handled spans of
// traces it then did not keep (not_sampled or dropped). Kind inefficiency:
// the work was spent on data discarded afterwards, but moving tail_sampling
// first does not give the same output by construction (its policies would see
// the spans before those processors changed them), which is what an ordering
// recommendation claims, so no swap is suggested.
//
// The count per processor is exact when the processors from it up to
// tail_sampling recorded every span they removed: spans that reached
// tail_sampling plus, per trace, the spans removed on the way (the removed
// spans of each stage input name their trace). It is a lower bound when a
// stage in between recorded no diff (a batch processor, an unresolved
// input) or its counts do not match its removed spans. A stage in between that
// added spans cannot be attributed, and leaves the processor out.
// memory_limiter is exempt: it guards the pipeline wherever it stands.
func samplingLateInPipeline(in Input) []Recommendation {
	var out []Recommendation
	for _, s := range in.Sampling {
		p, ok := pipeline(in.Topology, s.Pipeline)
		if !ok {
			continue
		}
		ids := pipelineIDs(p)
		at := slices.Index(ids, s.Processor)
		if at <= 0 {
			continue
		}
		spans := map[string]int{}
		var unkept []analysis.SampledTrace
		decided, pending := 0, 0
		for _, t := range s.Traces {
			switch t.Decision {
			case analysis.TraceNotSampled, analysis.TraceDropped:
				if n := traceSpans(t); n > 0 {
					spans[t.TraceID] = n
					unkept = append(unkept, t)
				}
				decided++
			case analysis.TraceSampled:
				decided++
			default:
				pending++
			}
		}
		if len(unkept) == 0 {
			continue
		}
		for i := range at {
			id := ids[i]
			if componentType(id) == "memory_limiter" {
				continue
			}
			if c, ok := in.Topology.Component(processorRef(id)); !ok || c.Mode != topology.ModeReal {
				continue
			}
			handled, received, exact, ok := spansBeforeSampling(in.Stages, s.Pipeline, ids[i:at], spans)
			if !ok {
				continue
			}
			prefix, bound := "", ""
			if !exact {
				prefix = "at least "
				bound = fmt.Sprintf("A lower bound: a processor from %s to %s did not record which spans of these traces it removed (a batch processor, for one), so spans removed there may be missing from the count.", id, s.Processor)
			}
			total := ""
			if received > 0 {
				total = fmt.Sprintf(" (of the %s it received)", plural(received, "span"))
			}
			coverage := []string{fmt.Sprintf("Observed on %s that %s did not keep, of the %s it decided; another input may keep more or fewer.",
				plural(len(unkept), "trace"), s.Processor, plural(decided, "trace"))}
			if pending > 0 {
				coverage = append(coverage, fmt.Sprintf("Not counted: %s with no sampling decision (pending or dropped at shutdown).", plural(pending, "trace")))
			}
			if bound != "" {
				coverage = append(coverage, bound)
			}
			coverage = append(coverage, fmt.Sprintf("Policies that read attributes set by %s would see different values if %s ran before them; this run does not show whether any do.",
				strings.Join(ids[i:at], ", "), s.Processor))
			evidence := []Evidence{{SectionTopology, "pipeline/" + s.Pipeline}, {SectionStages, processorRef(id)}}
			for _, t := range unkept {
				if len(evidence) >= 12 {
					break
				}
				evidence = append(evidence, Evidence{SectionSampling, fmt.Sprintf("%s#trace[%s]", processorRef(s.Processor), t.TraceID)})
			}
			out = append(out, Recommendation{
				Kind: KindInefficiency, Confidence: ConfidenceObserved, Pipeline: s.Pipeline,
				Components: []string{processorRef(id), processorRef(s.Processor)},
				Summary: fmt.Sprintf("%s handled %s%s%s of %s that %s did not keep in pipeline %s. Run after %s, it would not have handled them. No swap is suggested: moving %s earlier changes what its policies see.",
					id, prefix, plural(handled, "span"), total, plural(len(unkept), "trace"), s.Processor, s.Pipeline, s.Processor, s.Processor),
				Evidence: evidence,
				Benefit:  &Benefit{MetricRecordsNotProcessed, handled},
				Coverage: coverage,
			})
		}
	}
	return out
}

// traceSpans are the spans of the trace that reached the processor: the
// records of its inputs, or else those evaluated plus the late ones.
func traceSpans(t analysis.SampledTrace) int {
	if len(t.Records) > 0 {
		return len(t.Records)
	}
	return t.Spans + t.LateSpans
}

// spansBeforeSampling counts the spans of the given traces that the first of
// chain handled: those that reached tail_sampling (spans, per trace) plus
// those that any processor of chain removed. received is the first
// processor's input count when every one of its inputs has a diff, else 0.
// exact is false when a stage of chain left that count short; ok is false
// when it cannot be attributed at all (no stage for the first processor, or a
// stage that added spans).
func spansBeforeSampling(stages []analysis.Stage, pipeline string, chain []string, spans map[string]int) (handled, received int, exact, ok bool) {
	exact = true
	for _, n := range spans {
		handled += n
	}
	for k, id := range chain {
		i := slices.IndexFunc(stages, func(s analysis.Stage) bool {
			return s.Kind == analysis.KindProcessor && s.Component == id && s.Pipeline == pipeline
		})
		if i < 0 || len(stages[i].Inputs) == 0 {
			if k == 0 {
				return 0, 0, false, false
			}
			exact = false
			continue
		}
		counted := 0
		for _, input := range stages[i].Inputs {
			d := input.Diff
			if d == nil {
				exact, counted = false, -1
				continue
			}
			if len(d.Added) > 0 {
				return 0, 0, false, false
			}
			if d.Before.Items-d.After.Items != len(d.Removed) {
				exact = false
			}
			for _, r := range d.Removed {
				if _, kept := spans[r.TraceID]; kept && r.TraceID != "" {
					handled++
				}
			}
			if counted >= 0 {
				counted += d.Before.Items
			}
		}
		if k == 0 && counted > 0 {
			received = counted
		}
	}
	return handled, received, exact, true
}
