package recommend

import (
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// queueBatchSettings is a queue_batch processor's batch section in the
// batch processor's terms (min_size, flush_timeout).
func queueBatchSettings(minSize int, flush string) analysis.BatchSettings {
	return analysis.BatchSettings{Type: analysis.TypeQueueBatch, SendBatchSize: minSize, Timeout: flush, Sizer: "items"}
}

// TestRulesNameQueueBatchSettings: the batching rules speak in queue_batch's
// own option names and suggest batch::partition::metadata_keys, and a
// queue_batch without a batch section counts as no batching.
func TestRulesNameQueueBatchSettings(t *testing.T) {
	t.Run("size never reached", func(t *testing.T) {
		b := &analysis.Batching{Processor: "queue_batch", Pipeline: "logs", Signal: "logs",
			Settings: queueBatchSettings(8192, "200ms"),
			Arrivals: []analysis.BatchArrival{{Seq: 1, OffsetMS: at(0), Items: 20}, {Seq: 2, OffsetMS: at(1000), Items: 20}},
			Flushes:  []analysis.BatchFlush{{Items: 20, Trigger: analysis.TriggerTimeout}, {Items: 20, Trigger: analysis.TriggerTimeout}}}
		recs := sizeNeverReached(Input{Batching: []*analysis.Batching{b}})
		if len(recs) != 1 {
			t.Fatalf("recs = %+v", recs)
		}
		containsAll(t, recs[0].Summary, "batch::min_size 8192 fills after", "the 200ms batch::flush_timeout sent every batch")

		b.Settings.Sizer = "bytes"
		recs = sizeNeverReached(Input{Batching: []*analysis.Batching{b}})
		if len(recs) != 1 || strings.Contains(recs[0].Summary, "rate") {
			t.Fatalf("recs = %+v", recs)
		}
		containsAll(t, recs[0].Summary, "never reaching batch::min_size 8192 bytes")
	})

	t.Run("client context", func(t *testing.T) {
		tenant := []ClientRead{{What: "metadata", Key: "Tenant", How: `OTTL path otelcol.client.metadata["Tenant"]`}}
		recs := batchDropsClientContext(Input{Topology: graph(map[string][]string{"logs": {"queue_batch", "transform"}}),
			ContextLosses: []ContextLoss{{Pipeline: "logs", Component: "processor/transform", Batch: "queue_batch", Batches: []string{"queue_batch"}, Lost: tenant}}})
		if len(recs) != 1 {
			t.Fatalf("recs = %+v", recs)
		}
		r := recs[0]
		if r.Change == nil || r.Change.Option != "batch::partition::metadata_keys" || r.Change.Value != `["tenant"]` {
			t.Fatalf("change = %+v", r.Change)
		}
		containsAll(t, r.Summary, "sends a batch joining several requests with a new context", "its batch::partition::metadata_keys is empty")
		containsAll(t, strings.Join(r.Coverage, " "), "a request sent alone", "batch::partition::cache_size")
	})

	t.Run("chained batching", func(t *testing.T) {
		g := graph(map[string][]string{"logs": {"batch", "queue_batch"}})
		if recs := batchChained(Input{Topology: g}); len(recs) != 1 || !strings.Contains(recs[0].Summary, "batches 2 times") {
			t.Fatalf("recs = %+v", recs)
		}
		unbatched := &analysis.Batching{Processor: "queue_batch", Pipeline: "logs", Settings: analysis.BatchSettings{Type: analysis.TypeQueueBatch, Unbatched: true}}
		if recs := batchChained(Input{Topology: g, Batching: []*analysis.Batching{unbatched}}); len(recs) != 0 {
			t.Fatalf("an unbatched queue_batch batches nothing: %+v", recs)
		}
	})

	t.Run("tiny batchers", func(t *testing.T) {
		b := tenantBatching([][3]float64{{0, 0, 2}, {1, 5, 2}, {0, 500, 2}, {1, 505, 2}}, [][2]int{{0, 2}, {1, 2}, {0, 2}, {1, 2}}, batcher(4, 2, 2), batcher(4, 2, 2))
		b.Processor = "queue_batch"
		b.Settings.Type, b.Settings.Sizer = analysis.TypeQueueBatch, "items"
		recs := tinyBatchers(Input{Batching: []*analysis.Batching{b}})
		if len(recs) != 1 {
			t.Fatalf("recs = %+v", recs)
		}
		containsAll(t, recs[0].Summary, "by batch::partition::metadata_keys tenant", "fills batch::min_size 4", "200ms batch::flush_timeout")
		b.Settings.Sizer = "bytes"
		if recs := tinyBatchers(Input{Batching: []*analysis.Batching{b}}); len(recs) != 0 {
			t.Fatalf("bytes are not replayed: %+v", recs)
		}
	})
}

// TestOneBatcherReplayFollowsQueueBatch: queue_batch restarts its timer
// when a request arrives at an empty batcher, where the batch processor
// keeps ticking from its start.
func TestOneBatcherReplayFollowsQueueBatch(t *testing.T) {
	all := func(int) bool { return true }
	arrivals := [][2]float64{{150, 1}, {320, 1}}
	for _, c := range []struct {
		settings analysis.BatchSettings
		sends    int
	}{
		// batch: ticks at 200 (sends 1) and 400 (sends 1).
		{analysis.BatchSettings{SendBatchSize: 100}, 2},
		// queue_batch: the first request restarts the timer to 350, which
		// sends both.
		{queueBatchSettings(100, "200ms"), 1},
	} {
		b := &analysis.Batching{Settings: c.settings, Batchers: []analysis.BatchBatcher{{}}}
		for _, a := range arrivals {
			b.Arrivals = append(b.Arrivals, analysis.BatchArrival{OffsetMS: at(a[0]), Items: int(a[1])})
		}
		got, ok := oneBatcher(b, 200*time.Millisecond, all, true)
		if !ok || got.sends != c.sends || got.items != 2 {
			t.Fatalf("type %q: replay = %+v, want %d sends", c.settings.Type, got, c.sends)
		}
	}
}
