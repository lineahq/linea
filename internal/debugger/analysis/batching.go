package analysis

import (
	"math"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Batching explains when and why one batching processor instance (batch
// or queue_batch in one pipeline) sent what it held. Arrivals and flushes
// are observations (the stage's inputs and emissions); each flush's
// trigger is inferred from the configured sizes and a replay of the
// processor's timer, and the inference is checked against the processor's
// own metrics: the trigger counts of batch, the send count and sizes of
// queue_batch.
type Batching struct {
	Processor string        `json:"processor"`
	Pipeline  string        `json:"pipeline"`
	Signal    string        `json:"signal"`
	Settings  BatchSettings `json:"settings"`
	// Arrivals are the stage inputs, in event order.
	Arrivals []BatchArrival `json:"arrivals"`
	// Flushes are the stage's emissions, in event order.
	Flushes []BatchFlush `json:"flushes"`
	// Verification compares the inference with the processor's metrics
	// (batch: the size and timeout trigger counts; queue_batch: the number
	// of sends and the items they held): metrics_agree, metrics_differ, or
	// unverified when the metrics were not recorded.
	Verification string `json:"verification"`
	// Batchers are the processor's batchers with metadata_keys (queue_batch:
	// its partitions), one per combination of their values, in order of
	// first arrival; absent without metadata_keys, when one batcher holds
	// everything.
	Batchers []BatchBatcher `json:"batchers,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
}

// BatchBatcher is one batcher of a processor with metadata_keys: its own
// pending items, send_batch_size count and timer, serving one combination
// of metadata values (batchprocessor v0.162.0 multiShardBatcher).
type BatchBatcher struct {
	// Metadata holds the values of each metadata key (lowercased) the
	// requests carried; an empty list when they carried none. Only the
	// listed keys appear, never other headers.
	Metadata map[string][]string `json:"metadata"`
	// Unknown: the arrivals' headers could not be traced to scenario
	// inputs (another batch processor earlier in the path replaces the
	// request context, or the records have no lineage).
	Unknown bool `json:"unknown,omitempty"`
	// Refused: a new combination beyond metadata_cardinality_limit; the
	// processor refuses its requests with a permanent error.
	Refused bool `json:"refused,omitempty"`
	// Evicted: queue_batch held more partitions than
	// batch::partition::cache_size and flushed and removed this one, the
	// least recently used; a later request with its values starts a new
	// batcher.
	Evicted  bool `json:"evicted,omitempty"`
	Arrivals int  `json:"arrivals"`
	Items    int  `json:"items"`
	Flushes  int  `json:"flushes"`
}

// Batching processor types.
const (
	TypeBatch      = "batch"
	TypeQueueBatch = "queue_batch"
)

// IsBatcher reports whether a processor type batches requests: the batch
// processor and, from Collector 0.165, the queue_batch processor.
func IsBatcher(typ string) bool { return typ == TypeBatch || typ == TypeQueueBatch }

// BatchSettings are the processor's effective settings, in the batch
// processor's terms. For queue_batch, SendBatchSize is batch::min_size,
// SendBatchMaxSize batch::max_size, Timeout batch::flush_timeout and
// MetadataKeys batch::partition::metadata_keys; without its batch section
// it sends every request as it arrives (Unbatched, SendBatchSize 0).
type BatchSettings struct {
	// Type is the processor type, batch or queue_batch; empty in reports
	// from before queue_batch, which are batch.
	Type             string   `json:"type,omitempty"`
	SendBatchSize    int      `json:"send_batch_size"`
	SendBatchMaxSize int      `json:"send_batch_max_size"`
	Timeout          string   `json:"timeout"`
	MetadataKeys     []string `json:"metadata_keys,omitempty"`
	// MetadataCardinalityLimit bounds the batchers metadata_keys creates;
	// 0 is no limit.
	MetadataCardinalityLimit int `json:"metadata_cardinality_limit,omitempty"`
	// Sizer is what queue_batch's sizes count, items or bytes (the
	// serialized request); empty for batch, which counts items.
	Sizer string `json:"sizer,omitempty"`
	// CacheSize bounds queue_batch's partitions
	// (batch::partition::cache_size); beyond it the least recently used
	// one is flushed and removed.
	CacheSize int `json:"cache_size,omitempty"`
	// Unbatched: queue_batch without its batch section queues requests and
	// sends each one as it is.
	Unbatched bool `json:"unbatched,omitempty"`
}

// QueueBatch reports whether the settings are queue_batch's.
func (s BatchSettings) QueueBatch() bool { return s.Type == TypeQueueBatch }

// Bytes reports whether the sizes count bytes rather than items.
func (s BatchSettings) Bytes() bool { return s.Sizer == "bytes" }

// SizeName, MaxSizeName, TimeoutName and KeysName name the settings as the
// processor's own configuration does.
func (s BatchSettings) SizeName() string {
	if s.QueueBatch() {
		return "batch::min_size"
	}
	return "send_batch_size"
}

func (s BatchSettings) MaxSizeName() string {
	if s.QueueBatch() {
		return "batch::max_size"
	}
	return "send_batch_max_size"
}

func (s BatchSettings) TimeoutName() string {
	if s.QueueBatch() {
		return "batch::flush_timeout"
	}
	return "timeout"
}

func (s BatchSettings) KeysName() string {
	if s.QueueBatch() {
		return "batch::partition::metadata_keys"
	}
	return "metadata_keys"
}

// SizeUnit is what the sizes count, for summaries: the signal's item
// noun, or "byte".
func (s BatchSettings) SizeUnit(itemNoun string) string {
	if s.Bytes() {
		return "byte"
	}
	return itemNoun
}

// BatchArrival is one request the processor received.
type BatchArrival struct {
	Seq int `json:"seq"`
	// OffsetMS is the time since the processor started, in milliseconds;
	// absent in normalized reports.
	OffsetMS  *float64 `json:"offset_ms,omitempty"`
	Items     int      `json:"items"`
	Resources int      `json:"resources"`
	// Services is the service.name of each resource block, in order ("" when
	// a block has none), so a view can say where records came from.
	Services []string `json:"services,omitempty"`
	// Batcher indexes Batching.Batchers; 0 without metadata_keys.
	Batcher int `json:"batcher"`
}

// Batch flush triggers.
const (
	// TriggerSize: the pending items reached send_batch_size (queue_batch:
	// batch::min_size, in items or bytes).
	TriggerSize = "size"
	// TriggerTimeout: the timer fired with items pending.
	TriggerTimeout = "timeout"
	// TriggerShutdown: the processor shut down with items pending.
	TriggerShutdown = "shutdown"
	// TriggerImmediate: without a timer (timeout or send_batch_size 0)
	// every request is sent as it arrives, split by send_batch_max_size.
	TriggerImmediate = "immediate"
	// TriggerEvicted: queue_batch flushed a partition it removed to stay
	// within batch::partition::cache_size.
	TriggerEvicted = "evicted"
)

// Batching verifications.
const (
	BatchMetricsAgree  = "metrics_agree"
	BatchMetricsDiffer = "metrics_differ"
	BatchUnverified    = "unverified"
)

// BatchFlush is one request the processor sent.
type BatchFlush struct {
	Seq      int      `json:"seq"`
	OffsetMS *float64 `json:"offset_ms,omitempty"`
	Items    int      `json:"items"`
	// Resources counts the resource blocks in the request and
	// DistinctResources the different resources among them: batching
	// appends requests without merging equal resources.
	Resources         int `json:"resources"`
	DistinctResources int `json:"distinct_resources"`
	// Services is the service.name of each resource block, in order.
	Services []string `json:"services,omitempty"`
	// Batcher indexes Batching.Batchers; 0 without metadata_keys.
	Batcher int    `json:"batcher"`
	Trigger string `json:"trigger"`
	// Split: the send was capped at send_batch_max_size and items stayed
	// pending.
	Split bool `json:"split,omitempty"`
	// Pending is how many items the processor held right before this
	// send, and Left how many it still held after it.
	Pending int `json:"pending"`
	Left    int `json:"left"`
	// Sources are the arrivals the flush's records came from, by record
	// lineage.
	Sources []BatchSource `json:"sources,omitempty"`
	// Wait is how long the flush's records waited in the processor, from
	// their arrival to this flush; absent in normalized reports.
	Wait *BatchWait `json:"wait_ms,omitempty"`
}

// BatchSource is how many of a flush's records came from one arrival.
type BatchSource struct {
	Arrival int `json:"arrival"`
	Items   int `json:"items"`
}

// BatchWait is the range of record waits in milliseconds.
type BatchWait struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Metrics read for verification: the batch processor's trigger counts
// (batchprocessor v0.162.0) and queue_batch's histogram of the items in
// each send (queuebatchprocessor v0.165.0).
const (
	metricSizeTrigger    = "otelcol_processor_batch_batch_size_trigger_send"
	metricTimeoutTrigger = "otelcol_processor_batch_timeout_trigger_send"
	metricQueueBatchSend = "otelcol_processor_queuebatch_batch_send_size"
)

// BatchInput is what Batch reads for one processor instance.
type BatchInput struct {
	Processor, Pipeline, Signal string
	Settings                    BatchSettings
	Timeout                     time.Duration
	Events                      []instrument.Event
	Snapshots                   instrument.Snapshots
	Lineage                     []RecordLineage
	Metrics                     []instrument.ComponentMetric
	Completed                   bool
	// InputHeaders are, by input.injected seq, the headers the receiver
	// handed to the pipeline for each scenario input: what metadata_keys
	// reads. Inputs whose receiver dropped them are absent.
	InputHeaders map[int]map[string][]string
	// BatchProcessors are the IDs of every batching processor (batch and
	// queue_batch) in the configuration, to tell when an earlier one
	// replaced the request context.
	BatchProcessors map[string]bool
}

// Batch explains one batching processor instance. Shutdown sends its
// pending items with the timeout trigger (batchprocessor v0.162.0), so the
// timeout metric counts both timeout and shutdown flushes.
//
// queue_batch (queuebatchprocessor v0.165.0, docs/internals/batching.md)
// sends the same way but restarts a batcher's timer when a pending batch
// starts (a request reaching an empty batcher, or what a size send left)
// and at every tick, not after a size send that left nothing. Its sizes
// may count bytes, which the replay measures as the serialized request.
func Batch(in BatchInput) (*Batching, error) {
	b := &Batching{Processor: in.Processor, Pipeline: in.Pipeline, Signal: in.Signal, Settings: in.Settings, Arrivals: []BatchArrival{}, Flushes: []BatchFlush{}}
	var start time.Time
	at := map[int]time.Time{}
	var stream []instrument.Event
	for _, e := range in.Events {
		if e.Component != in.Processor || e.Pipeline != in.Pipeline {
			continue
		}
		switch {
		case e.Kind == instrument.KindComponentStarted:
			start = e.Time
		case e.Signal == in.Signal && (e.Kind == instrument.KindIngress || e.Kind == instrument.KindEgress):
			stream = append(stream, e)
			at[e.Seq] = e.Time
		}
	}
	if start.IsZero() && len(stream) > 0 {
		start = stream[0].Time
	}
	offset := func(t time.Time) *float64 {
		if t.IsZero() || start.IsZero() {
			return nil
		}
		v := ms(t.Sub(start))
		return &v
	}

	sources := batchSources(in)
	qb := in.Settings.QueueBatch()
	// sizeOf is an event's size in the unit of the configured sizes; with
	// bytes, the length of the serialized request, which adds up when
	// requests are joined (their resource blocks are appended).
	bytesUnknown := false
	sizeOf := func(e instrument.Event, items int) (int, error) {
		if !in.Settings.Bytes() {
			return items, nil
		}
		if e.Snapshot == "" {
			bytesUnknown = true
			return 0, nil
		}
		p, err := in.Snapshots.Payload(e.Snapshot)
		if err != nil {
			return 0, err
		}
		raw, err := p.Proto()
		return len(raw), err
	}
	hasTimer := in.Timeout > 0 && in.Settings.SendBatchSize > 0
	tol := in.Timeout / 2
	if tol > 25*time.Millisecond {
		tol = 25 * time.Millisecond
	}
	lastArrival := -1
	for i, e := range stream {
		if e.Kind == instrument.KindIngress {
			lastArrival = i
		}
	}
	shards := newShards(in, start)
	arrivalShard := map[int]int{}
	for i, e := range stream {
		items := 0
		if e.Counts != nil {
			items = e.Counts.Items
		}
		if e.Kind == instrument.KindIngress {
			resources := 0
			if e.Counts != nil {
				resources = e.Counts.Resources
			}
			a := BatchArrival{Seq: e.Seq, OffsetMS: offset(e.Time), Items: items, Resources: resources}
			if e.Snapshot != "" {
				p, err := in.Snapshots.Payload(e.Snapshot)
				if err != nil {
					return nil, err
				}
				a.Services = serviceNames(p)
			}
			size, err := sizeOf(e, items)
			if err != nil {
				return nil, err
			}
			a.Batcher = shards.arrive(e, items, size)
			arrivalShard[e.Seq] = a.Batcher
			b.Arrivals = append(b.Arrivals, a)
			continue
		}
		f := BatchFlush{Seq: e.Seq, OffsetMS: offset(e.Time), Items: items}
		f.Batcher = shards.of(sources[e.Seq], arrivalShard)
		sh := &shards.list[f.Batcher]
		f.Pending = sh.pending
		size, err := sizeOf(e, items)
		if err != nil {
			return nil, err
		}
		if e.Counts != nil {
			f.Resources = e.Counts.Resources
		}
		if e.Snapshot != "" {
			if p, err := in.Snapshots.Payload(e.Snapshot); err == nil {
				f.DistinctResources = distinctResources(p)
				f.Services = serviceNames(p)
			} else {
				return nil, err
			}
		}
		maxSize := in.Settings.SendBatchMaxSize
		if in.Settings.Bytes() {
			f.Split = maxSize > 0 && sh.size > maxSize && sh.size > size
		} else {
			f.Split = maxSize > 0 && items == maxSize && sh.pending > maxSize
		}
		switch {
		case !hasTimer:
			f.Trigger = TriggerImmediate
		case sh.evicted && sh.pending > 0:
			f.Trigger = TriggerEvicted
		case sh.size >= in.Settings.SendBatchSize:
			f.Trigger = TriggerSize
			// batch: processItem resets the timer after its last size
			// send. queue_batch: what the send left starts a pending
			// batch, which restarts the timer.
			if !qb || sh.size > size {
				sh.nextTick = e.Time.Add(in.Timeout)
			}
		default:
			for !sh.nextTick.IsZero() && sh.nextTick.Before(e.Time.Add(-tol)) {
				sh.nextTick = sh.nextTick.Add(in.Timeout)
			}
			onTick := !sh.nextTick.IsZero() && absDuration(e.Time.Sub(sh.nextTick)) <= tol
			switch {
			case onTick:
				f.Trigger = TriggerTimeout
				sh.nextTick = sh.nextTick.Add(in.Timeout)
			case i > lastArrival && in.Completed:
				f.Trigger = TriggerShutdown
			default:
				f.Trigger = TriggerTimeout
				b.Notes = append(b.Notes, "flush "+strconv.Itoa(e.Seq)+" is not on a replayed timer tick; it is counted as a timeout flush")
			}
		}
		sh.pending = max(sh.pending-items, 0)
		sh.size = max(sh.size-size, 0)
		if sh.pending == 0 {
			sh.size = 0
		}
		sh.flushes++
		f.Left = sh.pending
		for _, s := range sources[e.Seq] {
			f.Sources = append(f.Sources, s)
			if a, ok := at[s.Arrival]; ok && !a.IsZero() && !e.Time.IsZero() {
				w := ms(e.Time.Sub(a))
				if f.Wait == nil {
					f.Wait = &BatchWait{Min: w, Max: w}
				} else {
					f.Wait.Min, f.Wait.Max = math.Min(f.Wait.Min, w), math.Max(f.Wait.Max, w)
				}
			}
		}
		b.Flushes = append(b.Flushes, f)
	}
	pending := 0
	for _, sh := range shards.list {
		pending += sh.pending
	}
	b.Batchers, b.Notes = shards.report(b.Notes)
	if pending > 0 && in.Completed {
		b.Notes = append(b.Notes, strconv.Itoa(pending)+" items arrived without being sent")
	}
	if in.Settings.Bytes() {
		note := "batch::sizer is bytes: the replay measures each request serialized; arrivals, pending and left are shown in items"
		if bytesUnknown {
			note += ". Some requests were not recorded, so their bytes are missing and a size send may be counted as a timeout send"
		}
		b.Notes = append(b.Notes, note)
	}
	if qb {
		b.Verification = verifyQueueBatch(b, in.Metrics)
	} else {
		b.Verification = verifyTriggers(b, in.Metrics, hasTimer)
	}
	return b, nil
}

// verifyQueueBatch compares the sends with queue_batch's histogram of the
// items in each send: its count is the number of sends and its sum the
// items they held. The processor records it only with batching on.
func verifyQueueBatch(b *Batching, metrics []instrument.ComponentMetric) string {
	var count uint64
	var sum float64
	found := false
	for _, m := range metrics {
		if m.Component != b.Processor || m.Pipeline != b.Pipeline || m.Name != metricQueueBatchSend || m.Count == nil {
			continue
		}
		found = true
		count += *m.Count
		if m.Sum != nil {
			sum += *m.Sum
		}
	}
	if !found {
		return BatchUnverified
	}
	items := 0
	for _, f := range b.Flushes {
		items += f.Items
	}
	if count == uint64(len(b.Flushes)) && int64(sum) == int64(items) {
		return BatchMetricsAgree
	}
	b.Notes = append(b.Notes, "the processor counted "+strconv.FormatUint(count, 10)+" sends of "+strconv.FormatInt(int64(sum), 10)+" items in all; the run recorded "+strconv.Itoa(len(b.Flushes))+" sends of "+strconv.Itoa(items))
	return BatchMetricsDiffer
}

// batchSources counts, per flush seq, the records each arrival contributed,
// from the lineage hops across the processor.
func batchSources(in BatchInput) map[int][]BatchSource {
	counts := map[int]map[int]int{}
	for _, l := range in.Lineage {
		for _, h := range l.Hops {
			if h.Kind != instrument.KindEgress || h.Component != in.Processor || h.Pipeline != in.Pipeline || h.Parent < 0 || h.Parent >= len(l.Hops) {
				continue
			}
			p := l.Hops[h.Parent]
			if p.Kind != instrument.KindIngress || p.Component != in.Processor {
				continue
			}
			if counts[h.Seq] == nil {
				counts[h.Seq] = map[int]int{}
			}
			counts[h.Seq][p.Seq]++
		}
	}
	out := map[int][]BatchSource{}
	for flush, by := range counts {
		for arrival, n := range by {
			out[flush] = append(out[flush], BatchSource{Arrival: arrival, Items: n})
		}
		s := out[flush]
		for i := 1; i < len(s); i++ {
			for j := i; j > 0 && s[j].Arrival < s[j-1].Arrival; j-- {
				s[j], s[j-1] = s[j-1], s[j]
			}
		}
	}
	return out
}

func verifyTriggers(b *Batching, metrics []instrument.ComponentMetric, hasTimer bool) string {
	var size, timeout *int64
	for _, m := range metrics {
		if m.Component != b.Processor || m.Pipeline != b.Pipeline || m.IntValue == nil {
			continue
		}
		switch m.Name {
		case metricSizeTrigger:
			size = m.IntValue
		case metricTimeoutTrigger:
			timeout = m.IntValue
		}
	}
	if size == nil && timeout == nil {
		return BatchUnverified
	}
	var gotSize, gotTimeout int64
	for _, f := range b.Flushes {
		switch f.Trigger {
		case TriggerSize, TriggerImmediate:
			gotSize++
		case TriggerTimeout, TriggerShutdown:
			gotTimeout++
		}
	}
	if !hasTimer {
		// Without a timer every send counts as a size trigger.
		gotSize += gotTimeout
		gotTimeout = 0
	}
	if val(size) == gotSize && val(timeout) == gotTimeout {
		return BatchMetricsAgree
	}
	b.Notes = append(b.Notes, "the processor counted "+strconv.FormatInt(val(size), 10)+" size and "+strconv.FormatInt(val(timeout), 10)+" timeout sends; the replay inferred "+strconv.FormatInt(gotSize, 10)+" and "+strconv.FormatInt(gotTimeout, 10))
	return BatchMetricsDiffer
}

// Normalize removes wall-clock offsets and waits, which differ between
// executions; counts and triggers stay.
func (b *Batching) Normalize() {
	for i := range b.Arrivals {
		b.Arrivals[i].OffsetMS = nil
	}
	for i := range b.Flushes {
		b.Flushes[i].OffsetMS = nil
		b.Flushes[i].Wait = nil
	}
}

// serviceNames lists the service.name of each resource block of a payload.
func serviceNames(p telemetry.Payload) []string {
	out := make([]string, 0, p.Counts().Resources)
	for i := range p.Counts().Resources {
		res, _ := p.ResourceAt(i)
		name := ""
		if v, ok := res.Attributes().Get("service.name"); ok {
			name = v.AsString()
		}
		out = append(out, name)
	}
	return out
}

// distinctResources counts the different resources (attributes and schema
// URL) among a payload's resource blocks.
func distinctResources(p telemetry.Payload) int {
	seen := map[string]bool{}
	for i := range p.Counts().Resources {
		res, url := p.ResourceAt(i)
		seen[telemetry.Fingerprint(res.Attributes())+strconv.Quote(url)] = true
	}
	return len(seen)
}

func val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/float64(time.Millisecond)*10) / 10 }

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
