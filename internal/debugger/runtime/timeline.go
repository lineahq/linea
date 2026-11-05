package runtime

import (
	"context"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/tailsamplingprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/topology"
)

var tailSamplingType = component.MustNewType("tail_sampling")

// decisionTick is the tail_sampling processor's decision ticker period at
// v0.162.0 (tickerFrequency, a fixed time.Second).
const decisionTick = time.Second

// settleMargin covers the processor's decision pass and delivery after the
// last tick.
const settleMargin = 500 * time.Millisecond

// settleFor returns the wait after the last input. An explicit wait is
// used as declared. Otherwise components whose output a shutdown would
// lose get the time they need: a tail_sampling processor with
// drop_pending_traces_on_shutdown drops every trace not yet decided, and
// decides a trace on its ticker after decision_wait. Without that option
// its shutdown decides the pending traces, so no wait is needed.
func settleFor(cfg *otelcol.Config, explicit time.Duration) (time.Duration, string) {
	if explicit > 0 {
		return explicit, "execution.settle"
	}
	var wait time.Duration
	var refs []string
	for _, id := range usedProcessorIDs(cfg, tailSamplingType) {
		c, ok := cfg.Processors[id].(*tailsamplingprocessor.Config)
		if !ok || !c.DropPendingTracesOnShutdown {
			continue
		}
		// The processor keeps max(1, decision_wait in seconds) batches and
		// decides the oldest on each tick.
		batches := time.Duration(math.Max(1, c.DecisionWait.Seconds())) * decisionTick
		wait = max(wait, batches+decisionTick+settleMargin)
		refs = append(refs, topology.Ref(topology.KindProcessor, id))
	}
	if wait == 0 {
		return 0, ""
	}
	return wait, fmt.Sprintf("%s: traces still pending at shutdown are dropped (drop_pending_traces_on_shutdown), so the run waits until decision_wait has passed for the last input", strings.Join(refs, ", "))
}

// timeNote explains, for the topology, how timer-driven processors behave
// in a run.
func timeNote(cfg *otelcol.Config, id component.ID) string {
	if id.Type() != tailSamplingType {
		return ""
	}
	c, ok := cfg.Processors[id].(*tailsamplingprocessor.Config)
	if !ok {
		return ""
	}
	if c.DropPendingTracesOnShutdown {
		return fmt.Sprintf("decides traces on its ticker after decision_wait (%v); the run waits for it before shutting down (drop_pending_traces_on_shutdown)", c.DecisionWait)
	}
	return fmt.Sprintf("traces still pending when the run ends are decided at shutdown rather than after decision_wait (%v); declare execution.settle to let the ticker decide them", c.DecisionWait)
}

// checkWaits refuses a run whose declared waits cannot finish before ctx's
// deadline: it would end partial after waiting in vain.
func checkWaits(ctx context.Context, targets []target, settle time.Duration, reason string) error {
	total := settle
	for _, t := range targets {
		total += t.after
	}
	deadline, ok := ctx.Deadline()
	if !ok || total == 0 {
		return nil
	}
	if left := time.Until(deadline); left <= total {
		what := "inputs' after and the settle wait"
		if reason != "" && reason != "execution.settle" {
			what += " (" + reason + ")"
		}
		return fmt.Errorf("the run waits %v (%s), but its timeout leaves %v; raise execution.timeout", total, what, left.Round(time.Millisecond))
	}
	return nil
}

// sleep waits d or until ctx ends, and returns ctx's error if it ended.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// usedProcessorIDs returns the processor IDs of type typ that a pipeline
// uses, sorted, once each.
func usedProcessorIDs(cfg *otelcol.Config, typ component.Type) []component.ID {
	var out []component.ID
	for _, p := range cfg.Service.Pipelines {
		for _, id := range p.Processors {
			if id.Type() == typ && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	slices.SortFunc(out, func(a, b component.ID) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func settleResult(d time.Duration, reason string) *runresult.Settle {
	if d <= 0 {
		return nil
	}
	return &runresult.Settle{Duration: d.String(), Reason: reason}
}

// nowCall matches the OTTL Now() converter.
var nowCall = regexp.MustCompile(`\bNow\(\)`)

// WallClockReads finds the OTTL expressions that call Now() in the
// processors and connectors the pipelines use (transform and filter
// statements, routing and count conditions, tail_sampling OTTL policies).
// Receivers and exporters are replaced in a run, so they are not searched.
func WallClockReads(cfg *otelcol.Config) []runresult.ClockRead {
	var out []runresult.ClockRead
	search := func(ref string, c component.Config) {
		conf := confmap.New()
		if err := conf.Marshal(c); err != nil {
			return
		}
		var found []string
		walkStrings(conf.ToStringMap(), func(s string) {
			if nowCall.MatchString(s) && !slices.Contains(found, s) {
				found = append(found, s)
			}
		})
		slices.Sort(found)
		for _, e := range found {
			out = append(out, runresult.ClockRead{Component: ref, Expression: e})
		}
	}
	var processors []component.ID
	for _, p := range cfg.Service.Pipelines {
		for _, id := range p.Processors {
			if !slices.Contains(processors, id) {
				processors = append(processors, id)
			}
		}
	}
	slices.SortFunc(processors, func(a, b component.ID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range processors {
		search(topology.Ref(topology.KindProcessor, id), cfg.Processors[id])
	}
	connectors := slices.SortedFunc(maps.Keys(cfg.Connectors), func(a, b component.ID) int { return strings.Compare(a.String(), b.String()) })
	for _, id := range connectors {
		search(topology.Ref(topology.KindConnector, id), cfg.Connectors[id])
	}
	return out
}

// walkStrings calls f with every string in v, a decoded configuration.
func walkStrings(v any, f func(string)) {
	switch v := v.(type) {
	case string:
		f(v)
	case map[string]any:
		for _, e := range v {
			walkStrings(e, f)
		}
	case []any:
		for _, e := range v {
			walkStrings(e, f)
		}
	}
}
