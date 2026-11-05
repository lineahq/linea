// Package sampling explains tail sampling decisions. The decisions
// themselves are observations: the instrumented processor's decision hooks
// recorded each one with the spans its policies evaluated. Like aggregation
// and ottldebug, this package adds the why by replaying the processor's
// configuration on those spans with the upstream factory, once in full to
// verify the recorded decision and once per top-level policy to show what
// each policy decides on its own, and reports what each policy looked at.
package sampling

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor/pkg/samplingpolicy"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/features"
)

// MaxReplayedTraces bounds the traces replayed per processor; decisions of
// further traces are still reported, without policy results.
const MaxReplayedTraces = 200

// Input is a stage input of the processor.
type Input = features.SamplingInput

// Decision is a recorded stage.decision event.
type Decision = features.SamplingDecision

// The explanation is an optional feature: the generated registry
// imports this package only when the manifest has the tail_sampling
// processor.
func init() { features.ProvideSampling(Tail) }

// Tail explains the decisions of one tail_sampling processor (contrib
// v0.162.0) in one pipeline. completed says whether the run shut down
// cleanly, which decides whether traces without a decision were dropped at
// shutdown or are still pending.
func Tail(ctx context.Context, processor, pipeline string, cfg component.Config, inputs []Input, decisions []Decision, completed bool) (*analysis.Sampling, error) {
	c, ok := cfg.(*tailsamplingprocessor.Config)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected configuration %T", processor, cfg)
	}
	out := &analysis.Sampling{
		Processor: processor, Pipeline: pipeline, DecisionWait: c.DecisionWait.String(),
		Policies: []analysis.SamplingPolicy{}, Traces: []analysis.SampledTrace{},
	}
	rateDependent := false
	for _, p := range c.PolicyCfgs {
		l := fromPolicy(p)
		out.Policies = append(out.Policies, analysis.SamplingPolicy{Name: p.Name, Type: string(p.Type), Rule: l.rule()})
		rateDependent = rateDependent || l.rateDependent()
	}

	spans := indexInputs(inputs)
	// Traces are reported in the order they arrived: decisions made in
	// one batch are released in map order.
	first := map[string]Decision{}
	order := slices.Clone(spans.order)
	for _, d := range decisions {
		if _, seen := first[d.TraceID]; !seen {
			first[d.TraceID] = d
			if _, arrived := spans.byTrace[d.TraceID]; !arrived {
				order = append(order, d.TraceID)
			}
		}
	}

	replayed := 0
	for _, id := range order {
		all := spans.byTrace[id]
		t := analysis.SampledTrace{TraceID: id, Name: traceName(all), Inputs: inputSeqs(all), Policies: []analysis.PolicyResult{}}
		for _, s := range all {
			t.Records = append(t.Records, s.ref)
		}
		d, decided := first[id]
		if !decided {
			t.Spans, t.Duration = len(all), duration(all)
			t.Decision = analysis.TracePending
			if completed && c.DropPendingTracesOnShutdown {
				t.Decision = analysis.TracePendingDropped
			}
			out.Traces = append(out.Traces, t)
			continue
		}
		evaluated := spansOf(d.Spans.Traces)
		t.Decision, t.Policy, t.Event = d.Outcome, d.Policy, d.Seq
		t.Spans, t.Duration = len(evaluated), duration(evaluated)
		for _, s := range all {
			if s.seq > d.Seq {
				t.LateSpans++
			}
		}
		if t.LateSpans > 0 {
			t.Late = "dropped"
			if d.Outcome == analysis.TraceSampled {
				t.Late = "forwarded"
			}
		}
		if replayed == MaxReplayedTraces {
			out.Traces = append(out.Traces, t)
			continue
		}
		replayed++
		explain(ctx, c, d, evaluated, &t)
		out.Traces = append(out.Traces, t)
	}

	if replayed == MaxReplayedTraces && len(first) > MaxReplayedTraces {
		out.Notes = append(out.Notes, fmt.Sprintf("only the first %d decisions were replayed", MaxReplayedTraces))
	}
	if rateDependent {
		out.Notes = append(out.Notes, "rate and bytes limits depend on all the traffic the processor sees; the replay evaluates each trace alone, so those policies can decide differently than in the run")
	}
	if c.SampleOnFirstMatch {
		out.Notes = append(out.Notes, "sample_on_first_match: the processor stops at the first policy that samples, in configuration order")
	}
	return out, nil
}

// explain replays the full configuration and each policy alone on the
// evaluated spans of d.
func explain(ctx context.Context, c *tailsamplingprocessor.Config, d Decision, evaluated []span, t *analysis.SampledTrace) {
	outcome, policy, err := replay(ctx, c, c.PolicyCfgs, d.Spans.Traces)
	switch {
	case err != nil:
		t.Verification = analysis.ReplayFailed
	case outcome == d.Outcome && (outcome == analysis.TraceNotSampled || policy == d.Policy):
		t.Verification = analysis.ReplayVerified
	default:
		t.Verification = analysis.ReplayDiffers
	}
	for _, p := range c.PolicyCfgs {
		r := analysis.PolicyResult{Policy: p.Name, Evidence: fromPolicy(p).evidence(evaluated)}
		res, _, err := replay(ctx, c, []tailsamplingprocessor.PolicyCfg{p}, d.Spans.Traces)
		if err != nil {
			r.Result, r.Evidence = "error", err.Error()
		} else {
			r.Result = res
		}
		t.Policies = append(t.Policies, r)
	}
}

