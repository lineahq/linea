package recommend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// filterRun is every explained request of one filter processor, in one
// pipeline or, for rules about its configuration, in every pipeline that
// uses it (pipeline is then set only when there is one).
type filterRun struct {
	processor, pipeline, signal string
	pipelines                   []string
	requests                    []*analysis.Filtering
	records, dropped            int
	failed                      bool
}

func (r filterRun) verified() []*analysis.Filtering {
	var out []*analysis.Filtering
	for _, f := range r.requests {
		if f.Verification == analysis.FilterVerified {
			out = append(out, f)
		}
	}
	return out
}

func filterRuns(fs []*analysis.Filtering, perPipeline bool) []filterRun {
	var out []filterRun
	index := map[[2]string]int{}
	for _, f := range fs {
		k := [2]string{f.Processor, ""}
		if perPipeline {
			k[1] = f.Pipeline
		}
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, filterRun{processor: f.Processor, signal: f.Signal})
		}
		r := &out[i]
		if !slices.Contains(r.pipelines, f.Pipeline) {
			r.pipelines = append(r.pipelines, f.Pipeline)
		}
		r.pipeline = ""
		if len(r.pipelines) == 1 {
			r.pipeline = f.Pipeline
		}
		r.requests = append(r.requests, f)
		r.records += f.Records
		r.dropped += f.Dropped
		r.failed = r.failed || f.Error != ""
	}
	return out
}

func requestsCoverage(n int, records int, signal string) string {
	return fmt.Sprintf("Observed on %s in %s; another input may hold items it removes.", plural(records, itemNoun(signal)), plural(n, "request"))
}

// filterNoEffect: a filter processor that removed nothing in the whole run.
// The input held nothing its conditions match, so they are untested.
func filterNoEffect(in Input) []Recommendation {
	var out []Recommendation
	for _, r := range filterRuns(in.Filtering, false) {
		if r.dropped > 0 || r.records == 0 || r.failed {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindUntested, Confidence: ConfidenceObserved, Pipeline: r.pipeline,
			Components: []string{processorRef(r.processor)},
			Summary: fmt.Sprintf("None of the %s %s received in %s matched any of its conditions, so this input does not test them. To test them, add %ss it should drop to the scenario.",
				plural(r.records, itemNoun(r.signal)), r.processor, pipelinesPhrase(r.pipelines), itemNoun(r.signal)),
			Evidence: []Evidence{{SectionFiltering, processorRef(r.processor)}},
			Coverage: []string{requestsCoverage(len(r.requests), r.records, r.signal)},
		})
	}
	return out
}

// conditionNeverMatched: in a filter that removed something, a condition
// that was evaluated and never matched: this input does not test it. Counts come from the requests whose
// evaluation reproduces the processor's output.
func conditionNeverMatched(in Input) []Recommendation {
	var out []Recommendation
	for _, r := range filterRuns(in.Filtering, false) {
		verified := r.verified()
		if r.dropped == 0 || len(verified) == 0 {
			continue
		}
		type total struct {
			c                          analysis.FilterCondition
			evaluated, matched, errors int
		}
		var totals []*total
		byIndex := map[int]*total{}
		for _, f := range verified {
			for _, c := range f.Conditions {
				t, ok := byIndex[c.Index]
				if !ok {
					t = &total{c: c}
					byIndex[c.Index] = t
					totals = append(totals, t)
				}
				t.evaluated += c.Evaluated
				t.matched += c.Matched
				t.errors += c.Errors
			}
		}
		records := 0
		for _, f := range verified {
			records += f.Records
		}
		coverage := []string{requestsCoverage(len(verified), records, r.signal)}
		if n := len(r.requests) - len(verified); n > 0 {
			coverage = append(coverage, fmt.Sprintf("Not counted: %s whose evaluation did not reproduce the output.", plural(n, "request")))
		}
		for _, t := range totals {
			if t.evaluated == 0 || t.matched > 0 || t.errors > 0 {
				continue
			}
			ref := fmt.Sprintf("%s#condition[%d]", processorRef(r.processor), t.c.Index)
			out = append(out, Recommendation{
				Kind: KindUntested, Confidence: ConfidenceObserved, Pipeline: r.pipeline,
				Components: []string{processorRef(r.processor)}, Subject: t.c.Condition,
				Summary: fmt.Sprintf("Condition %s of %s matched none of the %s it was evaluated on; the other conditions dropped all %d dropped %s. To test it, add a %s it should drop to the scenario.",
					t.c.Source, r.processor, plural(t.evaluated, itemNoun(r.signal)), r.dropped, nounFor(r.dropped, itemNoun(r.signal)), itemNoun(r.signal)),
				Evidence: []Evidence{{SectionFiltering, ref}},
				Coverage: slices.Clone(coverage),
			})
		}
	}
	return out
}

// filterAfterBatch: a filter after batch drops records the batch processor
// already buffered and sent.
func filterAfterBatch(in Input) []Recommendation {
	var out []Recommendation
	for _, r := range filterRuns(in.Filtering, true) {
		if r.dropped == 0 {
			continue
		}
		p, ok := pipeline(in.Topology, r.pipeline)
		if !ok {
			continue
		}
		ids := pipelineIDs(p)
		at := slices.Index(ids, r.processor)
		batch := -1
		for i := range at {
			if componentType(ids[i]) == "batch" {
				batch = i
				break
			}
		}
		if at < 0 || batch < 0 {
			continue
		}
		order := slices.Delete(slices.Clone(ids), at, at+1)
		order = slices.Insert(order, batch, r.processor)
		var blockers []string
		if between := ids[batch+1 : at]; len(between) > 0 {
			pl, ok := placementFor(in, r.processor, r.pipeline)
			switch {
			case !ok:
				blockers = append(blockers, fmt.Sprintf("Not checked: the filter would run before %s; whether its conditions read what they set is not verified on this input.", strings.Join(between, ", ")))
			case pl.movable() < len(between):
				blockers = append(blockers, pl.blocker())
			}
		}
		out = append(out, Recommendation{
			Kind: KindOrdering, Confidence: ConfidenceObserved, Pipeline: r.pipeline,
			Components: []string{processorRef(ids[batch]), processorRef(r.processor)},
			Summary: fmt.Sprintf("%s runs after %s in pipeline %s and dropped %s that %s had already buffered and sent. Placed before it, they are dropped before they are batched.",
				r.processor, ids[batch], r.pipeline, plural(r.dropped, itemNoun(r.signal)), ids[batch]),
			Evidence: []Evidence{{SectionTopology, "pipeline/" + r.pipeline}, {SectionFiltering, processorRef(r.processor)}},
			Benefit:  &Benefit{Metric: MetricRecordsNotProcessed, Count: r.dropped},
			Change:   &ConfigChange{Op: OpSwap, Processors: order},
			Blockers: blockers,
		})
	}
	return out
}

// nounFor is noun pluralized for n, without the number.
func nounFor(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// pipelinesPhrase is "pipeline logs" or "pipelines logs/a, logs/b".
func pipelinesPhrase(pipelines []string) string {
	if len(pipelines) == 1 {
		return "pipeline " + pipelines[0]
	}
	return "pipelines " + strings.Join(pipelines, ", ")
}
