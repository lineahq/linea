package recommend

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// noEffectExempt are processor types processor.no_effect leaves out:
// buffering and guarding processors pass data on unchanged by design, those
// that enrich from the environment cannot act in a debug run, and the rest
// have rules of their own.
var noEffectExempt = map[string]bool{
	"batch": true, "memory_limiter": true,
	"k8sattributes": true, "resourcedetection": true,
	"filter": true, "transform": true, "groupbyattrs": true, "tail_sampling": true,
}

// processorNoEffect: a processor that changed and dropped no record of the
// run.
func processorNoEffect(in Input) []Recommendation {
	var out []Recommendation
	for _, s := range in.Stages {
		if s.Kind != analysis.KindProcessor || noEffectExempt[s.Type] || s.Emission != analysis.EmissionSynchronous || len(s.Inputs) == 0 {
			continue
		}
		if c, ok := in.Topology.Component(processorRef(s.Component)); !ok || c.Mode != topology.ModeReal {
			continue
		}
		records, unchanged := 0, true
		for _, input := range s.Inputs {
			if input.Outcome != analysis.OutcomeEmitted || input.Diff == nil || !input.Diff.Empty() {
				unchanged = false
				break
			}
			records += input.Diff.Before.Items
		}
		if !unchanged || records == 0 {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: s.Pipeline,
			Components: []string{processorRef(s.Component)},
			Summary: fmt.Sprintf("%s changed none of the %s it received in pipeline %s and dropped none.",
				s.Component, plural(records, itemNoun(s.Signal)), s.Pipeline),
			Evidence: []Evidence{{SectionStages, processorRef(s.Component)}},
			Benefit:  &Benefit{MetricRecordsWithoutEffect, records},
			Coverage: []string{fmt.Sprintf("Observed on %s; another input may hold records it changes.", plural(len(s.Inputs), "request"))},
		})
	}
	return out
}

// groupingNoEffect: a groupbyattrs processor that moved no attribute and
// merged no resource.
func groupingNoEffect(in Input) []Recommendation {
	var out []Recommendation
	for _, g := range in.Grouping {
		records, idle := 0, len(g.Requests) > 0
		for _, r := range g.Requests {
			if r.Output == nil || len(r.Moved) > 0 || r.Grouped > 0 || r.Unfollowed > 0 || len(r.After) != len(r.Before) {
				idle = false
				break
			}
			records += r.Kept
		}
		if !idle || records == 0 {
			continue
		}
		keys := "no keys"
		if len(g.Keys) > 0 {
			keys = "keys " + strings.Join(g.Keys, ", ")
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: g.Pipeline,
			Components: []string{processorRef(g.Processor)},
			Summary: fmt.Sprintf("%s (%s) moved no attribute and merged no resource: none of the %s it received carried its keys, and no two resources were equal.",
				g.Processor, keys, plural(records, itemNoun(g.Signal))),
			Evidence: []Evidence{{SectionGrouping, processorRef(g.Processor)}},
			Benefit:  &Benefit{MetricRecordsWithoutEffect, records},
			Coverage: []string{fmt.Sprintf("Observed on %s; another input may carry the keys.", plural(len(g.Requests), "request"))},
		})
	}
	return out
}

// sizeNeverReached: every send of a batch processor came from its timeout.
// No threshold decides it; the summary says what the input's own rate means
// for each setting.
func sizeNeverReached(in Input) []Recommendation {
	var out []Recommendation
	for _, b := range in.Batching {
		size := b.Settings.SendBatchSize
		if size <= 0 || len(b.Flushes) == 0 {
			continue
		}
		timeouts, most := 0, 0
		bySize := false
		for _, f := range b.Flushes {
			switch f.Trigger {
			case analysis.TriggerSize:
				bySize = true
			case analysis.TriggerTimeout:
				timeouts++
			}
			most = max(most, f.Items)
		}
		if bySize || timeouts == 0 {
			continue
		}
		items := 0
		first, last := math.Inf(1), math.Inf(-1)
		for _, a := range b.Arrivals {
			items += a.Items
			if a.OffsetMS != nil {
				first, last = min(first, *a.OffsetMS), max(last, *a.OffsetMS)
			}
		}
		noun := itemNoun(b.Signal)
		per := float64(items) / float64(len(b.Flushes))
		var summary string
		if span := last - first; span > 0 && items > 0 {
			rate := float64(items) * 1000 / span
			fill := time.Duration(float64(size) / rate * float64(time.Second))
			summary = fmt.Sprintf("At this input's rate of about %s %ss/s, send_batch_size %d fills after %s; the %s timeout sent every batch instead, %s %ss per send on average. A longer timeout makes fuller batches with longer waits; a send_batch_size near the batch you want sends by size at this rate.",
				number(rate), noun, size, roundDuration(fill), b.Settings.Timeout, number(per), noun)
		} else {
			summary = fmt.Sprintf("Every send of %s came from its %s timeout, with at most %s against send_batch_size %d. The input arrived at once, so its rate does not say which setting would decide.",
				b.Processor, b.Settings.Timeout, plural(most, noun), size)
		}
		out = append(out, Recommendation{
			Kind: KindInefficiency, Confidence: ConfidenceObserved, Pipeline: b.Pipeline,
			Components: []string{processorRef(b.Processor)},
			Summary:    summary,
			Evidence:   []Evidence{{SectionBatching, processorRef(b.Processor)}},
			Coverage:   []string{"Rates come from this input; production traffic decides which setting fires."},
		})
	}
	return out
}

// lateSpans: spans that reached tail_sampling after their trace's decision
// followed it without being evaluated.
func lateSpans(in Input) []Recommendation {
	var out []Recommendation
	for _, s := range in.Sampling {
		spans, traces := 0, 0
		var evidence []Evidence
		for _, t := range s.Traces {
			if t.LateSpans == 0 {
				continue
			}
			spans += t.LateSpans
			traces++
			if len(evidence) < 10 {
				evidence = append(evidence, Evidence{SectionSampling, fmt.Sprintf("%s#trace[%s]", processorRef(s.Processor), t.TraceID)})
			}
		}
		if spans == 0 {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindRisk, Confidence: ConfidenceObserved, Pipeline: s.Pipeline,
			Components: []string{processorRef(s.Processor)},
			Summary: fmt.Sprintf("%s of %s reached %s after the trace's decision (decision_wait %s) and followed it without being evaluated. A longer decision_wait evaluates them with their trace.",
				plural(spans, "span"), plural(traces, "trace"), s.Processor, s.DecisionWait),
			Evidence: evidence,
			Change:   &ConfigChange{Op: OpSetOption, Component: s.Processor, Option: "decision_wait"},
		})
	}
	return out
}

// number formats a rate or an average: one decimal under 10.
func number(v float64) string {
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(math.Round(v), 'f', 0, 64)
}

func roundDuration(d time.Duration) time.Duration {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second)
	case d >= time.Second:
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}
