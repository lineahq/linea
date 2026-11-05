package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"
)

// The performance workloads (testdata/perf/README.md). The test
// keeps them valid and runnable; the benchmarks time them. Nothing here
// asserts a duration: timing is host-dependent.
var (
	perfDir        = filepath.Join("..", "..", "..", "testdata", "perf")
	perfComponents = filepath.Join(perfDir, "components-100.yaml")
	perfSyncLogs   = filepath.Join(perfDir, "sync-logs.yaml")
	perfLogs100    = filepath.Join(perfDir, "logs-100.json")
	perfLogs1000   = filepath.Join(perfDir, "logs-1000.json")
	perfExecutions = []struct {
		name, config, fixture string
		// delivered is the records each recorded exporter receives.
		delivered map[string]int
	}{
		{"W2", perfSyncLogs, perfLogs100, map[string]int{"otlp/backend": 75, "debug": 75}},
		{"W3", perfSyncLogs, perfLogs1000, map[string]int{"otlp/backend": 750, "debug": 750}},
		{"W4", perfComponents, perfLogs100, map[string]int{"otlp/a": 800, "otlp/b": 800, "debug": 800}},
	}
)

// TestPerformanceWorkloadsRun keeps the performance workloads from rotting: W1
// validates with its 100 components, and W2 to W4 complete and deliver what
// their configurations say.
func TestPerformanceWorkloadsRun(t *testing.T) {
	ins, err := Inspect(context.Background(), InspectRequest{Config: perfComponents})
	if err != nil {
		t.Fatal(err)
	}
	if !ins.Valid || ins.Topology == nil || len(ins.Topology.Components) != 100 {
		t.Fatalf("W1: valid=%v diagnostics=%v components=%d", ins.Valid, ins.Diagnostics, componentCount(ins))
	}
	for _, w := range perfExecutions {
		t.Run(w.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{Config: w.config, Fixture: w.fixture})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Result.Status != runresult.StatusCompleted {
				t.Fatalf("status = %s", rep.Result.Status)
			}
			got := map[string]int{}
			for _, o := range rep.Result.Outputs {
				got[o.Exporter] += o.Counts.Items
			}
			if len(got) != len(w.delivered) {
				t.Fatalf("outputs = %v, want %v", got, w.delivered)
			}
			for exp, n := range w.delivered {
				if got[exp] != n {
					t.Errorf("%s received %d records, want %d", exp, got[exp], n)
				}
			}
		})
	}
}

func componentCount(ins *Inspection) int {
	if ins.Topology == nil {
		return 0
	}
	return len(ins.Topology.Components)
}

// BenchmarkInspect times W1: resolving, validating and graphing 100
// components (static graph generation).
func BenchmarkInspect(b *testing.B) {
	for b.Loop() {
		ins, err := Inspect(context.Background(), InspectRequest{Config: perfComponents})
		if err != nil || !ins.Valid {
			b.Fatalf("inspect: %v", err)
		}
	}
}

// BenchmarkExecute times W2 to W4 in-process, Collector startup and
// analysis included (small fixture replay).
func BenchmarkExecute(b *testing.B) {
	for _, w := range perfExecutions {
		b.Run(w.name, func(b *testing.B) {
			for b.Loop() {
				rep, err := Execute(context.Background(), Request{Config: w.config, Fixture: w.fixture})
				if err != nil || rep.Result.Status != runresult.StatusCompleted {
					b.Fatalf("execute: %v", err)
				}
			}
		})
	}
}
