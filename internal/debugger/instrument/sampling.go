package instrument

import (
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Decision is a processor's sampling decision for one trace.
type Decision struct {
	TraceID string `json:"trace_id"`
	// Outcome is sampled, not_sampled or dropped (a drop policy matched).
	Outcome string `json:"outcome"`
	// Policy is the policy that sampled or dropped the trace; empty when
	// no policy sampled it.
	Policy string `json:"policy,omitempty"`
}

// Decision outcomes.
const (
	DecisionSampled    = "sampled"
	DecisionNotSampled = "not_sampled"
	DecisionDropped    = "dropped"
)

// DecisionHook returns a processor's cfg with in-code hooks that call record
// for every sampling decision, with the spans the decision evaluated (none
// for a repeated decision), or cfg unchanged when the processor makes no
// sampling decisions. The hooks must leave the processor's behavior
// unchanged. The executor supplies the hook (see SetDecisionHook), so this
// package links no component implementation.
type DecisionHook func(cfg component.Config, record func(Decision, ptrace.Traces)) component.Config

// SetDecisionHook registers the hook that wrapped trace processors apply to
// their configuration. Without one, no decision events are recorded.
func (r *Recorder) SetDecisionHook(h DecisionHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions = h
}

// withDecisionHooks applies the recorder's decision hook to cfg, recording
// every decision as a stage.decision event whose snapshot holds exactly the
// spans the policies evaluated. A repeated decision has no snapshot.
func withDecisionHooks(cfg component.Config, s *stage) component.Config {
	s.rec.mu.Lock()
	h := s.rec.decisions
	s.rec.mu.Unlock()
	if h == nil {
		return cfg
	}
	return h(cfg, func(d Decision, evaluated ptrace.Traces) {
		e := s.event(KindDecision)
		e.Decision = &d
		if evaluated.SpanCount() == 0 {
			s.rec.Record(e)
			return
		}
		s.rec.RecordTelemetry(e, telemetry.FromTraces(evaluated))
	})
}
