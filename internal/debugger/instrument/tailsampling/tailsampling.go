// Package tailsampling records the sampling decisions of the tail_sampling
// processor. It is the only instrumentation that links the processor, and
// only the in-process executor imports it.
package tailsampling

import (
	"context"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor/pkg/samplingpolicy"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// Hook is an instrument.DecisionHook for tail_sampling (contrib v0.162.0).
// It adds sampled and not-sampled hooks as in-code processor options
// (Config.Options, not part of the YAML); the processor's behavior is
// unchanged. A span arriving after a not-sampled decision calls the hook
// again with the trace's batches already released: the decision is repeated
// and nothing is evaluated. Other configs are returned as they are.
func Hook(cfg component.Config, record func(instrument.Decision, ptrace.Traces)) component.Config {
	c, ok := cfg.(*tailsamplingprocessor.Config)
	if !ok {
		return cfg
	}
	hook := func(_ context.Context, id pcommon.TraceID, td *tailsamplingprocessor.TraceData) {
		record(instrument.Decision{TraceID: id.String(), Outcome: outcome(td.FinalDecision), Policy: td.PolicyName}, td.ReceivedBatches)
	}
	hooked := *c
	hooked.Options = append(append([]tailsamplingprocessor.Option{}, c.Options...),
		tailsamplingprocessor.WithSampledHooks(hook), tailsamplingprocessor.WithNonSampledHooks(hook))
	return &hooked
}

var _ instrument.DecisionHook = Hook

func outcome(d samplingpolicy.Decision) string {
	switch d {
	case samplingpolicy.Sampled:
		return instrument.DecisionSampled
	case samplingpolicy.Dropped:
		return instrument.DecisionDropped
	case samplingpolicy.NotSampled:
		return instrument.DecisionNotSampled
	}
	return strings.ToLower(d.String())
}
