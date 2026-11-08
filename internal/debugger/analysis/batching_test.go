package analysis

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// batchEvents builds a batch stage's events: "in" arrivals and "out"
// flushes of n items at ms after the processor started.
type batchStep struct {
	kind  string
	ms    int
	items int
}

func batchEvents(steps []batchStep) []instrument.Event {
	t0 := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	events := []instrument.Event{{Seq: 1, Kind: instrument.KindComponentStarted, Component: "batch", Pipeline: "logs", Time: t0}}
	for i, s := range steps {
		kind := instrument.KindIngress
		if s.kind == "out" {
			kind = instrument.KindEgress
		}
		events = append(events, instrument.Event{
			Seq: i + 2, Kind: kind, Component: "batch", Pipeline: "logs", Signal: "logs",
			Time:   t0.Add(time.Duration(s.ms) * time.Millisecond),
			Counts: &telemetry.Counts{Resources: 1, Scopes: 1, Items: s.items},
		})
	}
	return events
}

func intp(v int64) *int64 { return &v }

// TestBatchInfersFlushTriggers: the trigger of each flush follows from the
// pending items, the configured sizes and the replayed timer, which restarts
// after every send and every tick.
func TestBatchInfersFlushTriggers(t *testing.T) {
	for _, tt := range []struct {
		name     string
		settings BatchSettings
		timeout  time.Duration
		steps    []batchStep
		metrics  [2]int64 // size, timeout trigger sends; -1 for none
		want     []string
		split    []bool
		verified string
	}{
		{
			name:     "size, timeout, split and shutdown",
			settings: BatchSettings{SendBatchSize: 4, SendBatchMaxSize: 6, Timeout: "300ms"},
			timeout:  300 * time.Millisecond,
			steps: []batchStep{
				{"in", 2, 2}, {"in", 2, 3}, {"out", 2, 5},
				// The timer restarted at 2ms: ticks at 302 (empty) and 602.
				{"in", 502, 1}, {"out", 603, 1},
				{"in", 1003, 8}, {"out", 1003, 6}, {"out", 1004, 2},
			},
			metrics:  [2]int64{2, 2},
			want:     []string{TriggerSize, TriggerTimeout, TriggerSize, TriggerShutdown},
			split:    []bool{false, false, true, false},
			verified: BatchMetricsAgree,
		},
		{
			name:     "no timer sends every request",
			settings: BatchSettings{SendBatchSize: 0, Timeout: "200ms"},
			timeout:  200 * time.Millisecond,
			steps:    []batchStep{{"in", 1, 2}, {"out", 1, 2}, {"in", 50, 1}, {"out", 50, 1}},
			metrics:  [2]int64{2, 0},
			want:     []string{TriggerImmediate, TriggerImmediate},
			split:    []bool{false, false},
			verified: BatchMetricsAgree,
		},
		{
			name:     "differing metrics are reported",
			settings: BatchSettings{SendBatchSize: 4, Timeout: "300ms"},
			timeout:  300 * time.Millisecond,
			steps:    []batchStep{{"in", 1, 1}, {"out", 300, 1}},
			metrics:  [2]int64{1, 0},
			want:     []string{TriggerTimeout},
			split:    []bool{false},
			verified: BatchMetricsDiffer,
		},
		{
			name:     "without metrics the triggers are unverified",
			settings: BatchSettings{SendBatchSize: 4, Timeout: "300ms"},
			timeout:  300 * time.Millisecond,
			steps:    []batchStep{{"in", 1, 1}, {"out", 300, 1}},
			metrics:  [2]int64{-1, -1},
			want:     []string{TriggerTimeout},
			split:    []bool{false},
			verified: BatchUnverified,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var metrics []instrument.ComponentMetric
			if tt.metrics[0] >= 0 {
				metrics = []instrument.ComponentMetric{
					{Component: "batch", Pipeline: "logs", Name: metricSizeTrigger, IntValue: intp(tt.metrics[0])},
					{Component: "batch", Pipeline: "logs", Name: metricTimeoutTrigger, IntValue: intp(tt.metrics[1])},
				}
			}
			b, err := Batch(BatchInput{
				Processor: "batch", Pipeline: "logs", Signal: "logs",
				Settings: tt.settings, Timeout: tt.timeout,
				Events: batchEvents(tt.steps), Metrics: metrics, Completed: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			var split []bool
			for _, f := range b.Flushes {
				got = append(got, f.Trigger)
				split = append(split, f.Split)
			}
			if !reflect.DeepEqual(got, tt.want) || !reflect.DeepEqual(split, tt.split) {
				t.Fatalf("triggers = %v split %v, want %v split %v (notes %v)", got, split, tt.want, tt.split, b.Notes)
			}
			if b.Verification != tt.verified {
				t.Fatalf("verification = %s, want %s (notes %v)", b.Verification, tt.verified, b.Notes)
			}
		})
	}
}

// TestBatchAttributesRecordsAndWaits: each flush names the arrivals its
// records came from (by lineage), how long they waited, and how many of
// its resource blocks are distinct resources.
func TestBatchAttributesRecordsAndWaits(t *testing.T) {
	events := batchEvents([]batchStep{{"in", 0, 1}, {"in", 100, 1}, {"out", 300, 2}})
	const payload = `{"resourceLogs":[` +
		`{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeLogs":[{"logRecords":[{"body":{"stringValue":"a"}}]}]},` +
		`{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeLogs":[{"logRecords":[{"body":{"stringValue":"b"}}]}]}]}`
	events[3].Snapshot = "sha256:out"
	events[3].Counts.Resources = 2
	snaps := instrument.Snapshots{"sha256:out": json.RawMessage(payload)}
	hops := func(in, out int) []Hop {
		return []Hop{
			{Parent: -1, Kind: instrument.KindInput},
			{Parent: 0, Seq: in, Kind: instrument.KindIngress, Component: "batch", Pipeline: "logs"},
			{Parent: 1, Seq: out, Kind: instrument.KindEgress, Component: "batch", Pipeline: "logs"},
		}
	}
	lineage := []RecordLineage{{ID: "a", Hops: hops(3, 4)}, {ID: "b", Hops: hops(2, 4)}}
	b, err := Batch(BatchInput{
		Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: BatchSettings{SendBatchSize: 4, Timeout: "300ms"}, Timeout: 300 * time.Millisecond,
		Events: events, Snapshots: snaps, Lineage: lineage, Completed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := b.Flushes[0]
	if want := []BatchSource{{Arrival: 2, Items: 1}, {Arrival: 3, Items: 1}}; !reflect.DeepEqual(f.Sources, want) {
		t.Fatalf("sources = %+v", f.Sources)
	}
	if f.Wait == nil || f.Wait.Min != 200 || f.Wait.Max != 300 {
		t.Fatalf("wait = %+v, want 200 to 300", f.Wait)
	}
	if !reflect.DeepEqual(f.Services, []string{"checkout", "checkout"}) {
		t.Fatalf("services = %v", f.Services)
	}
	if f.Resources != 2 || f.DistinctResources != 1 {
		t.Fatalf("resources = %d distinct %d, want 2 and 1", f.Resources, f.DistinctResources)
	}
	b.Normalize()
	if b.Flushes[0].OffsetMS != nil || b.Flushes[0].Wait != nil || b.Arrivals[0].OffsetMS != nil {
		t.Fatal("normalized batching keeps wall-clock values")
	}
}

// TestBatchReplaysQueueBatch: queue_batch restarts a batcher's timer when
// a pending batch starts (a request reaching an empty batcher, or what a
// size send left) and keeps ticking otherwise, and its sends are checked
// against its batch_send_size histogram.
func TestBatchReplaysQueueBatch(t *testing.T) {
	qb := BatchSettings{Type: TypeQueueBatch, SendBatchSize: 4, SendBatchMaxSize: 6, Timeout: "300ms", Sizer: "items"}
	for _, tt := range []struct {
		name     string
		settings BatchSettings
		steps    []batchStep
		count    int64 // batch_send_size count; -1 for none
		sum      float64
		want     []string
		verified string
	}{
		{
			name:     "timer restarts with each pending batch",
			settings: qb,
			steps: []batchStep{
				{"in", 2, 2}, {"in", 2, 3}, {"out", 2, 5},
				// The size send left nothing; the request at 502 starts a
				// pending batch and its timer: 802, not the batch
				// processor's tick at 602.
				{"in", 502, 1}, {"out", 802, 1},
				// The split leaves 2 items, whose timer starts at 1003.
				{"in", 1003, 8}, {"out", 1003, 6}, {"out", 1303, 2},
			},
			count: 4, sum: 14,
			want:     []string{TriggerSize, TriggerTimeout, TriggerSize, TriggerTimeout},
			verified: BatchMetricsAgree,
		},
		{
			name:     "differing histogram is reported",
			settings: qb,
			steps:    []batchStep{{"in", 1, 1}, {"out", 301, 1}},
			count:    2, sum: 1,
			want:     []string{TriggerTimeout},
			verified: BatchMetricsDiffer,
		},
		{
			name:     "without a batch section every request is sent",
			settings: BatchSettings{Type: TypeQueueBatch, Unbatched: true, Timeout: "0s"},
			steps:    []batchStep{{"in", 1, 2}, {"out", 1, 2}},
			count:    -1,
			want:     []string{TriggerImmediate},
			verified: BatchUnverified,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var metrics []instrument.ComponentMetric
			if tt.count >= 0 {
				c := uint64(tt.count)
				metrics = []instrument.ComponentMetric{{Component: "batch", Pipeline: "logs", Name: metricQueueBatchSend, Type: "histogram", Count: &c, Sum: &tt.sum}}
			}
			timeout, _ := time.ParseDuration(tt.settings.Timeout)
			b, err := Batch(BatchInput{
				Processor: "batch", Pipeline: "logs", Signal: "logs",
				Settings: tt.settings, Timeout: timeout,
				Events: batchEvents(tt.steps), Metrics: metrics, Completed: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range b.Flushes {
				got = append(got, f.Trigger)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("triggers = %v, want %v (notes %v)", got, tt.want, b.Notes)
			}
			if b.Verification != tt.verified {
				t.Fatalf("verification = %s, want %s (notes %v)", b.Verification, tt.verified, b.Notes)
			}
		})
	}
}

// TestBatchMeasuresQueueBatchBytes: with batch::sizer bytes the replay
// sizes each request serialized, so two requests that reach min_size
// together are a size send.
func TestBatchMeasuresQueueBatchBytes(t *testing.T) {
	const payload = `{"resourceLogs":[{"resource":{},"scopeLogs":[{"logRecords":[{"body":{"stringValue":"0123456789"}}]}]}]}`
	snaps := instrument.Snapshots{"sha256:r": json.RawMessage(payload), "sha256:two": json.RawMessage(
		`{"resourceLogs":[{"resource":{},"scopeLogs":[{"logRecords":[{"body":{"stringValue":"0123456789"}}]}]},{"resource":{},"scopeLogs":[{"logRecords":[{"body":{"stringValue":"0123456789"}}]}]}]}`)}
	one, err := snaps.Payload("sha256:r")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := one.Proto()
	if err != nil {
		t.Fatal(err)
	}
	events := batchEvents([]batchStep{{"in", 0, 1}, {"in", 10, 1}, {"out", 10, 2}})
	events[1].Snapshot, events[2].Snapshot, events[3].Snapshot = "sha256:r", "sha256:r", "sha256:two"
	b, err := Batch(BatchInput{
		Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: BatchSettings{Type: TypeQueueBatch, SendBatchSize: 2 * len(raw), Timeout: "300ms", Sizer: "bytes"},
		Timeout:  300 * time.Millisecond, Events: events, Snapshots: snaps, Completed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f := b.Flushes[0]; f.Trigger != TriggerSize || f.Left != 0 {
		t.Fatalf("flush = %+v (notes %v)", f, b.Notes)
	}
}
