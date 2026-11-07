package recommend

import (
	"fmt"
	"slices"
	"strings"
)

// Placement is what a run shows about moving one filter processor earlier
// in one pipeline. The engine computes it (it evaluates the filter's
// conditions on the inputs of the processors before it); the rules only
// read it.
type Placement struct {
	Filter, Pipeline, Signal string
	// Requests counts the filter's requests whose replay is verified and
	// were compared; Skipped those left out because it is not.
	Requests, Skipped int
	// Records counts the records compared and Dropped those the filter
	// removed in the compared requests.
	Records, Dropped int
	// Checks are the processors before the filter, nearest first, up to and
	// including the first one it cannot move before.
	Checks []PlacementCheck
}

// PlacementCheck is the result for one processor before the filter.
type PlacementCheck struct {
	Processor string
	// Blocker says why the filter cannot move before Processor; empty when
	// the filter's verdict on every compared record is the same at
	// Processor's input as at its own.
	Blocker string
}

// movable is how many processors the filter can move before: the leading
// checks without a blocker.
func (p Placement) movable() int {
	n := 0
	for _, c := range p.Checks {
		if c.Blocker != "" {
			break
		}
		n++
	}
	return n
}

// blocker is the first check's blocker, "" when none failed.
func (p Placement) blocker() string {
	for _, c := range p.Checks {
		if c.Blocker != "" {
			return c.Blocker
		}
	}
	return ""
}

func placementFor(in Input, filter, pipeline string) (Placement, bool) {
	for _, p := range in.Placements {
		if p.Filter == filter && p.Pipeline == pipeline {
			return p, true
		}
	}
	return Placement{}, false
}

// filterMoveEarlier: a filter whose verdict on every record is the same
// before the processors in front of it, so placed before them, the records
// it drops skip them.
func filterMoveEarlier(in Input) []Recommendation {
	var out []Recommendation
	for _, pl := range in.Placements {
		n := pl.movable()
		if n == 0 || pl.Dropped == 0 {
			continue
		}
		p, ok := pipeline(in.Topology, pl.Pipeline)
		if !ok {
			continue
		}
		ids := pipelineIDs(p)
		at := slices.Index(ids, pl.Filter)
		if at < n || hasBatchBefore(ids, at) {
			continue // filter.after_batch covers a filter behind a batch.
		}
		skipped := make([]string, n)
		for i := range n {
			skipped[i] = ids[at-1-i]
		}
		slices.Reverse(skipped)
		order := slices.Delete(slices.Clone(ids), at, at+1)
		order = slices.Insert(order, at-n, pl.Filter)
		components := []string{processorRef(pl.Filter)}
		evidence := []Evidence{{SectionTopology, "pipeline/" + pl.Pipeline}, {SectionFiltering, processorRef(pl.Filter)}}
		for _, s := range skipped {
			components = append(components, processorRef(s))
			evidence = append(evidence, Evidence{SectionStages, processorRef(s)})
		}
		var blockers []string
		if b := pl.blocker(); b != "" {
			blockers = append(blockers, b)
		}
		out = append(out, Recommendation{
			Kind: KindOrdering, Confidence: ConfidenceObserved, Pipeline: pl.Pipeline,
			Components: components,
			Summary: fmt.Sprintf("%s dropped %s in pipeline %s after %s processed %s. Its conditions give every %s the same verdict before %s, so placed there, %s %s %s %s.",
				pl.Filter, plural(pl.Dropped, itemNoun(pl.Signal)), pl.Pipeline, strings.Join(skipped, ", "), pick(pl.Dropped, "it", "them"),
				itemNoun(pl.Signal), skipped[0], pick(pl.Dropped, "that", "those"), nounFor(pl.Dropped, itemNoun(pl.Signal)),
				pick(pl.Dropped, "skips", "skip"), plural(n, "processor")),
			Evidence: evidence,
			Benefit:  &Benefit{Metric: MetricRecordsNotProcessed, Count: pl.Dropped * n},
			Change:   &ConfigChange{Op: OpSwap, Processors: order},
			Blockers: blockers,
			Coverage: placementCoverage(pl),
		})
	}
	return out
}

func placementCoverage(pl Placement) []string {
	out := []string{fmt.Sprintf("Same verdict observed on %s in %s; another input may hold records whose verdict depends on what the skipped processors change.",
		plural(pl.Records, itemNoun(pl.Signal)), plural(pl.Requests, "request"))}
	if pl.Skipped > 0 {
		out = append(out, fmt.Sprintf("Not compared: %s whose filter evaluation did not reproduce the output.", plural(pl.Skipped, "request")))
	}
	return out
}

func hasBatchBefore(ids []string, at int) bool {
	for _, id := range ids[:max(at, 0)] {
		if componentType(id) == "batch" {
			return true
		}
	}
	return false
}

// pick is one when n is 1 and many otherwise.
func pick(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
