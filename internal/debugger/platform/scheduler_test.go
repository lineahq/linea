package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// testClock is the scheduler's injected clock; heartbeats run only when a
// test calls Service.heartbeat.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// blockingExecute signals each execution it starts and returns when its
// context ends, with the context's cause.
func blockingExecute(started chan<- engine.Request) ExecuteFunc {
	return func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
}

// crashWithRuns admits a running and a queued run on a service whose
// process then dies: its store handle ends and its leases are never renewed.
func crashWithRuns(t *testing.T, data *memory.Data, clock *testClock) (running, queued string) {
	t.Helper()
	started := make(chan engine.Request, 1)
	crashed, kill := openKillable(t, data, Options{Now: clock.Now, Execute: blockingExecute(started)})
	active := submit(t, crashed, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	waiting := submit(t, crashed, engine.Request{ConfigYAML: bundleConfig, ScenarioYAML: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"})
	kill()
	return active.ID(), waiting.ID()
}

func TestRecoveredRunsResumeFromTheirBundles(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	runningID, queuedID := crashWithRuns(t, data, clock)
	executed := make(chan engine.Request, 2)
	s := openPlatform(t, data, Options{Now: clock.Now, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return report(req, "completed"), nil
	}})

	// The queued run executes from its bundle at once.
	queued, err := s.Get(queuedID)
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, queued); v.State != StateDone || v.Attempt != 1 {
		t.Fatalf("queued run = %s attempt %d, %v", v.State, v.Attempt, queued.Err())
	}
	if req := receive(t, executed); req.RunID != queuedID || !req.Prepared() {
		t.Fatalf("executed %q, prepared %v", req.RunID, req.Prepared())
	}

	// The run the dead worker held waits for its lease, then runs again.
	running, err := s.Get(runningID)
	if err != nil {
		t.Fatal(err)
	}
	if v := running.View(false); v.State != StateRunning || v.Attempt != 1 {
		t.Fatalf("held run = %s attempt %d", v.State, v.Attempt)
	}
	s.heartbeat(clock.Now())
	select {
	case req := <-executed:
		t.Fatalf("executed %s before its lease ended", req.RunID)
	default:
	}
	s.heartbeat(clock.Advance(DefaultLease + time.Second))
	if v := waitRun(t, running); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("requeued run = %s attempt %d, %v", v.State, v.Attempt, running.Err())
	}
	if req := receive(t, executed); req.RunID != runningID {
		t.Fatalf("executed %q", req.RunID)
	}
}

func TestRunIsLostWhenItsLastLeaseEnds(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	runningID, queuedID := crashWithRuns(t, data, clock)
	started := make(chan engine.Request, 2)
	s, kill := openKillable(t, data, Options{Now: clock.Now, Execute: blockingExecute(started)})
	if req := receive(t, started); req.RunID != queuedID {
		t.Fatalf("executed %q", req.RunID)
	}
	queued, err := s.Get(queuedID)
	if err != nil {
		t.Fatal(err)
	}
	if err := queued.Cancel(); err != nil {
		t.Fatal(err)
	}
	waitRun(t, queued)

	s.heartbeat(clock.Advance(DefaultLease + time.Second))
	r, err := s.Get(runningID)
	if err != nil {
		t.Fatal(err)
	}
	if req := receive(t, started); req.RunID != runningID {
		t.Fatalf("executed %q", req.RunID)
	}
	if v := r.View(false); v.State != StateRunning || v.Attempt != 2 {
		t.Fatalf("second attempt = %s attempt %d", v.State, v.Attempt)
	}

	// This worker stops renewing: its lease ends on the run's last attempt.
	ctx := context.Background()
	expired, err := s.runQueue.Expired(ctx, clock.Advance(2*DefaultLease))
	if err != nil || len(expired) != 1 || expired[0].RunID != runningID {
		t.Fatalf("expired = %+v, %v", expired, err)
	}
	s.expire(expired[0], clock.Now())
	v := waitRun(t, r)
	if v.State != StateLost || !errors.Is(r.Err(), ErrLost) {
		t.Fatalf("lost run = %s, %v", v.State, r.Err())
	}
	if _, err := s.runQueue.Assignment(ctx, runningID); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("lost run still queued: %v", err)
	}
	kill()

	// The outcome survives a restart.
	reopened := openPlatform(t, data, Options{Now: clock.Now})
	again, err := reopened.Get(runningID)
	if err != nil {
		t.Fatal(err)
	}
	if v := again.View(false); v.State != StateLost || v.Attempt != 2 || !errors.Is(again.Err(), ErrLost) {
		t.Fatalf("recovered = %s attempt %d, %v", v.State, v.Attempt, again.Err())
	}
}

func TestQueueCancellationStopsTheRunningRun(t *testing.T) {
	clock := newTestClock()
	started := make(chan engine.Request, 1)
	s := New(Options{Now: clock.Now, Execute: blockingExecute(started)})
	t.Cleanup(s.Close)
	r := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	// Another instance asks for cancellation through the queue; this worker
	// sees it on its next heartbeat.
	if _, err := s.runQueue.RequestCancel(context.Background(), r.ID()); err != nil {
		t.Fatal(err)
	}
	s.heartbeat(clock.Now())
	if v := waitRun(t, r); v.State != StateError || !errors.Is(r.Err(), context.Canceled) {
		t.Fatalf("cancelled run = %s, %v", v.State, r.Err())
	}
	if _, err := s.runQueue.Assignment(context.Background(), r.ID()); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("ended run still queued: %v", err)
	}
}

func TestSchedulerClaimsRunsInAdmissionOrderAcrossRuntimes(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	s := New(Options{Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		<-release
		mu.Lock()
		order = append(order, req.RunID)
		mu.Unlock()
		return report(req, "completed"), nil
	}})
	t.Cleanup(s.Close)
	var handles []*Handle
	for range 4 {
		handles = append(handles, submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}))
	}
	close(release)
	for _, h := range handles {
		waitRun(t, h)
	}
	for i, h := range handles {
		if order[i] != h.ID() {
			t.Fatalf("execution order %v", order)
		}
	}
	for _, h := range handles {
		if _, err := s.runQueue.Assignment(context.Background(), h.ID()); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("ended run still queued: %v", err)
		}
	}
}
