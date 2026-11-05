package runresult

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Canonicalize puts the event log and the exporter outputs into a canonical
// order and renumbers the events.
//
// Two Collector behaviors make the observed order of independent pipelines
// vary between runs (v0.162.0): a receiver's fan-out consumer calls its
// pipelines in the order of graph.componentGraph.From, which iterates a map,
// and graph.ShutdownAll stops components in unstabilized topological order.
// Startup (topo.SortStabilized) is stable.
//
// Canonicalize therefore splits the log after startup into segments, one per
// injected input plus the shutdown phase (from the first component.stopped),
// and stable-sorts each segment by (signal, pipeline), which keeps every
// pipeline's own order. Events without a pipeline (input injections, exporter
// captures) belong to the preceding event of their signal. Input references
// are remapped. For the same reason exporter outputs concatenate pipeline
// deliveries in varying order, so their resource entries are sorted by
// content (telemetry.SortResources), as are endpoint outputs.
//
// The result is a valid linearization of the run but not necessarily the
// observed one; call it only for normalized output.
func (r *Result) Canonicalize() {
	for i := range r.Outputs {
		r.Outputs[i].Data = canonicalData(r.Outputs[i].Data)
	}
	for i := range r.EndpointOutputs {
		r.EndpointOutputs[i].Data = canonicalData(r.EndpointOutputs[i].Data)
	}
	r.sortMetricSnapshots()
	r.canonicalizeEvents()
}

// canonicalData sorts the resources and metrics of an output's
// telemetry; data not produced by buildOutputs is left as delivered.
func canonicalData(data json.RawMessage) json.RawMessage {
	p, err := telemetry.Decode(data)
	if err != nil {
		return data
	}
	if out, err := telemetry.SortResources(telemetry.SortMetrics(p)).JSON(); err == nil {
		return out
	}
	return data
}

// sortMetricSnapshots puts the metrics and data points of every metrics
// snapshot into canonical order (telemetry.SortMetrics) and re-keys the
// snapshots by their new content. Analysis runs on the result, so record
// positions in the report refer to the canonical order; the events' record
// object numbers are reordered with their records, or dropped when the new
// order cannot be told.
func (r *Result) sortMetricSnapshots() {
	type sorted struct {
		ref   string
		order []int
	}
	renamed := map[string]sorted{}
	for ref, data := range r.Snapshots {
		p, err := telemetry.Decode(data)
		if err != nil || p.Signal != pipeline.SignalMetrics {
			continue
		}
		out, order := telemetry.SortMetricsOrder(p)
		data2, err := out.JSON()
		if err != nil || bytes.Equal(data2, data) {
			continue
		}
		sum := sha256.Sum256(data2)
		to := "sha256:" + hex.EncodeToString(sum[:])
		renamed[ref] = sorted{to, order}
		delete(r.Snapshots, ref)
		r.Snapshots[to] = data2
	}
	for i := range r.Events {
		e := &r.Events[i]
		to, ok := renamed[e.Snapshot]
		if !ok {
			continue
		}
		e.Snapshot = to.ref
		if e.Objects == nil {
			continue
		}
		if len(to.order) != len(e.Objects) {
			e.Objects = nil
			continue
		}
		objects := make([]int, len(to.order))
		for j, from := range to.order {
			objects[j] = e.Objects[from]
		}
		e.Objects = objects
	}
}

func (r *Result) canonicalizeEvents() {
	ev := r.Events
	end := len(ev)
	if end > 0 && strings.HasPrefix(string(ev[end-1].Kind), "run.") {
		end--
	}
	var bounds []int
	shutdown := false
	for i, e := range ev[:end] {
		switch {
		case e.Kind == instrument.KindInput && !shutdown:
			bounds = append(bounds, i)
		case e.Kind == instrument.KindComponentStopped && !shutdown:
			shutdown = true
			bounds = append(bounds, i)
		}
	}
	bounds = append(bounds, end)
	for i := 0; i+1 < len(bounds); i++ {
		sortSegment(ev[bounds[i]:bounds[i+1]])
	}

	renumber := map[int]int{}
	for i := range ev {
		renumber[ev[i].Seq] = i + 1
		ev[i].Seq = i + 1
		ev[i].ID = fmt.Sprintf("%s:%d", ev[i].RunID, ev[i].Seq)
	}
	for i := range ev {
		if ev[i].InputSeq != 0 {
			ev[i].InputSeq = renumber[ev[i].InputSeq]
		}
		if ev[i].FromSeq != 0 {
			ev[i].FromSeq = renumber[ev[i].FromSeq]
		}
	}
	canonicalFanOut(ev)
	// Record object numbers follow the observed order too; renumber them
	// in order of first appearance.
	objects := map[int]int{}
	for i := range ev {
		if ev[i].Objects == nil {
			continue
		}
		renamed := make([]int, len(ev[i].Objects))
		for j, n := range ev[i].Objects {
			to, ok := objects[n]
			if !ok {
				to = len(objects) + 1
				objects[n] = to
			}
			renamed[j] = to
		}
		ev[i].Objects = renamed
	}
}

