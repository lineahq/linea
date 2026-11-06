// Package filterreplay explains filter processors: which configured
// condition removed each resource, scope, record or span event of a
// recorded stage input. It parses the conditions with the processor's own
// function sets and parser collection, evaluates them in the processor's
// order on a copy of the input (filterprocessor v0.162.0,
// internal/condition), and checks the result against what the processor
// actually emitted. The recorded output is the observation; drops are
// attributed to conditions only when the evaluation reproduces it exactly.
// See docs/internals/filtering.md.
package filterreplay

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// The explanation is an optional feature: the generated registry imports
// this package only when the manifest has the filter processor.
func init() { features.ProvideFilter(Explain) }

// Explain explains one input of a filter processor. output is the
// processor's correlated emission (nil when it emitted nothing) and
// stageError the error it returned for the input, if any.
func Explain(ctx context.Context, processor string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload, stageError string) (*analysis.Filtering, error) {
	c, ok := cfg.(*filterprocessor.Config)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected configuration %T", processor, cfg)
	}
	out := &analysis.Filtering{
		Processor:  processor,
		Signal:     input.Signal.String(),
		Input:      inputSeq,
		Conditions: []analysis.FilterCondition{},
		Records:    len(telemetry.Records(input)),
		Error:      stageError,
	}
	out.Dropped = out.Records
	if output != nil {
		o := outputSeq
		out.Output = &o
		out.Dropped -= len(telemetry.Records(*output))
	}

	p, err := newPlan(c, input.Signal)
	if err != nil {
		out.Verification = analysis.FilterUnsupported
		out.Notes = append(out.Notes, "the conditions could not be parsed: "+err.Error())
		return out, nil
	}
	out.Conditions = p.conditions
	for i := range out.Conditions {
		out.Conditions[i].Index = i
	}
	if p.unsupported != "" {
		out.Verification = analysis.FilterUnsupported
		out.Notes = append(out.Notes, p.unsupported)
		return out, nil
	}

	got := input.Clone()
	e := &evaluation{ctx: ctx, conditions: out.Conditions}
	evalErr := e.run(p.groups, got)

	if why := verify(p, got, evalErr, output, stageError); why != "" {
		out.Verification = analysis.FilterDiffers
		out.Notes = append(out.Notes, why)
		return out, nil
	}
	out.Verification = analysis.FilterVerified
	out.Drops = e.drops
	if evalErr != nil {
		out.Notes = append(out.Notes, "a condition failed under error_mode propagate: the processor returned the error and the pipeline dropped the whole request")
	}
	return out, nil
}

// run applies the groups in turn, as the processor runs its consumers, and
// returns the condition errors it would return.
func (e *evaluation) run(groups []group, p telemetry.Payload) error {
	pos := tree(p)
	var errs error
	for i := range groups {
		g := &groups[i]
		var err error
		switch p.Signal {
		case pipeline.SignalLogs:
			err = e.logs(g, p.Logs, &pos)
		case pipeline.SignalTraces:
			err = e.traces(g, p.Traces, &pos)
		case pipeline.SignalMetrics:
			err = e.metrics(g, p.Metrics, &pos)
		}
		errs = errors.Join(errs, err)
	}
	return errs
}

// verify compares the evaluation with what the processor did; it returns
// "" when they agree.
func verify(p *plan, got telemetry.Payload, evalErr error, output *telemetry.Payload, stageError string) string {
	switch {
	case evalErr != nil:
		if output != nil || stageError == "" {
			return "the evaluation failed under error_mode propagate, but the processor passed the input on: " + evalErr.Error()
		}
		return ""
	case stageError != "":
		return "the processor returned an error for this input, but the evaluation did not: " + stageError
	case len(p.groups) > 0 && empty(got):
		if output != nil {
			return "the evaluation removed every record, but the processor emitted some"
		}
		return ""
	case output == nil:
		return "the evaluation kept records, but the processor emitted nothing"
	}
	want := telemetry.ContextFingerprints(got)
	have := telemetry.ContextFingerprints(*output)
	slices.Sort(want)
	slices.Sort(have)
	if !slices.Equal(want, have) {
		return fmt.Sprintf("the evaluation kept %d records, the processor emitted %d that differ", len(want), len(have))
	}
	return ""
}

func empty(p telemetry.Payload) bool {
	switch p.Signal {
	case pipeline.SignalLogs:
		return p.Logs.ResourceLogs().Len() == 0
	case pipeline.SignalTraces:
		return p.Traces.ResourceSpans().Len() == 0
	case pipeline.SignalMetrics:
		return p.Metrics.ResourceMetrics().Len() == 0
	}
	return false
}

// tree numbers every item of the payload by its input position.
func tree(p telemetry.Payload) []node {
	var out []node
	switch p.Signal {
	case pipeline.SignalLogs:
		out = nodes(p.Logs.ResourceLogs().Len())
		for i, rl := range p.Logs.ResourceLogs().All() {
			out[i].kids = nodes(rl.ScopeLogs().Len())
			for j, sl := range rl.ScopeLogs().All() {
				out[i].kids[j].kids = nodes(sl.LogRecords().Len())
			}
		}
	case pipeline.SignalTraces:
		out = nodes(p.Traces.ResourceSpans().Len())
		for i, rs := range p.Traces.ResourceSpans().All() {
			out[i].kids = nodes(rs.ScopeSpans().Len())
			for j, ss := range rs.ScopeSpans().All() {
				out[i].kids[j].kids = nodes(ss.Spans().Len())
				for k, s := range ss.Spans().All() {
					out[i].kids[j].kids[k].kids = nodes(s.Events().Len())
				}
			}
		}
	case pipeline.SignalMetrics:
		out = nodes(p.Metrics.ResourceMetrics().Len())
		for i, rm := range p.Metrics.ResourceMetrics().All() {
			out[i].kids = nodes(rm.ScopeMetrics().Len())
			for j, sm := range rm.ScopeMetrics().All() {
				out[i].kids[j].kids = nodes(sm.Metrics().Len())
				for k, m := range sm.Metrics().All() {
					out[i].kids[j].kids[k].kids = nodes(pointCount(m))
				}
			}
		}
	}
	return out
}
