package recommend

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

func at(ms float64) *float64 { return &ms }

func containsAll(t *testing.T, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("%q does not contain %q", got, w)
		}
	}
}

// tenantBatching is a batch processor with metadata_keys [tenant], send
// batch size 4 and a 200ms timeout, over the arrivals given (batcher index,
// offset, items) and the flushes given as (batcher, items).
func tenantBatching(arrivals [][3]float64, flushes [][2]int, batchers ...analysis.BatchBatcher) *analysis.Batching {
	b := &analysis.Batching{Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: analysis.BatchSettings{SendBatchSize: 4, Timeout: "200ms", MetadataKeys: []string{"tenant"}},
		Batchers: batchers}
	for i, a := range arrivals {
		b.Arrivals = append(b.Arrivals, analysis.BatchArrival{Seq: i, OffsetMS: at(a[1]), Items: int(a[2]), Batcher: int(a[0])})
	}
	for i, f := range flushes {
		b.Flushes = append(b.Flushes, analysis.BatchFlush{Seq: 100 + i, Batcher: f[0], Items: f[1]})
	}
	return b
}

func batcher(items, flushes, arrivals int) analysis.BatchBatcher {
	return analysis.BatchBatcher{Metadata: map[string][]string{"tenant": {"t"}}, Arrivals: arrivals, Items: items, Flushes: flushes}
}

// TestTinyBatchersCompareWithOneBatcher: the rule fires only when the
// batchers sent smaller batches than one batcher would on the same
// arrivals, skips batchers of unknown metadata and says so.
func TestTinyBatchersCompareWithOneBatcher(t *testing.T) {
	// Two tenants alternate 2-item requests half a second apart. Each
	// waits out its own timer and sends 2 items four times; one batcher
	// fills 4 items from both and sends twice.
	spread := [][3]float64{{0, 0, 2}, {1, 5, 2}, {0, 500, 2}, {1, 505, 2}}
	cases := []struct {
		name     string
		b        *analysis.Batching
		want     []string
		coverage string
	}{
		{"split sends are smaller",
			tenantBatching(spread, [][2]int{{0, 2}, {1, 2}, {0, 2}, {1, 2}}, batcher(4, 2, 2), batcher(4, 2, 2)),
			[]string{"into 2 batchers", "2.0 log records per send on average (4 sends)", "one batcher holding the same 4 requests would have sent 4.0 log records per send (2 sends)", "batcher 0 2.0, batcher 1 2.0", "no change is suggested"}, ""},
		{"split sends fill as well",
			tenantBatching([][3]float64{{0, 0, 4}, {1, 5, 4}}, [][2]int{{0, 4}, {1, 4}}, batcher(4, 1, 1), batcher(4, 1, 1)),
			nil, ""},
		{"one batcher only",
			tenantBatching([][3]float64{{0, 0, 2}, {0, 500, 2}}, [][2]int{{0, 2}, {0, 2}}, batcher(4, 2, 2)),
			nil, ""},
		{"unknown batcher is left out",
			tenantBatching(append(slices.Clone(spread), [3]float64{2, 10, 1}), [][2]int{{0, 2}, {1, 2}, {0, 2}, {1, 2}, {2, 1}},
				batcher(4, 2, 2), batcher(4, 2, 2), analysis.BatchBatcher{Metadata: map[string][]string{}, Unknown: true, Arrivals: 1, Items: 1, Flushes: 1}),
			[]string{"into 2 batchers", "same 4 requests"}, "Not compared: 1 request in a batcher of unknown metadata"},
		{"normalized arrivals carry no offsets",
			func() *analysis.Batching {
				b := tenantBatching(spread, [][2]int{{0, 2}, {1, 2}, {0, 2}, {1, 2}}, batcher(4, 2, 2), batcher(4, 2, 2))
				for i := range b.Arrivals {
					b.Arrivals[i].OffsetMS = nil
				}
				return b
			}(), nil, ""},
		{"without metadata_keys",
			func() *analysis.Batching {
				b := tenantBatching(spread, [][2]int{{0, 4}, {0, 4}})
				b.Settings.MetadataKeys = nil
				return b
			}(), nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := tinyBatchers(Input{Topology: graph(map[string][]string{"logs": {"batch"}}), Batching: []*analysis.Batching{c.b}})
			if c.want == nil {
				if len(recs) != 0 {
					t.Fatalf("recs = %+v", recs)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("recs = %+v", recs)
			}
			r := recs[0]
			containsAll(t, r.Summary, c.want...)
			if r.Kind != KindInefficiency || r.Confidence != ConfidenceObserved || r.Pipeline != "logs" || r.Change != nil || r.Benefit != nil {
				t.Fatalf("rec = %+v", r)
			}
			if !slices.Equal(r.Components, []string{"processor/batch"}) || r.Evidence[0] != (Evidence{SectionBatching, "processor/batch"}) {
				t.Fatalf("components %q evidence %+v", r.Components, r.Evidence)
			}
			if c.coverage != "" {
				containsAll(t, strings.Join(r.Coverage, " "), c.coverage)
			}
		})
	}
}

