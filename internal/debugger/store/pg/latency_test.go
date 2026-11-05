package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// latencyBound is the median the queue's overhead must stay under. A lost
// change hint falls back to the one-second follow interval, which this
// catches without timing a single run.
const latencyBound = 500 * time.Millisecond

// TestARunSubmittedOnOneInstanceStartsAndEndsPromptlyOnAWorkerOfAnother
// measures what the shared queue adds to a run: a run submitted on instance
// a, executed by a worker connected to a (the baseline) or to b, as a client
// watching on a sees it.
func TestARunSubmittedOnOneInstanceStartsAndEndsPromptlyOnAWorkerOfAnother(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name string
		toA  int
	}{{"worker on the same instance", 1}, {"worker on the other instance", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			m := measureLatency(t, f, tc.toA, 5)
			for _, s := range []struct {
				what string
				ds   []time.Duration
				max  time.Duration
			}{
				{"submit to started (server)", m.dispatch, latencyBound},
				{"finished to seen done on a", m.seenDone, latencyBound},
				{"submit to seen done on a", m.total, 0},
			} {
				med, worst := medianMax(s.ds)
				t.Logf("%s: median %v, max %v", s.what, med.Round(time.Millisecond), worst.Round(time.Millisecond))
				if s.max > 0 && med > s.max {
					t.Errorf("%s: median %v, want under %v", s.what, med, s.max)
				}
			}
		})
	}
}

type latencies struct{ dispatch, seenDone, total []time.Duration }

// measureLatency runs n runs one after another on a worker of one slot that
// the balancer connects to a when toA is 1 and to b when it is 0. Each run is
// submitted on a, and a watcher on a waits on its change signal, without
// polling, until it ends. A first, unmeasured run warms the bucket: the first
// write to a fresh SeaweedFS bucket allocates a volume, which takes seconds.
func measureLatency(t *testing.T, f fixture, toA, n int) latencies {
	t.Helper()
	a, b := openInstance(t, f, 10*time.Second), openInstance(t, f, 10*time.Second)
	lb := balancer(t, a, b, toA)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	workers, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := worker.Connect(workers, worker.ConnectOptions{URL: lb.URL, Token: gateToken, Name: "team", Capacity: 1}); err != nil {
			t.Errorf("worker: %v", err)
		}
	}()
	defer func() { stop(); wg.Wait() }()
	registerGateRuntime(ctx, t, a, 1)

	sample := filepath.Join("..", "..", "..", "..", "samples", "logs-cleanup")
	input, err := filepath.Abs(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario := fmt.Sprintf("version: 1\nname: once\ninputs:\n  - receiver: otlp\n    file: %q\n", filepath.ToSlash(input))
	caller := platform.Local("latency")
	var m latencies
	for i := range n + 1 {
		submitted := time.Now()
		run, err := a.service.SubmitRun(ctx, caller, platform.RunRequest{Mode: platform.ModeScenario, Runtime: "team",
			Config: platform.Source{Path: filepath.Join(sample, "config.yaml")}, Scenario: &platform.ScenarioInput{Scenario: platform.Source{Text: scenario}}})
		if err != nil {
			t.Fatal(err)
		}
		var v platform.Run
		for {
			w, err := a.service.WatchRun(ctx, caller, run.ID, platform.EventCursor{})
			if err != nil {
				t.Fatal(err)
			}
			if v, err = a.service.GetRun(ctx, caller, run.ID); err != nil {
				t.Fatal(err)
			}
			if v.Finished != nil {
				break
			}
			select {
			case <-w.Changed:
			case <-ctx.Done():
				t.Fatalf("run %s still %s", run.ID, v.State)
			}
		}
		seen := time.Now()
		if v.State != platform.StateDone || v.Started == nil {
			t.Fatalf("run %s ended %s: %s", v.ID, v.State, v.Error)
		}
		if i == 0 {
			continue
		}
		m.dispatch = append(m.dispatch, v.Started.Sub(v.Created))
		m.seenDone = append(m.seenDone, seen.Sub(*v.Finished))
		m.total = append(m.total, seen.Sub(submitted))
	}
	return m
}

// registerGateRuntime registers the workers' runtime as "team" on r and waits
// until r counts the given number of its workers.
func registerGateRuntime(ctx context.Context, t *testing.T, r *instance, workers int) {
	t.Helper()
	hello, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	caller := platform.Local("latency")
	if _, err := r.service.RegisterRuntime(ctx, caller, platform.RegisterRuntimeRequest{
		Name: "team", Image: "registry.example/linea-runtime:team", Hello: raw}); err != nil {
		t.Fatal(err)
	}
	for {
		info, err := r.service.GetRuntime(ctx, caller, "team")
		if err != nil {
			t.Fatal(err)
		}
		if info.Catalog.Workers == workers && !info.Catalog.Confirmed.IsZero() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%d workers connected, want %d", info.Catalog.Workers, workers)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func medianMax(ds []time.Duration) (time.Duration, time.Duration) {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2], s[len(s)-1]
}
