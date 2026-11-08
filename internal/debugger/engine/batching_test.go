package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// TestBatchingExplainsEveryTrigger runs samples/batch-timeline on the real
// batch processor: its requests are timed so that the size is reached, the
// timer fires, a burst is split at send_batch_max_size and the rest leaves
// at shutdown. The inferred triggers agree with the processor's own trigger
// metrics, and the first flush carries three resource blocks of two
// services, since batching does not merge equal resources.
func TestBatchingExplainsEveryTrigger(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "batch-timeline")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Batching) != 1 {
		t.Fatalf("batching = %d entries, want 1", len(rep.Batching))
	}
	b := rep.Batching[0]
	if want := (analysis.BatchSettings{SendBatchSize: 4, SendBatchMaxSize: 6, Timeout: "300ms", MetadataCardinalityLimit: 1000}); !reflect.DeepEqual(b.Settings, want) {
		t.Fatalf("settings = %+v", b.Settings)
	}
	var triggers []string
	var items []int
	for _, f := range b.Flushes {
		triggers = append(triggers, f.Trigger)
		items = append(items, f.Items)
	}
	if want := []string{analysis.TriggerSize, analysis.TriggerTimeout, analysis.TriggerSize, analysis.TriggerShutdown}; !reflect.DeepEqual(triggers, want) {
		t.Fatalf("triggers = %v, want %v (notes %v)", triggers, want, b.Notes)
	}
	if want := []int{5, 1, 6, 3}; !reflect.DeepEqual(items, want) {
		t.Fatalf("items = %v, want %v", items, want)
	}
	if b.Verification != analysis.BatchMetricsAgree {
		t.Fatalf("verification = %s (notes %v)", b.Verification, b.Notes)
	}
	if f := b.Flushes[0]; f.Resources != 3 || f.DistinctResources != 2 || len(f.Sources) != 3 {
		t.Fatalf("first flush = %+v, want 3 blocks of 2 resources from 3 arrivals", f)
	}
	if got := b.Flushes[0].Services; !reflect.DeepEqual(got, []string{"checkout", "cart", "checkout"}) {
		t.Fatalf("first flush services = %v", got)
	}
	if got := b.Arrivals[1].Services; !reflect.DeepEqual(got, []string{"cart"}) {
		t.Fatalf("second arrival services = %v", got)
	}
	if !b.Flushes[2].Split || b.Flushes[2].Left != 3 {
		t.Fatalf("third flush = %+v, want a split leaving 3", b.Flushes[2])
	}
	// The timer restarts after each send, so the lone log waits less than
	// the timeout.
	if w := b.Flushes[1].Wait; w == nil || w.Max >= 300 {
		t.Fatalf("timeout flush wait = %+v, want under 300ms", w)
	}
}

// TestBatchingKeepsOneBatcherPerMetadataValue runs samples/batch-metadata:
// the tenant header reaches the batch processor through include_metadata,
// so acme fills its own batch of 4 while globex and the request without a
// tenant wait in batchers of their own and leave on their own timers.
// Without include_metadata the same requests share one batcher.
func TestBatchingKeepsOneBatcherPerMetadataValue(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "batch-metadata")
	config := filepath.Join(dir, "config.yaml")
	scenario := filepath.Join(dir, "scenario.yaml")
	run := func(t *testing.T, config string) *analysis.Batching {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		rep, err := Execute(ctx, Request{Config: config, Scenario: scenario})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Batching) != 1 {
			t.Fatalf("batching = %d entries, want 1", len(rep.Batching))
		}
		return rep.Batching[0]
	}
	summary := func(b *analysis.Batching) (batchers []int, flushes []string) {
		for _, a := range b.Arrivals {
			batchers = append(batchers, a.Batcher)
		}
		for _, f := range b.Flushes {
			flushes = append(flushes, f.Trigger+":"+strings.Repeat("x", f.Items)+":"+string(rune('0'+f.Batcher)))
		}
		return batchers, flushes
	}

	t.Run("include_metadata", func(t *testing.T) {
		b := run(t, config)
		want := []analysis.BatchBatcher{
			{Metadata: map[string][]string{"tenant": {"acme"}}, Arrivals: 2, Items: 4, Flushes: 1},
			{Metadata: map[string][]string{"tenant": {"globex"}}, Arrivals: 1, Items: 2, Flushes: 1},
			{Metadata: map[string][]string{"tenant": {}}, Arrivals: 1, Items: 1, Flushes: 1},
		}
		if !reflect.DeepEqual(b.Batchers, want) {
			t.Fatalf("batchers = %+v, want %+v (notes %v)", b.Batchers, want, b.Notes)
		}
		arrivals, flushes := summary(b)
		if want := []int{0, 1, 0, 2}; !reflect.DeepEqual(arrivals, want) {
			t.Fatalf("arrival batchers = %v, want %v", arrivals, want)
		}
		// globex's and the no-tenant timers fire within a millisecond of
		// each other, in either order.
		slices.Sort(flushes)
		if want := []string{"size:xxxx:0", "timeout:x:2", "timeout:xx:1"}; !reflect.DeepEqual(flushes, want) {
			t.Fatalf("flushes = %v, want %v (notes %v)", flushes, want, b.Notes)
		}
		if b.Verification != analysis.BatchMetricsAgree {
			t.Fatalf("verification = %s (notes %v)", b.Verification, b.Notes)
		}
	})

	t.Run("without include_metadata", func(t *testing.T) {
		raw, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		dropped := strings.ReplaceAll(string(raw), "include_metadata: true", "include_metadata: false")
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(dropped), 0o600); err != nil {
			t.Fatal(err)
		}
		b := run(t, path)
		if len(b.Batchers) != 1 || !reflect.DeepEqual(b.Batchers[0].Metadata, map[string][]string{"tenant": {}}) || b.Batchers[0].Arrivals != 4 {
			t.Fatalf("batchers = %+v, want one without metadata", b.Batchers)
		}
		_, flushes := summary(b)
		if want := []string{"size:xxxx:0", "timeout:xxx:0"}; !reflect.DeepEqual(flushes, want) {
			t.Fatalf("flushes = %v, want %v (notes %v)", flushes, want, b.Notes)
		}
		if len(b.Notes) == 0 || !strings.Contains(b.Notes[0], "include_metadata") {
			t.Fatalf("notes = %v, want the dropped headers", b.Notes)
		}
	})
}