// TestOneBatcherReplayFollowsTheBatchProcessor: the timer starts with the
// processor, restarts after a size send, sends at most send_batch_max_size
// and, without a timer, sends every request on arrival.
func TestOneBatcherReplayFollowsTheBatchProcessor(t *testing.T) {
	all := func(int) bool { return true }
	cases := []struct {
		name       string
		settings   analysis.BatchSettings
		timeout    time.Duration
		arrivals   [][2]float64 // offset, items
		final      bool
		sends, got int
	}{
		{"size send and a final send", analysis.BatchSettings{SendBatchSize: 4}, 200 * time.Millisecond, [][2]float64{{0, 2}, {5, 2}, {6, 1}}, true, 2, 5},
		{"timer send between arrivals", analysis.BatchSettings{SendBatchSize: 100}, 200 * time.Millisecond, [][2]float64{{0, 2}, {450, 3}}, true, 2, 5},
		{"timer restarts after a size send", analysis.BatchSettings{SendBatchSize: 2}, 200 * time.Millisecond, [][2]float64{{190, 2}, {300, 1}, {380, 1}}, true, 2, 4},
		{"max size splits a send", analysis.BatchSettings{SendBatchSize: 2, SendBatchMaxSize: 3}, time.Second, [][2]float64{{0, 7}}, true, 3, 7},
		{"no timer sends every request", analysis.BatchSettings{SendBatchSize: 100}, 0, [][2]float64{{0, 1}, {1, 1}}, true, 2, 2},
		{"unsent items stay pending", analysis.BatchSettings{SendBatchSize: 4}, 200 * time.Millisecond, [][2]float64{{0, 2}}, false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := &analysis.Batching{Settings: c.settings, Batchers: []analysis.BatchBatcher{{}}}
			for _, a := range c.arrivals {
				b.Arrivals = append(b.Arrivals, analysis.BatchArrival{OffsetMS: at(a[0]), Items: int(a[1])})
			}
			got, ok := oneBatcher(b, c.timeout, all, c.final)
			if !ok || got.sends != c.sends || got.items != c.got {
				t.Fatalf("replay = %+v ok=%v, want %d sends of %d items", got, ok, c.sends, c.got)
			}
		})
	}
}

