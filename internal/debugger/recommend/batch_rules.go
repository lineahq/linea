package recommend

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// tinyBatchers: a batch processor whose metadata_keys split its arrivals
// into batchers that sent smaller batches than one batcher would have sent
// on the same arrivals. No threshold decides it: the one-batcher figure is a
// replay of this run's own arrivals through the processor's settings
// (oneBatcher), the way the batching analysis replays the timer. The
// summary states both figures and suggests no change: metadata_keys usually
// exist for a reason, such as keeping one tenant's requests apart.
func tinyBatchers(in Input) []Recommendation {
	var out []Recommendation
	for _, b := range in.Batching {
		if len(b.Batchers) < 2 {
			continue
		}
		timeout, err := time.ParseDuration(b.Settings.Timeout)
		if err != nil {
			continue
		}
		sent := make([]batcherSends, len(b.Batchers))
		for _, f := range b.Flushes {
			if f.Batcher >= 0 && f.Batcher < len(sent) {
				sent[f.Batcher].sends++
				sent[f.Batcher].items += f.Items
			}
		}
		compared := func(i int) bool { return !b.Batchers[i].Unknown && !b.Batchers[i].Refused }
		var knownBatchers, knownRequests, unknownRequests, refusedRequests, unsent int
		var splitSends, splitItems int
		for i, bt := range b.Batchers {
			switch {
			case bt.Unknown:
				unknownRequests += bt.Arrivals
			case bt.Refused:
				refusedRequests += bt.Arrivals
			default:
				knownBatchers++
				knownRequests += bt.Arrivals
				splitSends += sent[i].sends
				splitItems += sent[i].items
				unsent += bt.Items - sent[i].items
			}
		}
		if knownBatchers < 2 || splitSends == 0 {
			continue
		}
		one, ok := oneBatcher(b, timeout, compared, unsent == 0)
		if !ok || one.sends == 0 {
			continue
		}
		// splitItems/splitSends < one.items/one.sends, without division.
		if splitItems*one.sends >= one.items*splitSends {
			continue
		}

		type below struct {
			index int
			sends batcherSends
		}
		var smaller []below
		for i := range b.Batchers {
			s := sent[i]
			if compared(i) && s.sends > 0 && s.items*one.sends < one.items*s.sends {
				smaller = append(smaller, below{i, s})
			}
		}
		slices.SortStableFunc(smaller, func(x, y below) int {
			return cmp.Compare(x.sends.average(), y.sends.average())
		})
		noun := itemNoun(b.Signal)
		const listed = 5
		var parts []string
		var evidence = []Evidence{{SectionBatching, processorRef(b.Processor)}}
		for _, s := range smaller[:min(len(smaller), listed)] {
			parts = append(parts, fmt.Sprintf("batcher %d %s", s.index, number(s.sends.average())))
			evidence = append(evidence, Evidence{SectionBatching, fmt.Sprintf("%s#batcher[%d]", processorRef(b.Processor), s.index)})
		}
		list := strings.Join(parts, ", ")
		if n := len(smaller) - listed; n > 0 {
			list += fmt.Sprintf(" and %d more", n)
		}
		summary := fmt.Sprintf("%s splits the arrivals of pipeline %s by metadata_keys %s into %d batchers, which sent %s %ss per send on average (%s); one batcher holding the same %s would have sent %s %ss per send (%s). Below that, per send: %s. Each batcher fills send_batch_size %d, or waits out its own %s timeout, on its share of the arrivals alone. metadata_keys usually exist for a reason, such as keeping one tenant's requests apart, so no change is suggested.",
			b.Processor, b.Pipeline, strings.Join(b.Settings.MetadataKeys, ", "), knownBatchers,
			number(float64(splitItems)/float64(splitSends)), noun, plural(splitSends, "send"),
			plural(knownRequests, "request"), number(float64(one.items)/float64(one.sends)), noun, plural(one.sends, "send"),
			list, b.Settings.SendBatchSize, b.Settings.Timeout)

		coverage := []string{
			fmt.Sprintf("Observed on %s in %s; production traffic and its metadata mix decide how full each batcher gets.", plural(knownRequests, "request"), plural(knownBatchers, "batcher")),
		}
		if unknownRequests > 0 {
			coverage = append(coverage, fmt.Sprintf("Not compared: %s in a batcher of unknown metadata (the headers could not be traced to scenario inputs).", plural(unknownRequests, "request")))
		}
		if refusedRequests > 0 {
			coverage = append(coverage, fmt.Sprintf("Not compared: %s refused beyond metadata_cardinality_limit.", plural(refusedRequests, "request")))
		}
		if unsent > 0 {
			coverage = append(coverage, fmt.Sprintf("%s arrived without being sent in this run and are not counted.", plural(unsent, noun)))
		}
		out = append(out, Recommendation{
			Kind: KindInefficiency, Confidence: ConfidenceObserved, Pipeline: b.Pipeline,
			Components: []string{processorRef(b.Processor)},
			Summary:    summary,
			Evidence:   evidence,
			Blockers:   []string{"No change suggested: whether to keep metadata_keys is the user's decision."},
			Coverage:   coverage,
		})
	}
	return out
}