// replay runs a copy of the processor with the given policies on one
// trace and returns its decision. The decision wait is long enough that the
// decision is made when the processor shuts down, which evaluates every
// pending trace (drop_pending_traces_on_shutdown is turned off).
func replay(ctx context.Context, c *tailsamplingprocessor.Config, policies []tailsamplingprocessor.PolicyCfg, td ptrace.Traces) (outcome, policy string, err error) {
	var mu sync.Mutex
	outcome = analysis.TracePending
	hook := func(_ context.Context, _ pcommon.TraceID, d *tailsamplingprocessor.TraceData) {
		mu.Lock()
		defer mu.Unlock()
		if outcome == analysis.TracePending {
			outcome, policy = decisionOutcome(d.FinalDecision), d.PolicyName
		}
	}
	rc := *c
	rc.PolicyCfgs = policies
	rc.DecisionWait = time.Hour
	rc.DecisionWaitAfterRootReceived = 0
	rc.DropPendingTracesOnShutdown = false
	rc.DecisionCache = tailsamplingprocessor.DecisionCacheConfig{}
	rc.Options = []tailsamplingprocessor.Option{tailsamplingprocessor.WithSampledHooks(hook), tailsamplingprocessor.WithNonSampledHooks(hook)}

	f := tailsamplingprocessor.NewFactory()
	p, err := f.CreateTraces(ctx, processortest.NewNopSettings(f.Type()), &rc, consumertest.NewNop())
	if err != nil {
		return "", "", err
	}
	if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
		return "", "", err
	}
	data := ptrace.NewTraces()
	td.CopyTo(data)
	consumeErr := p.ConsumeTraces(ctx, data)
	if err := p.Shutdown(ctx); err != nil {
		return "", "", err
	}
	if consumeErr != nil {
		return "", "", consumeErr
	}
	mu.Lock()
	defer mu.Unlock()
	if outcome == analysis.TracePending {
		return "", "", fmt.Errorf("the replay made no decision")
	}
	return outcome, policy, nil
}

func decisionOutcome(d samplingpolicy.Decision) string {
	switch d {
	case samplingpolicy.Sampled:
		return analysis.TraceSampled
	case samplingpolicy.Dropped:
		return analysis.TraceDropped
	case samplingpolicy.NotSampled:
		return analysis.TraceNotSampled
	}
	return strings.ToLower(d.String())
}

// span is one span with the resource it belongs to and, for input spans,
// its record ref.
type span struct {
	seq      int
	ref      string
	span     ptrace.Span
	resource pcommon.Resource
}

type inputSpans struct {
	byTrace map[string][]span
	order   []string
}

func indexInputs(inputs []Input) inputSpans {
	idx := inputSpans{byTrace: map[string][]span{}}
	for _, in := range inputs {
		for _, s := range spansOf(in.Payload.Traces) {
			s.seq = in.Seq
			s.ref = fmt.Sprintf("%d:%s", in.Seq, s.ref)
			id := s.span.TraceID().String()
			if _, ok := idx.byTrace[id]; !ok {
				idx.order = append(idx.order, id)
			}
			idx.byTrace[id] = append(idx.byTrace[id], s)
		}
	}
	return idx
}

func spansOf(td ptrace.Traces) []span {
	var out []span
	for i, rs := range td.ResourceSpans().All() {
		for j, ss := range rs.ScopeSpans().All() {
			for k, s := range ss.Spans().All() {
				out = append(out, span{ref: fmt.Sprintf("resource[%d].scope[%d].span[%d]", i, j, k), span: s, resource: rs.Resource()})
			}
		}
	}
	return out
}

// traceName is the root span's name, or else the earliest span's.
func traceName(spans []span) string {
	if len(spans) == 0 {
		return ""
	}
	for _, s := range spans {
		if s.span.ParentSpanID().IsEmpty() {
			return s.span.Name()
		}
	}
	return slices.MinFunc(spans, func(a, b span) int { return cmp.Compare(a.span.StartTimestamp(), b.span.StartTimestamp()) }).span.Name()
}

// duration is measured as the latency policy measures it: from the earliest
// start to the latest end.
func duration(spans []span) string {
	if len(spans) == 0 {
		return ""
	}
	var lo, hi pcommon.Timestamp
	for _, s := range spans {
		if lo == 0 || s.span.StartTimestamp() < lo {
			lo = s.span.StartTimestamp()
		}
		if s.span.EndTimestamp() > hi {
			hi = s.span.EndTimestamp()
		}
	}
	return time.Duration(hi - lo).String()
}

func inputSeqs(spans []span) []int {
	seqs := []int{}
	for _, s := range spans {
		if !slices.Contains(seqs, s.seq) {
			seqs = append(seqs, s.seq)
		}
	}
	return seqs
}