// TestBatchDropsClientContext: a lost metadata read behind one batch
// processor is fixed by its metadata_keys; other reads by moving a processor
// reader before the batch when only batch processors sit between them.
func TestBatchDropsClientContext(t *testing.T) {
	addr := []ClientRead{{What: "address", How: "pod_association is empty"}}
	tenant := []ClientRead{{What: "metadata", Key: "Tenant", How: `OTTL path otelcol.client.metadata["Tenant"]`}}
	cases := []struct {
		name       string
		processors []string
		loss       ContextLoss
		op         Op
		change     []string // processors for a swap, or the value
		blocker    string
	}{
		{"adjacent", []string{"memory_limiter", "batch", "k8sattributes"},
			ContextLoss{Component: "processor/k8sattributes", Batch: "batch", Batches: []string{"batch"}, Lost: addr},
			OpSwap, []string{"memory_limiter", "k8sattributes", "batch"}, ""},
		{"behind chained batches", []string{"batch", "batch/2", "k8sattributes", "resource"},
			ContextLoss{Component: "processor/k8sattributes", Batch: "batch", Batches: []string{"batch", "batch/2"}, Lost: addr},
			OpSwap, []string{"k8sattributes", "batch", "batch/2", "resource"}, ""},
		{"metadata kept by metadata_keys", []string{"batch", "transform"},
			ContextLoss{Component: "processor/transform", Batch: "batch", MetadataKeys: []string{"region"}, Batches: []string{"batch"}, Lost: tenant},
			OpSetOption, []string{`["region","tenant"]`}, ""},
		{"metadata behind two batches", []string{"batch", "resource", "batch/2", "transform"},
			ContextLoss{Component: "processor/transform", Batch: "batch", Batches: []string{"batch", "batch/2"}, Lost: tenant},
			"", nil, "would have to list tenant"},
		{"processors between", []string{"batch", "resource", "k8sattributes"},
			ContextLoss{Component: "processor/k8sattributes", Batch: "batch", Batches: []string{"batch"}, Lost: addr},
			"", nil, "resource sit between"},
		{"exporter", []string{"batch"},
			ContextLoss{Component: "exporter/otlp", Batch: "batch", Batches: []string{"batch"}, Lost: []ClientRead{{What: "auth", Key: "sub", How: "headers_setter headers[0] (X-Sub) from_attribute"}}},
			"", nil, "cannot run before a processor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.loss.Pipeline = "logs"
			got := batchDropsClientContext(Input{Topology: graph(map[string][]string{"logs": c.processors}), ContextLosses: []ContextLoss{c.loss}})
			if len(got) != 1 {
				t.Fatalf("recs = %+v", got)
			}
			r := got[0]
			if r.Kind != KindRisk || r.Confidence != ConfidenceStatic || r.Pipeline != "logs" || !slices.Equal(r.Components, []string{"processor/batch", c.loss.Component}) {
				t.Fatalf("rec = %+v", r)
			}
			switch c.op {
			case "":
				if r.Change != nil || len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], c.blocker) {
					t.Fatalf("change %+v blockers %q", r.Change, r.Blockers)
				}
			case OpSwap:
				if r.Change == nil || r.Change.Op != OpSwap || !slices.Equal(r.Change.Processors, c.change) {
					t.Fatalf("change = %+v, want swap to %q", r.Change, c.change)
				}
			case OpSetOption:
				if r.Change == nil || r.Change.Op != OpSetOption || r.Change.Component != "batch" || r.Change.Option != "metadata_keys" || r.Change.Value != c.change[0] || len(r.Coverage) != 2 {
					t.Fatalf("change = %+v coverage %q", r.Change, r.Coverage)
				}
			}
			if r.Evidence[0] != (Evidence{SectionTopology, "pipeline/logs"}) || !strings.Contains(r.Summary, "client context") {
				t.Fatalf("evidence %+v summary %q", r.Evidence, r.Summary)
			}
		})
	}
	if got := batchDropsClientContext(Input{Topology: graph(map[string][]string{"logs": {"batch"}}), ContextLosses: []ContextLoss{{Pipeline: "logs", Component: "exporter/otlp", Batch: "batch", Batches: []string{"batch"}}}}); len(got) != 0 {
		t.Fatalf("nothing lost: %+v", got)
	}
}

// grouped is a groupbyattrs run of two requests whose blocks it kept apart.
func grouped(processor string) *analysis.Grouping {
	out := 0
	return &analysis.Grouping{Processor: processor, Pipeline: "logs", Signal: "logs", Keys: []string{"k"},
		Requests: []analysis.GroupRequest{
			{Input: 1, Output: &out, Before: make([]analysis.GroupBlockIn, 2), After: make([]analysis.GroupBlockOut, 2)},
			{Input: 2, Output: &out, Before: make([]analysis.GroupBlockIn, 1), After: make([]analysis.GroupBlockOut, 1)},
		}}
}

// batchesOf is a batch processor with one send of the given resource
// blocks and distinct resources, which arrived as 6 items.
func batchesOf(resources, distinct int) *analysis.Batching {
	return &analysis.Batching{Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: analysis.BatchSettings{SendBatchSize: 6, Timeout: "1s"},
		Arrivals: []analysis.BatchArrival{{Seq: 1, Items: 4, Resources: 2}, {Seq: 2, Items: 2, Resources: 1}},
		Flushes:  []analysis.BatchFlush{{Seq: 3, Items: 6, Resources: resources, DistinctResources: distinct, Trigger: analysis.TriggerSize}}}
}

