package analysis

import (
	"slices"

	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Sent is a simulated endpoint a record was also sent to: a
// component that runs for real encoded it and delivered it to a client
// endpoint of its configuration. Nothing links the call to the record, so
// it is paired by content, never by object.
type Sent struct {
	// Component is the topology ref of the component that sent it.
	Component string `json:"component"`
	// Endpoint is the client configuration's path in the component's
	// configuration.
	Endpoint string `json:"endpoint"`
	// Seq is the endpoint.received event, and Ref the record's position
	// in its payload.
	Seq int    `json:"seq"`
	Ref string `json:"ref"`
	// Confidence is exact when the record is identical to the record at
	// one of the lineage's hops, id for a span with the lineage's IDs,
	// and high or medium for the content keys stage diffs use.
	Confidence Confidence `json:"confidence"`
}

// pairEndpoints adds to each lineage the endpoint records that are one of
// its hops' records. A received record is paired only when exactly one
// lineage has it; records no lineage or several have are left unpaired.
func (l *lineage) pairEndpoints(events []instrument.Event, lineages []RecordLineage) error {
	type received struct {
		seq   int
		event instrument.Event
		rec   telemetry.Record
		ref   string
	}
	bySignal := map[string][]received{}
	for _, e := range events {
		if e.Kind != instrument.KindEndpointReceived || e.Snapshot == "" {
			continue
		}
		recs, p, err := l.recordsOf(e.Seq)
		if err != nil {
			return err
		}
		for _, r := range recs {
			bySignal[e.Signal] = append(bySignal[e.Signal], received{e.Seq, e, r, r.Ref(p.Signal)})
		}
	}
	if len(bySignal) == 0 {
		return nil
	}
	// The records a lineage passed through, one per hop with a record.
	hopRecords := func(rl *RecordLineage) ([]telemetry.Record, error) {
		var out []telemetry.Record
		for _, h := range rl.Hops {
			if h.Ref == "" {
				continue
			}
			recs, p, err := l.recordsOf(h.Seq)
			if err != nil {
				return nil, err
			}
			if i := slices.IndexFunc(recs, func(r telemetry.Record) bool { return r.Ref(p.Signal) == h.Ref }); i >= 0 {
				out = append(out, recs[i])
			}
		}
		return out, nil
	}
	for signal, recv := range bySignal {
		var candidates []int
		hops := map[int][]telemetry.Record{}
		for i := range lineages {
			if lineages[i].Signal != signal {
				continue
			}
			recs, err := hopRecords(&lineages[i])
			if err != nil {
				return err
			}
			candidates = append(candidates, i)
			hops[i] = recs
		}
		tiers := endpointTiers(signal)
		paired := make([]bool, len(recv))
		for _, t := range tiers {
			// keys of the lineages, and how many received records share
			// each key.
			owners := map[string][]int{}
			for _, i := range candidates {
				seen := map[string]bool{}
				for _, r := range hops[i] {
					if k, ok := t.key(r, 0); ok && !seen[k] {
						seen[k] = true
						owners[k] = append(owners[k], i)
					}
				}
			}
			shared := map[string]int{}
			for j, r := range recv {
				if paired[j] {
					continue
				}
				if k, ok := t.key(r.rec, 0); ok {
					shared[k]++
				}
			}
			for j, r := range recv {
				if paired[j] {
					continue
				}
				k, ok := t.key(r.rec, 0)
				if !ok || len(owners[k]) != 1 || (t.confidence != ConfidenceExact && shared[k] != 1) {
					continue
				}
				paired[j] = true
				rl := &lineages[owners[k][0]]
				rl.Sent = append(rl.Sent, Sent{Component: r.event.Component, Endpoint: r.event.Endpoint,
					Seq: r.seq, Ref: r.ref, Confidence: t.confidence})
			}
		}
	}
	for i := range lineages {
		slices.SortStableFunc(lineages[i].Sent, func(a, b Sent) int { return a.Seq - b.Seq })
	}
	return nil
}

// endpointTiers are the content tiers stage diffs use, without the object
// tier: an endpoint received an encoded copy.
func endpointTiers(signal string) []tier {
	switch signal {
	case pipeline.SignalTraces.String():
		return []tier{exactTier, spanTiers[0]}
	case pipeline.SignalMetrics.String():
		return pointTiers[1:]
	}
	return logTiers[1:]
}