// batcherSends is what one batcher sent in the run.
type batcherSends struct{ sends, items int }

func (s batcherSends) average() float64 {
	if s.sends == 0 {
		return 0
	}
	return float64(s.items) / float64(s.sends)
}

// oneBatcher replays the arrivals of the compared batchers through one
// batcher with the processor's settings: the batchprocessor v0.162.0 loop
// (docs/internals/batching.md). Its timer starts with the processor and
// restarts after every tick and every size send; a tick sends what is
// pending (capped at send_batch_max_size); after a request is appended the
// batcher sends while it holds send_batch_size items, or on every request
// without a timer; what is pending after the last arrival leaves in one
// send when the run completed (finalSend), by a tick or at shutdown. It
// reports false when an arrival has no offset (normalized reports).
func oneBatcher(b *analysis.Batching, timeout time.Duration, compared func(batcher int) bool, finalSend bool) (batcherSends, bool) {
	size, maxSize := b.Settings.SendBatchSize, b.Settings.SendBatchMaxSize
	tick := float64(timeout) / float64(time.Millisecond)
	timed := timeout > 0 && size > 0
	var out batcherSends
	pending := 0
	send := func() {
		n := pending
		if maxSize > 0 && n > maxSize {
			n = maxSize
		}
		pending -= n
		out.sends++
		out.items += n
	}
	next := tick
	for _, a := range b.Arrivals {
		if a.Batcher < 0 || a.Batcher >= len(b.Batchers) || !compared(a.Batcher) {
			continue
		}
		if a.OffsetMS == nil {
			return batcherSends{}, false
		}
		at := *a.OffsetMS
		for timed && next < at {
			if pending > 0 {
				send()
				next += tick
				continue
			}
			// Idle ticks send nothing: skip to the first one at or after
			// the arrival.
			next += (math.Floor((at-next)/tick) + 1) * tick
		}
		pending += a.Items
		sentBySize := false
		for pending > 0 && (!timed || pending >= size) {
			send()
			sentBySize = true
		}
		if timed && sentBySize {
			next = at + tick
		}
	}
	for finalSend && pending > 0 {
		send()
	}
	return out, true
}