// TestGroupingAfterBatch: the run shows the merge exactly when the batch
// processor follows groupbyattrs directly and its sends are fully recorded;
// otherwise the recommendation is static and says what is missing.
func TestGroupingAfterBatch(t *testing.T) {
	cases := []struct {
		name       string
		processors []string
		grouping   []*analysis.Grouping
		batching   []*analysis.Batching
		want       Confidence // empty for no recommendation
		summary    []string
		coverage   string
		change     []string
		blocker    string
	}{
		{"observed", []string{"groupbyattrs", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, []*analysis.Batching{batchesOf(3, 1)},
			ConfidenceObserved,
			[]string{"it received 3 resource blocks in 2 requests and emitted 3", "sent 3 resource blocks in 1 send, 1 of them distinct", "1 resource block instead of 3 blocks"},
			"Observed on 2 requests", []string{"batch", "groupbyattrs"}, ""},
		{"nothing repeats within a send", []string{"groupbyattrs", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, []*analysis.Batching{batchesOf(3, 3)},
			"", nil, "", nil, ""},
		{"topology alone", []string{"groupbyattrs", "batch"}, nil, nil,
			ConfidenceStatic, []string{"regroups one request at a time"}, "Read from the configuration alone", []string{"batch", "groupbyattrs"}, ""},
		{"no batching evidence", []string{"groupbyattrs", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, nil,
			ConfidenceStatic, nil, "The run holds no batching evidence", []string{"batch", "groupbyattrs"}, ""},
		{"processors between", []string{"groupbyattrs", "transform", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, []*analysis.Batching{batchesOf(3, 1)},
			ConfidenceStatic, nil, "Processors between them may change", []string{"transform", "batch", "groupbyattrs"}, "transform would then run before grouping"},
		{"unsent items", []string{"groupbyattrs", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, []*analysis.Batching{func() *analysis.Batching {
			b := batchesOf(3, 1)
			b.Flushes[0].Items = 4
			return b
		}()},
			ConfidenceStatic, nil, "2 items arrived at the batch processor without being sent", []string{"batch", "groupbyattrs"}, ""},
		{"unrecorded resources", []string{"groupbyattrs", "batch"}, []*analysis.Grouping{grouped("groupbyattrs")}, []*analysis.Batching{batchesOf(3, 0)},
			ConfidenceStatic, nil, "were not recorded", []string{"batch", "groupbyattrs"}, ""},
		{"after batch already", []string{"batch", "groupbyattrs"}, nil, nil, "", nil, "", nil, ""},
		{"no batch", []string{"groupbyattrs", "attributes"}, nil, nil, "", nil, "", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := groupingAfterBatch(Input{Topology: graph(map[string][]string{"logs": c.processors}), Grouping: c.grouping, Batching: c.batching})
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("recs = %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("recs = %+v", got)
			}
			r := got[0]
			if r.Kind != KindOrdering || r.Confidence != c.want || r.Pipeline != "logs" || !slices.Equal(r.Components, []string{"processor/groupbyattrs", "processor/batch"}) {
				t.Fatalf("rec = %+v", r)
			}
			if r.Change == nil || r.Change.Op != OpSwap || !slices.Equal(r.Change.Processors, c.change) {
				t.Fatalf("change = %+v, want swap to %q", r.Change, c.change)
			}
			containsAll(t, r.Summary, c.summary...)
			containsAll(t, strings.Join(r.Coverage, " "), c.coverage)
			if c.blocker != "" {
				containsAll(t, strings.Join(r.Blockers, " "), c.blocker)
			} else if len(r.Blockers) != 0 {
				t.Fatalf("blockers = %q", r.Blockers)
			}
			if c.want == ConfidenceObserved {
				want := []Evidence{{SectionTopology, "pipeline/logs"}, {SectionGrouping, "processor/groupbyattrs"}, {SectionBatching, "processor/batch"}}
				if !slices.Equal(r.Evidence, want) {
					t.Fatalf("evidence = %+v", r.Evidence)
				}
				if r.Benefit == nil || *r.Benefit != (Benefit{MetricResourcesMerged, 2}) {
					t.Fatalf("benefit = %+v", r.Benefit)
				}
			} else if r.Benefit != nil {
				t.Fatalf("benefit = %+v", r.Benefit)
			}
		})
	}
}
