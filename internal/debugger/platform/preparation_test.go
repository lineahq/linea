package platform

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	configpolicy "github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestQueuedQuickAndScenarioRunsUseAcceptedFilesAndEnvironment(t *testing.T) {
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			root, child, fixture := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "resource.yaml"), filepath.Join(dir, "input.json")
			config := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: '${file:" + child + "}'}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
			payload, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{root: []byte(config), child: []byte("attributes: [{key: captured, value: '${env:OTEL_QUEUE_CAPTURE}', action: upsert}]\n"), fixture: payload}
			t.Setenv("OTEL_QUEUE_CAPTURE", "accepted")
			req := engine.Request{Config: root, Fixture: fixture, Compare: true}
			if mode == ModeScenario {
				scenario := filepath.Join(dir, "scenario.yaml")
				files[scenario] = []byte("version: 1\nname: accepted scenario\ninputs: [{file: input.json}]\n")
				req.Fixture, req.Scenario = "", scenario
			}
			for path, raw := range files {
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			started, release := make(chan struct{}), make(chan struct{})
			calls := 0
			s := New(Options{Environment: configpolicy.EnvironmentPolicy{Host: []string{"OTEL_QUEUE_CAPTURE"}}, Paths: PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				calls++
				if calls == 1 {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
					return report(req, runresult.StatusCompleted), nil
				}
				return engine.Execute(ctx, req)
			}})
			t.Cleanup(s.Close)
			submit(t, s, engine.Request{ConfigYAML: config, FixtureJSON: string(payload)})
			receive(t, started)
			r := submit(t, s, req)
			if r.View(false).State != StateQueued {
				t.Fatal("run was not queued")
			}
			for path := range files {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Setenv("OTEL_QUEUE_CAPTURE", "changed"); err != nil {
				t.Fatal(err)
			}
			close(release)
			got := waitRun(t, r)
			if got.State != StateDone || got.Mode != mode || !got.Report.Comparison.Equal {
				t.Fatalf("captured run failed: %+v", got)
			}
			if len(got.Report.Result.Outputs) == 0 {
				t.Fatal("missing captured output")
			}
			for _, output := range got.Report.Result.Outputs {
				if !bytes.Contains(output.Data, []byte("accepted")) || bytes.Contains(output.Data, []byte("changed")) {
					t.Fatalf("queued source changed: %s", output.Data)
				}
			}
		})
	}
}

func TestPreparationFailureDoesNotAdmitOrConsumeCapacity(t *testing.T) {
	s := New(Options{QueueCapacity: 1})
	t.Cleanup(s.Close)
	if _, err := s.Submit(engine.Request{ConfigYAML: "invalid", FixtureJSON: "{}"}); err == nil {
		t.Fatal("invalid sources admitted")
	}
	if len(s.List()) != 0 {
		t.Fatal("preparation failure created history")
	}
	s.mu.Lock()
	preparing := s.preparing
	s.mu.Unlock()
	if preparing != 0 {
		t.Fatal("preparation reservation leaked")
	}
}

func TestPreparationIsBoundedAndCloseWaitsWithoutHoldingSchedulerLock(t *testing.T) {
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := openPlatform(t, memory.NewData(), Options{QueueCapacity: 1, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return engine.Request{}, ctx.Err()
	}, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		t.Error("unprepared work executed")
		return nil, nil
	}})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); s.Close() })
	finished := make(chan error, 1)
	go func() { _, err := s.Submit(request("preparing")); finished <- err }()
	receive(t, started)
	if _, err := s.Submit(request("over capacity")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("preparation capacity = %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("preparing request appeared as accepted")
	}
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	receive(t, cancelled)
	select {
	case <-closed:
		t.Fatal("close returned before preparation cleanup")
	default:
	}
	if _, err := s.repository.Get(context.Background(), strings.Repeat("0", 32)); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("repository closed before preparation cleanup: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := receive(t, finished); !errors.Is(err, ErrClosed) {
		t.Fatalf("close/admission = %v", err)
	}
	receive(t, closed)
}

func TestForegroundCancellationStopsPreparationWithoutAdmission(t *testing.T) {
	started := make(chan struct{})
	s := New(Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		close(started)
		<-ctx.Done()
		return engine.Request{}, ctx.Err()
	}})
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := s.Execute(ctx, request("cancel")); finished <- err }()
	receive(t, started)
	cancel()
	if err := receive(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatalf("preparation cancellation = %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("cancelled preparation admitted")
	}
}