// groupingAfterBatch: groupbyattrs before a batch processor regroups one
// request at a time, while the batch processor appends requests without
// merging equal resources. Run after it, groupbyattrs sees each send whole
// and merges the equal resources of the requests the batch joined.
//
// Observed when the run shows it exactly: groupbyattrs emits blocks without
// schema URLs and without the keys on its records, so the resources of a
// send of the batch processor right behind it are the blocks it would merge,
// and the blocks left after merging are the send's distinct resources
// (BatchFlush.DistinctResources). The count needs the two processors to be
// adjacent, every arrival to be sent and every send's counts recorded;
// otherwise the recommendation is static and says what the run cannot show.
// A run whose sends hold no repeated resource gives none: grouping before
// batching merged as much as grouping after it would.
func groupingAfterBatch(in Input) []Recommendation {
	var out []Recommendation
	for _, p := range in.Topology.Pipelines {
		ids := pipelineIDs(p)
		for at, id := range ids {
			if componentType(id) != "groupbyattrs" {
				continue
			}
			rel := slices.IndexFunc(ids[at+1:], func(id string) bool { return componentType(id) == "batch" })
			if rel < 0 {
				continue
			}
			batch := at + 1 + rel
			order := slices.Delete(slices.Clone(ids), at, at+1)
			// The batch processor moved up by one when groupbyattrs left.
			order = slices.Insert(order, batch, id)
			var blockers []string
			if between := ids[at+1 : batch]; len(between) > 0 {
				blockers = append(blockers, fmt.Sprintf("%s would then run before grouping, on requests that still hold the grouping keys on their records and whose resources are not merged; whether they read those is not verified.", strings.Join(between, ", ")))
			}
			g := groupingFor(in, id, p.ID)
			b := batchingFor(in, ids[batch], p.ID)
			gain, why := groupingGain(g, b, batch == at+1)
			rec := Recommendation{
				Kind: KindOrdering, Pipeline: p.ID,
				Components: []string{processorRef(id), processorRef(ids[batch])},
				Evidence:   []Evidence{{SectionTopology, "pipeline/" + p.ID}},
				Change:     &ConfigChange{Op: OpSwap, Processors: order},
				Blockers:   blockers,
			}
			if why == "" {
				if gain.resources <= gain.distinct {
					continue
				}
				rec.Confidence = ConfidenceObserved
				rec.Evidence = append(rec.Evidence, Evidence{SectionGrouping, processorRef(id)}, Evidence{SectionBatching, processorRef(ids[batch])})
				rec.Summary = fmt.Sprintf("%s runs before %s in pipeline %s and regroups one request at a time: it received %s in %s and emitted %s. %s appended those requests without merging equal resources and sent %s in %s, %d of them distinct. Run after %s, %s sees each send whole and merges equal resources: %s instead of %s.",
					id, ids[batch], p.ID, plural(gain.received, "resource block"), plural(gain.requests, "request"), plural(gain.emitted, "resource block"),
					ids[batch], plural(gain.resources, "resource block"), plural(gain.sends, "send"), gain.distinct,
					ids[batch], id, plural(gain.distinct, "resource block"), plural(gain.resources, "block"))
				rec.Benefit = &Benefit{MetricResourcesMerged, gain.resources - gain.distinct}
				rec.Coverage = []string{fmt.Sprintf("Observed on %s; another input may repeat fewer or more resources within a batch.", plural(gain.requests, "request"))}
			} else {
				rec.Confidence = ConfidenceStatic
				rec.Summary = fmt.Sprintf("%s runs before %s in pipeline %s. groupbyattrs regroups one request at a time, while the batch processor appends requests without merging equal resources; run after it, %s sees each send whole and merges the equal resources of the requests the batch joined.",
					id, ids[batch], p.ID, id)
				rec.Coverage = []string{"Not shown by the run: how many resource blocks that merges. " + why}
			}
			out = append(out, rec)
		}
	}
	return out
}

// groupingFor and batchingFor find the evidence of one processor instance.
func groupingFor(in Input, processor, pipeline string) *analysis.Grouping {
	for _, g := range in.Grouping {
		if g.Processor == processor && g.Pipeline == pipeline {
			return g
		}
	}
	return nil
}

func batchingFor(in Input, processor, pipeline string) *analysis.Batching {
	for _, b := range in.Batching {
		if b.Processor == processor && b.Pipeline == pipeline {
			return b
		}
	}
	return nil
}

// resourceGain is the resource blocks of one groupbyattrs processor and the
// batch processor behind it.
type resourceGain struct {
	// received and emitted are groupbyattrs' resource blocks over requests
	// requests.
	received, emitted, requests int
	// resources and distinct are the blocks the batch processor sent and
	// the different ones among them, over sends sends.
	resources, distinct, sends int
}

// groupingGain counts the blocks groupbyattrs would merge after the batch
// processor, or says why the run cannot show it.
func groupingGain(g *analysis.Grouping, b *analysis.Batching, adjacent bool) (resourceGain, string) {
	switch {
	case g == nil && b == nil:
		return resourceGain{}, "Read from the configuration alone."
	case g == nil:
		return resourceGain{}, "The run holds no grouping evidence for it."
	case b == nil:
		return resourceGain{}, "The run holds no batching evidence for the batch processor."
	case !adjacent:
		return resourceGain{}, "Processors between them may change resources or record attributes, which the sends do not separate from grouping."
	case len(b.Flushes) == 0:
		return resourceGain{}, "The batch processor sent nothing in this run."
	}
	var gain resourceGain
	gain.requests = len(g.Requests)
	for _, r := range g.Requests {
		gain.received += len(r.Before)
		if r.Output != nil {
			gain.emitted += len(r.After)
		}
	}
	arrived, sent := 0, 0
	for _, a := range b.Arrivals {
		arrived += a.Items
	}
	for _, f := range b.Flushes {
		sent += f.Items
		if f.Items > 0 && (f.Resources == 0 || f.DistinctResources == 0) {
			return resourceGain{}, "The resources of the batch processor's sends were not recorded."
		}
		gain.resources += f.Resources
		gain.distinct += f.DistinctResources
	}
	if sent < arrived {
		return resourceGain{}, fmt.Sprintf("%s arrived at the batch processor without being sent in this run.", plural(arrived-sent, "item"))
	}
	gain.sends = len(b.Flushes)
	return gain, ""
}
