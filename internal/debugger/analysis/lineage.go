package analysis

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// ConfidenceID: matched by a stable identifier (a span's trace and span ID).
const ConfidenceID Confidence = "id"

// End is how a record's path through the run ends.
type End string

const (
	// EndDelivered: the record reached an exporter.
	EndDelivered End = "delivered"
	// EndDropped: a stage consumed the record and did not emit it.
	EndDropped End = "dropped"
	// EndNotRouted: a connector consumed the record and delivered it into
	// no pipeline.
	EndNotRouted End = "not_routed"
	// EndUnresolved: the record could not be followed with confidence (it
	// may have been changed beyond recognition, merged, or the run did not
	// complete).
	EndUnresolved End = "unresolved"
	// EndUnobserved: the record was passed to a component the debugger
	// does not observe, such as a local debug exporter.
	EndUnobserved End = "unobserved"
	// EndAggregated: an aggregating connector consumed the record and
	// counted it into data points (Hop.Into), which continue as records
	// of their own.
	EndAggregated End = "aggregated"
	// EndUncounted: an aggregating connector consumed the record and no
	// configured metric counted it.
	EndUncounted End = "uncounted"
)

// OriginAggregated marks a lineage that starts at a data point an
// aggregating connector emitted.
const OriginAggregated = "aggregated"

// Hop is one place a record was observed: the event that recorded the
// payload holding it, and the record's position in that payload.
type Hop struct {
	// Parent indexes the hop the record came from; -1 for the injection.
	Parent    int             `json:"parent"`
	Seq       int             `json:"seq"`
	Kind      instrument.Kind `json:"kind"`
	Component string          `json:"component,omitempty"`
	Pipeline  string          `json:"pipeline,omitempty"`
	// Ref is the record's position in the event's payload, for example
	// "resource[0].scope[0].log_record[2]".
	Ref string `json:"ref"`
	// Confidence states how a stage output was matched to the record in
	// the stage's input: by span ID, as the same record object (object, or
	// exact when also unchanged), or by content (exact, high, medium).
	// It is empty for deliveries, which are linked by the call that
	// carried the payload (from_seq) and keep positions.
	Confidence Confidence `json:"confidence,omitempty"`
	// Changes are what the stage changed on this record, including its
	// resource and scope, on stage.output hops. Entities name positions in
	// the stage's input.
	Changes []Change `json:"changes,omitempty"`
	// End is set on the last hop of a path.
	End  End    `json:"end,omitempty"`
	Note string `json:"note,omitempty"`
	// Into names the lineages of the data points an aggregating connector
	// counted the record into (End aggregated).
	Into []string `json:"into,omitempty"`
}

// Delivery is an exporter a record reached.
type Delivery struct {
	Exporter string `json:"exporter"`
	// Pipeline is the pipeline that delivered it, when known.
	Pipeline string `json:"pipeline,omitempty"`
	Hop      int    `json:"hop"`
	// Changed reports whether any stage on the way changed the record.
	Changed bool `json:"changed"`
}

// Stop is a path that ended without reaching an exporter.
type Stop struct {
	Component string `json:"component,omitempty"`
	Pipeline  string `json:"pipeline,omitempty"`
	Hop       int    `json:"hop"`
	End       End    `json:"end"`
}