// sortSegment stable-sorts events by (signal, pipeline) in place. A
// pipeline fed by a connector sorts after the pipeline that fed it (by
// connector depth below the same root pipeline), so a connector's delivery
// still precedes what the receiving pipeline did with it.
func sortSegment(segment []instrument.Event) {
	type keyed struct {
		e                      instrument.Event
		signal, root, pipeline string
		depth                  int
	}
	keys := make([]keyed, 0, len(segment))
	last := map[string]string{}
	group := map[int]string{}
	for _, e := range segment {
		pipeline := e.Pipeline
		// A connector's input has no pipeline of its own: it belongs to
		// the pipeline that delivered it, like an exporter capture.
		if pipeline == "" || (e.Source == instrument.SourceConnectorWrapper && e.Kind == instrument.KindIngress) {
			pipeline = last[e.Signal]
		}
		last[e.Signal] = pipeline
		group[e.Seq] = pipeline
		keys = append(keys, keyed{e: e, signal: e.Signal, pipeline: pipeline})
	}
	// fedBy maps a pipeline to the pipeline whose connector fed it.
	fedBy := map[string]string{}
	for _, k := range keys {
		e := k.e
		if e.Source == instrument.SourceConnectorWrapper && e.Kind == instrument.KindEgress && e.Pipeline != "" {
			if src, ok := group[e.InputSeq]; ok && src != e.Pipeline {
				fedBy[e.Pipeline] = src
			}
		}
	}
	for i := range keys {
		root, depth := keys[i].pipeline, 0
		for src, ok := fedBy[root]; ok && depth < len(fedBy); src, ok = fedBy[root] {
			root, depth = src, depth+1
		}
		keys[i].root, keys[i].depth = root, depth
		if sig, _, _ := strings.Cut(root, "/"); root != "" {
			keys[i].signal = sig
		}
	}
	slices.SortStableFunc(keys, func(a, b keyed) int {
		return cmp.Or(cmp.Compare(a.signal, b.signal), cmp.Compare(a.root, b.root),
			cmp.Compare(a.depth, b.depth), cmp.Compare(a.pipeline, b.pipeline))
	})
	// A pipeline's exporter fan-out also calls its exporters in varying
	// order, so adjacent deliveries of one payload (exporter captures and
	// connector inputs with the same from_seq) are ordered by component.
	// Neither has further events in its own pipeline group, so they are
	// adjacent after the sort above.
	fanned := func(e instrument.Event) bool {
		return e.FromSeq != 0 && (e.Kind == instrument.KindExport ||
			(e.Source == instrument.SourceConnectorWrapper && e.Kind == instrument.KindIngress))
	}
	for i := 0; i < len(keys); {
		j := i + 1
		for j < len(keys) && fanned(keys[i].e) && fanned(keys[j].e) &&
			keys[j].e.FromSeq == keys[i].e.FromSeq && keys[j].pipeline == keys[i].pipeline {
			j++
		}
		slices.SortStableFunc(keys[i:j], func(a, b keyed) int { return cmp.Compare(a.e.Component, b.e.Component) })
		i = j
	}
	// tail_sampling releases the traces it decides in one batch in map
	// order. Each decision and the events that follow it in its pipeline
	// group (the release and what downstream components did with it) form
	// one group, and groups are ordered by trace ID.
	for i := 0; i < len(keys); {
		d := keys[i].e
		if d.Kind != instrument.KindDecision || d.Decision == nil {
			i++
			continue
		}
		end := i
		for end < len(keys) && keys[end].pipeline == keys[i].pipeline {
			end++
		}
		var groups [][]keyed
		start := i
		for k := i + 1; k <= end; k++ {
			if k == end || (keys[k].e.Kind == instrument.KindDecision && keys[k].e.Decision != nil && keys[k].e.Component == d.Component) {
				groups = append(groups, slices.Clone(keys[start:k]))
				start = k
			}
		}
		slices.SortStableFunc(groups, func(a, b []keyed) int {
			return cmp.Compare(a[0].e.Decision.TraceID, b[0].e.Decision.TraceID)
		})
		copy(keys[i:end], slices.Concat(groups...))
		i = end
	}
	for i, k := range keys {
		segment[i] = k.e
	}
}

// canonicalFanOut fixes which delivery of a fanned-out emission keeps its
// record objects. The Collector's fan-out consumer (v0.162.0) passes an
// emission's objects on to the last of its mutating consumers, in an order
// that varies between runs, and clones the data for the others. Where
// exactly one delivery holds the emission's objects, they are given to the
// canonically last delivery instead, by swapping its object numbers with
// that delivery's in every later event. A clone is the same records in the
// same order, so the result is a valid identity assignment, not
// necessarily the observed one.
func canonicalFanOut(ev []instrument.Event) {
	deliveries := map[int][]int{}
	for i, e := range ev {
		if e.FromSeq != 0 && e.Objects != nil {
			deliveries[e.FromSeq] = append(deliveries[e.FromSeq], i)
		}
	}
	for at := range ev {
		from := ev[at].Objects
		if from == nil || len(deliveries[ev[at].Seq]) < 2 {
			continue
		}
		var same []int
		last := -1
		for _, d := range deliveries[ev[at].Seq] {
			if len(ev[d].Objects) != len(from) {
				continue
			}
			if slices.Equal(ev[d].Objects, from) {
				same = append(same, d)
			}
			last = d
		}
		if len(same) != 1 || same[0] == last {
			continue
		}
		swap := map[int]int{}
		for k, a := range ev[same[0]].Objects {
			b := ev[last].Objects[k]
			swap[a], swap[b] = b, a
		}
		for i := at + 1; i < len(ev); i++ {
			if ev[i].Objects == nil {
				continue
			}
			swapped := make([]int, len(ev[i].Objects))
			for k, n := range ev[i].Objects {
				if to, ok := swap[n]; ok {
					n = to
				}
				swapped[k] = n
			}
			ev[i].Objects = swapped
		}
	}
}