// RecordLineage follows one record through the run: an
// injected record, or a data point an aggregating connector derived from
// injected records (Origin aggregated). Fan-out (a receiver feeding several
// pipelines, a connector copying to several routes) makes Hops a tree.
type RecordLineage struct {
	// ID is "<seq>:<ref>": the event that first holds the record (its
	// input.injected event, or the aggregating connector's delivery) and
	// the record's position in it.
	ID       string `json:"id"`
	Signal   string `json:"signal"`
	Receiver string `json:"receiver,omitempty"`
	// Origin is empty for injected records and "aggregated" for data
	// points an aggregating connector emitted.
	Origin string `json:"origin,omitempty"`
	// Connector is the aggregating connector that emitted the data point,
	// and Sources the lineages of the records it counted.
	Connector string   `json:"connector,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	// Name is the span name, metric name, or log event name or body.
	Name string `json:"name"`
	// Span is "span:<trace_id>:<span_id>" for spans, the name stage diffs
	// use for them.
	Span       string     `json:"span,omitempty"`
	Hops       []Hop      `json:"hops"`
	Deliveries []Delivery `json:"deliveries,omitempty"`
	Stops      []Stop     `json:"stops,omitempty"`
	// Sent are the simulated endpoints the record was also sent to,
	// paired by content.
	Sent []Sent `json:"sent,omitempty"`
}

// Lineage follows every injected record through the run, then every data
// point an aggregating connector derived from them (aggs; attributed only
// when verified). Deliveries between
// components are exact: an event's from_seq names the emission that made
// the call. Across a stage, records are matched from the stage's input to
// its outputs like stage diffs match them (spans by ID, logs and data points
// by content with confidence), and a record is reported dropped only when
// that is certain. Connectors are matched per output, since a route may
// copy a record into several pipelines. What simulated endpoints received
// is paired with the lineages by content (pairEndpoints).
func Lineage(events []instrument.Event, snaps instrument.Snapshots, stages []Stage, aggs []*Aggregation, completed bool) ([]RecordLineage, error) {
	l := &lineage{
		bySeq:    map[int]instrument.Event{},
		children: map[int][]int{},
		inputs:   map[int]*StageInput{},
		stageOf:  map[int]*Stage{},
		records:  map[int][]telemetry.Record{},
		payloads: map[int]telemetry.Payload{},
		matched:  map[int]map[int][]target{},
		snaps:    snaps,
		complete: completed,
		aggs:     map[int]*Aggregation{},
		sources:  map[string][]string{},
	}
	for _, a := range aggs {
		l.aggs[a.Input] = a
	}
	for _, e := range events {
		l.bySeq[e.Seq] = e
		if e.FromSeq != 0 {
			l.children[e.FromSeq] = append(l.children[e.FromSeq], e.Seq)
		}
	}
	for si := range stages {
		for ii := range stages[si].Inputs {
			in := &stages[si].Inputs[ii]
			l.inputs[in.Seq] = in
			l.stageOf[in.Seq] = &stages[si]
		}
	}

	var out []RecordLineage
	for _, e := range events {
		if e.Kind != instrument.KindInput || e.Snapshot == "" {
			continue
		}
		recs, p, err := l.recordsOf(e.Seq)
		if err != nil {
			return nil, err
		}
		for i, r := range recs {
			ref := r.Ref(p.Signal)
			rl := RecordLineage{
				ID:       fmt.Sprintf("%d:%s", e.Seq, ref),
				Signal:   p.Signal.String(),
				Receiver: e.Component,
				Name:     recordName(p.Signal, r),
			}
			if p.Signal == pipeline.SignalTraces {
				rl.Span = "span:" + r.Str("trace_id") + ":" + r.Str("span_id")
			}
			if err := l.follow(&rl, -1, e.Seq, i, "", nil); err != nil {
				return nil, err
			}
			summarize(&rl)
			out = append(out, rl)
		}
	}
	derived, err := l.derived(stages)
	if err != nil {
		return nil, err
	}
	out = append(out, derived...)
	if err := l.pairEndpoints(events, out); err != nil {
		return nil, err
	}
	return out, nil
}

// derived follows the data points of every verified aggregation from the
// connector's input through each of its deliveries.
func (l *lineage) derived(stages []Stage) ([]RecordLineage, error) {
	var out []RecordLineage
	for _, s := range stages {
		for _, in := range s.Inputs {
			a := l.aggs[in.Seq]
			if a == nil || a.Verification != AggregationVerified || a.Output == 0 {
				continue
			}
			recs, p, err := l.recordsOf(a.Output)
			if err != nil {
				return nil, err
			}
			index := map[string]int{}
			for i, r := range recs {
				index[r.Ref(p.Signal)] = i
			}
			for _, pt := range a.Points {
				id := aggregatedID(a, pt)
				rl := RecordLineage{ID: id, Signal: p.Signal.String(), Origin: OriginAggregated,
					Connector: a.Connector, Name: pointName(pt), Sources: l.sources[id]}
				e := l.bySeq[a.Input]
				rl.Hops = append(rl.Hops, Hop{Parent: -1, Seq: a.Input, Kind: e.Kind, Component: e.Component,
					Note: fmt.Sprintf("counted from %d record(s)", pt.Value)})
				for _, d := range in.Outputs {
					idx, ok := index[pt.Ref], true
					if d != a.Output {
						if idx, ok, err = l.sameRecord(a.Output, idx, d); err != nil {
							return nil, err
						}
					}
					if !ok {
						rl.Hops = append(rl.Hops, Hop{Parent: 0, Seq: d, Kind: l.bySeq[d].Kind, Component: l.bySeq[d].Component,
							Pipeline: l.bySeq[d].Pipeline, End: EndUnresolved, Note: "the delivered payload differs from the emitted one"})
						continue
					}
					if err := l.follow(&rl, 0, d, idx, "", nil); err != nil {
						return nil, err
					}
				}
				summarize(&rl)
				out = append(out, rl)
			}
		}
	}
	return out, nil
}

func aggregatedID(a *Aggregation, p AggregatedPoint) string {
	return fmt.Sprintf("%d:%s", a.Output, p.Ref)
}

// pointName labels a data point by its metric and attributes, for example
// "app.log.errors{http.route=/pay, service.name=payments}".
func pointName(p AggregatedPoint) string {
	if len(p.Attributes) == 0 {
		return p.Metric
	}
	kv := make([]string, 0, len(p.Attributes))
	for k, v := range p.Attributes {
		kv = append(kv, k+"="+v)
	}
	slices.Sort(kv)
	return p.Metric + "{" + strings.Join(kv, ", ") + "}"
}

// aggregated ends a record's path at an aggregating connector's input hop.
func (l *lineage) aggregated(rl *RecordLineage, at int, a *Aggregation) {
	h := &rl.Hops[at]
	switch {
	case a.Output == 0:
		h.End, h.Note = EndDropped, "the connector emitted nothing for this input"
		if len(a.Notes) > 0 {
			h.Note = a.Notes[0]
		}
	case a.Verification != AggregationVerified:
		h.End, h.Note = EndAggregated, "aggregated into another signal; which data points count it is not known ("+string(a.Verification)+")"
	default:
		for _, p := range a.Points {
			if slices.ContainsFunc(p.Records, func(r string) bool { return Contains(r, h.Ref) }) {
				id := aggregatedID(a, p)
				h.Into = append(h.Into, id)
				l.sources[id] = append(l.sources[id], rl.ID)
			}
		}
		if len(h.Into) == 0 {
			h.End, h.Note = EndUncounted, "no metric of the connector counted this record"
			return
		}
		h.End, h.Note = EndAggregated, fmt.Sprintf("counted into %d data point(s)", len(h.Into))
	}
}

// target is a record in a stage output that a record of the stage's input
// became.
type target struct {
	seq, index int
	confidence Confidence
	changes    []Change
}

type lineage struct {
	bySeq    map[int]instrument.Event
	children map[int][]int
	inputs   map[int]*StageInput
	stageOf  map[int]*Stage
	records  map[int][]telemetry.Record
	payloads map[int]telemetry.Payload
	// matched caches, per stage.input seq, the targets of each input
	// record; a missing entry means the record has none.
	matched  map[int]map[int][]target
	snaps    instrument.Snapshots
	complete bool
	hops     int
	// aggs are aggregations by connector stage.input seq; sources collects
	// the lineages counted into each aggregated data point.
	aggs    map[int]*Aggregation
	sources map[string][]string
}

// maxHops bounds the work for one run; the Collector rejects cyclic
// pipelines, so it is only a guard.
const maxHops = 200000

func (l *lineage) recordsOf(seq int) ([]telemetry.Record, telemetry.Payload, error) {
	if recs, ok := l.records[seq]; ok {
		return recs, l.payloads[seq], nil
	}
	p, err := l.snaps.Payload(l.bySeq[seq].Snapshot)
	if err != nil {
		return nil, telemetry.Payload{}, err
	}
	recs := telemetry.Records(p)
	l.records[seq], l.payloads[seq] = recs, p
	return recs, p, nil
}

// follow adds the hop for record index of event seq and everything after
// it.
func (l *lineage) follow(rl *RecordLineage, parent, seq, index int, conf Confidence, changes []Change) error {
	if l.hops++; l.hops > maxHops {
		return fmt.Errorf("lineage exceeds %d hops", maxHops)
	}
	e := l.bySeq[seq]
	recs, p, err := l.recordsOf(seq)
	if err != nil {
		return err
	}
	h := Hop{Parent: parent, Seq: seq, Kind: e.Kind, Component: e.Component, Pipeline: e.Pipeline,
		Ref: recs[index].Ref(p.Signal), Confidence: conf, Changes: changes}
	at := len(rl.Hops)
	rl.Hops = append(rl.Hops, h)
	end := func(end End, note string) error {
		rl.Hops[at].End, rl.Hops[at].Note = end, note
		return nil
	}

	switch e.Kind {
	case instrument.KindExport:
		return end(EndDelivered, "")
	case instrument.KindInput, instrument.KindEgress:
		kids := l.children[seq]
		if len(kids) == 0 {
			if !l.complete {
				return end(EndUnresolved, "the run did not complete")
			}
			return end(EndUnobserved, "passed on to a component the debugger does not observe, such as a local exporter")
		}
		for _, k := range kids {
			idx, ok, err := l.sameRecord(seq, index, k)
			if err != nil {
				return err
			}
			if !ok {
				rl.Hops = append(rl.Hops, Hop{Parent: at, Seq: k, Kind: l.bySeq[k].Kind, Component: l.bySeq[k].Component,
					Pipeline: l.bySeq[k].Pipeline, End: EndUnresolved, Note: "the delivered payload differs from the emitted one"})
				continue
			}
			if err := l.follow(rl, at, k, idx, "", nil); err != nil {
				return err
			}
		}
		return nil
	case instrument.KindIngress:
		in := l.inputs[seq]
		if in == nil {
			return end(EndUnresolved, "no stage analysis for this input")
		}
		if a := l.aggs[seq]; a != nil {
			l.aggregated(rl, at, a)
			return nil
		}
		for _, o := range in.Outputs {
			if l.bySeq[o].Signal != e.Signal {
				return end(EndUnresolved, "the connector converts records to "+l.bySeq[o].Signal+"; the debugger cannot attribute its output")
			}
		}
		targets, err := l.targets(seq)
		if err != nil {
			return err
		}
		ts := targets[index]
		if len(ts) == 0 {
			return end(l.lost(seq, index))
		}
		for _, t := range ts {
			if err := l.follow(rl, at, t.seq, t.index, t.confidence, t.changes); err != nil {
				return err
			}
		}
		return nil
	}
	return end(EndUnresolved, "unexpected event "+string(e.Kind))
}

// sameRecord maps a record of an emission to the delivery k that carried
// it. A delivery normally holds the same payload, so positions are kept;
// otherwise the record is found by its object, then by identical content.
func (l *lineage) sameRecord(from, index, k int) (int, bool, error) {
	if l.bySeq[from].Snapshot == l.bySeq[k].Snapshot {
		return index, true, nil
	}
	a, _, err := l.recordsOf(from)
	if err != nil {
		return 0, false, err
	}
	b, _, err := l.recordsOf(k)
	if err != nil {
		return 0, false, err
	}
	if fo, ko := l.objects(from, len(a)), l.objects(k, len(b)); fo != nil && ko != nil && fo[index] != 0 {
		if i := slices.Index(ko, fo[index]); i >= 0 {
			return i, true, nil
		}
	}
	want := telemetry.Fingerprint(a[index].Fields)
	found := -1
	for i, r := range b {
		if telemetry.Fingerprint(r.Fields) == want {
			if found >= 0 {
				return 0, false, nil
			}
			found = i
		}
	}
	return found, found >= 0, nil
}

// lost explains why a stage input record has no target.
func (l *lineage) lost(seq, index int) (End, string) {
	in, stage := l.inputs[seq], l.stageOf[seq]
	connector := l.bySeq[seq].Source == instrument.SourceConnectorWrapper
	switch in.Outcome {
	case OutcomeUnresolved:
		return EndUnresolved, "the run did not complete, so missing output proves nothing"
	case OutcomeNoOutput:
		if connector {
			return EndNotRouted, "the connector delivered nothing for this input"
		}
		return EndDropped, "the stage emitted nothing for this input"
	case OutcomeDeferred:
		if connector && len(stage.Emissions) == 0 {
			return EndAggregated, "aggregated into another signal and emitted later, outside any consume call; which data points include it is not attributed"
		}
		if stage.Reconciliation == nil {
			return EndUnresolved, "the stage emitted later and its output is not final"
		}
	}
	if connector {
		return EndNotRouted, "no route delivered this record"
	}
	if l.certainlyRemoved(seq, index) {
		return EndDropped, "the stage did not emit this record"
	}
	return EndUnresolved, "no output record matches it with confidence; it may have been changed, merged or removed"
}

// certainlyRemoved reports whether every record left unmatched is on the
// input side, which is when stage diffs list removals.
func (l *lineage) certainlyRemoved(seq, index int) bool {
	return l.matched[seq] != nil && l.matched[seq][-1] == nil
}

// targets matches the records of a stage input to its outputs and caches
// the result. The entry -1 is set (to an empty slice) when output records
// were left unmatched, which makes removals uncertain.
func (l *lineage) targets(seq int) (map[int][]target, error) {
	if m, ok := l.matched[seq]; ok {
		return m, nil
	}
	in, stage := l.inputs[seq], l.stageOf[seq]
	m := map[int][]target{}
	l.matched[seq] = m
	connector := l.bySeq[seq].Source == instrument.SourceConnectorWrapper
	switch {
	case len(in.Outputs) > 0 && connector:
		for _, out := range in.Outputs {
			if err := l.match([]int{seq}, []int{out}, m, nil); err != nil {
				return nil, err
			}
		}
	case len(in.Outputs) > 0:
		if err := l.match([]int{seq}, in.Outputs, m, nil); err != nil {
			return nil, err
		}
	case in.Outcome == OutcomeDeferred && stage.Reconciliation != nil:
		// Everything the stage received without a correlated output, and
		// everything it emitted later, are matched as one; the result
		// serves every deferred input of the stage.
		var ins []int
		for _, x := range stage.Inputs {
			if len(x.Outputs) == 0 {
				ins = append(ins, x.Seq)
			}
		}
		byInput := map[int]map[int][]target{}
		for _, x := range ins {
			byInput[x] = map[int][]target{}
		}
		if err := l.match(ins, stage.Emissions, nil, byInput); err != nil {
			return nil, err
		}
		for x, mm := range byInput {
			l.matched[x] = mm
		}
		return l.matched[seq], nil
	}
	return m, nil
}

// match pairs the records of the before events with those of the after
// events and adds targets, to m when there is one before event and to
// byInput (keyed by before event) otherwise.
func (l *lineage) match(before, after []int, m map[int][]target, byInput map[int]map[int][]target) error {
	type loc struct{ seq, index int }
	var bRecs, aRecs []telemetry.Record
	var bObj, aObj []int
	var bLoc, aLoc []loc
	var signal pipeline.Signal
	for _, s := range before {
		recs, p, err := l.recordsOf(s)
		if err != nil {
			return err
		}
		signal = p.Signal
		bObj = append(bObj, l.objectsOrZero(s, len(recs))...)
		for i, r := range recs {
			bRecs, bLoc = append(bRecs, r), append(bLoc, loc{s, i})
		}
	}
	for _, s := range after {
		recs, p, err := l.recordsOf(s)
		if err != nil {
			return err
		}
		if p.Signal != signal {
			// A connector that converts signals has no record lineage.
			continue
		}
		aObj = append(aObj, l.objectsOrZero(s, len(recs))...)
		for i, r := range recs {
			aRecs, aLoc = append(aRecs, r), append(aLoc, loc{s, i})
		}
	}
	tiers := logTiers
	switch signal {
	case pipeline.SignalMetrics:
		tiers = pointTiers
	case pipeline.SignalTraces:
		tiers = spanTiers
	}
	matches, _, unA := matchRecords(bRecs, aRecs, bObj, aObj, tiers)
	for _, x := range matches {
		b, a := bLoc[x.b], aLoc[x.a]
		bp, ap := l.payloads[b.seq], l.payloads[a.seq]
		t := target{seq: a.seq, index: a.index, confidence: x.confidence,
			changes: recordChanges(bp, ap, bRecs[x.b], aRecs[x.a])}
		dst := m
		if byInput != nil {
			dst = byInput[b.seq]
		}
		dst[b.index] = append(dst[b.index], t)
	}
	if len(unA) > 0 {
		if byInput != nil {
			for _, mm := range byInput {
				mm[-1] = []target{}
			}
		} else {
			m[-1] = []target{}
		}
	}
	return nil
}

// spanTiers pair spans by ID, then (a span whose ID a stage changed) by
// object.
var spanTiers = []tier{{ConfidenceID, func(r telemetry.Record, _ int) (string, bool) {
	return r.Str("trace_id") + ":" + r.Str("span_id"), true
}}, objectTier}

// objects returns the record object numbers of event seq when it has one
// for each of its n records, else nil.
func (l *lineage) objects(seq, n int) []int {
	if o := l.bySeq[seq].Objects; len(o) == n && n > 0 {
		return o
	}
	return nil
}

// objectsOrZero is objects with zeros (unknown) in place of nil.
func (l *lineage) objectsOrZero(seq, n int) []int {
	if o := l.objects(seq, n); o != nil {
		return o
	}
	return make([]int, n)
}

// recordChanges diffs one matched record and its resource and scope.
func recordChanges(bp, ap telemetry.Payload, b, a telemetry.Record) []Change {
	entity := b.Ref(bp.Signal)
	if bp.Signal == pipeline.SignalMetrics {
		// Data point fields carry their metric's; report them once.
		entity = fmt.Sprintf("resource[%d].scope[%d].metric[%d].data_point[%d]", b.Resource, b.Scope, b.Metric, b.Index)
	}
	var out []Change
	br, bURL := bp.ResourceAt(b.Resource)
	ar, aURL := ap.ResourceAt(a.Resource)
	res := fmt.Sprintf("resource[%d]", b.Resource)
	out = append(out, DiffResource(res, br, ar)...)
	out = append(out, diffScalar(res, "schema_url", bURL, aURL)...)
	bs, _ := bp.ScopeAt(b.Resource, b.Scope)
	as, _ := ap.ScopeAt(a.Resource, a.Scope)
	out = append(out, DiffScope(fmt.Sprintf("%s.scope[%d]", res, b.Scope), bs, as)...)
	return append(out, diffFields(entity, b.Fields, a.Fields)...)
}

func recordName(signal pipeline.Signal, r telemetry.Record) string {
	if signal == pipeline.SignalTraces {
		return r.Str("name")
	}
	return recordRef(signal, r).Name
}

// summarize lists where each path of a record ended.
func summarize(rl *RecordLineage) {
	changed := make([]bool, len(rl.Hops))
	for i, h := range rl.Hops {
		changed[i] = len(h.Changes) > 0 || (h.Parent >= 0 && changed[h.Parent])
	}
	for i, h := range rl.Hops {
		switch h.End {
		case "":
		case EndDelivered:
			d := Delivery{Exporter: h.Component, Hop: i, Changed: changed[i]}
			if h.Parent >= 0 {
				d.Pipeline = rl.Hops[h.Parent].Pipeline
			}
			rl.Deliveries = append(rl.Deliveries, d)
		default:
			rl.Stops = append(rl.Stops, Stop{Component: h.Component, Pipeline: h.Pipeline, Hop: i, End: h.End})
		}
	}
	slices.SortStableFunc(rl.Deliveries, func(a, b Delivery) int {
		return cmp.Or(cmp.Compare(a.Exporter, b.Exporter), cmp.Compare(a.Pipeline, b.Pipeline))
	})
}
