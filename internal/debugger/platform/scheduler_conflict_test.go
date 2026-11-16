package platform

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// agrees fails the test unless the run's handle on s holds the generation
// and terminality of the repository's record.
func agrees(t *testing.T, s *Service, id string) {
	t.Helper()
	waitFor(t, "the handle to follow the repository", func() bool {
		rec, err := s.repository.Run(context.Background(), id)
		if err != nil {
			return false
		}
		r, err := s.Get(id)
		if err != nil {
			return false
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.generation == rec.Generation && r.terminalLocked() == rec.Terminal
	})
}

// TestConcurrentTransitionsResolveInTheStore: a run's outcome and the
// requeue of its expired lease race; whichever the repository accepts
// first decides, the other is refused as a conflict, and the run's handle
// follows the repository rather than its own write.
func TestConcurrentTransitionsResolveInTheStore(t *testing.T) {
	t.Run("the requeue first", func(t *testing.T) {
		clock := newTestClock()
		entered, resume := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		s := openGated(t, clock, nil, func(ctx context.Context, op string, terminal bool, _ []byte) error {
			if op != "save" || !terminal {
				return nil
			}
			blocked := false
			once.Do(func() { blocked = true })
			if !blocked {
				return nil
			}
			entered <- struct{}{}
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		r := submit(t, s, request("quick"))
		// The first attempt's outcome is being written when its lease ends.
		receive(t, entered)
		s.tendLeases(clock.Advance(2 * DefaultLease))
		close(resume)
		// The outcome of the first attempt is refused; the requeued run runs
		// again and its second outcome is accepted.
		if v := waitRun(t, r); v.State != StateDone || v.Attempt != 2 {
			t.Fatalf("run = %s attempt %d, want done on attempt 2", v.State, v.Attempt)
		}
		agrees(t, s, r.ID())
	})
	t.Run("the outcome first", func(t *testing.T) {
		clock := newTestClock()
		entered, resume := make(chan struct{}, 1), make(chan struct{})
		release := make(chan struct{})
		s := openGated(t, clock, release, func(ctx context.Context, op string, _ bool, _ []byte) error {
			if op != "requeue" {
				return nil
			}
			entered <- struct{}{}
			select {
			case <-resume:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		r := submit(t, s, request("quick"))
		waitFor(t, "the run to start", func() bool { return r.View(false).State == StateRunning })
		tended := make(chan struct{})
		go func() {
			s.tendLeases(clock.Advance(2 * DefaultLease))
			close(tended)
		}()
		// The requeue is being written when the run's outcome is.
		receive(t, entered)
		close(release)
		if v := waitRun(t, r); v.State != StateDone || v.Attempt != 1 {
			t.Fatalf("run = %s attempt %d, want done on attempt 1", v.State, v.Attempt)
		}
		close(resume)
		receive(t, tended)
		if v := r.View(false); v.State != StateDone || v.Attempt != 1 {
			t.Fatalf("run after the refused requeue = %s attempt %d", v.State, v.Attempt)
		}
		agrees(t, s, r.ID())
	})
}

// TestATransitionWriteHasADeadline: every write of a run's transition ends
// within its bound, so a hung store cannot hold a transition forever:
// metadata within a third of the lease, an outcome with its evidence
// within publishTimeout and a hand-back within releaseTimeout.
func TestATransitionWriteHasADeadline(t *testing.T) {
	type write struct {
		op       string
		terminal bool
	}
	var mu sync.Mutex
	left := map[write]time.Duration{}
	record := func(ctx context.Context, op string, terminal bool) {
		d := time.Duration(-1)
		if deadline, ok := ctx.Deadline(); ok {
			d = time.Until(deadline)
		}
		mu.Lock()
		defer mu.Unlock()
		if _, seen := left[write{op, terminal}]; !seen {
			left[write{op, terminal}] = d
		}
	}
	clock := newTestClock()
	release := make(chan struct{})
	var refuse sync.Once
	s := openGated(t, clock, release, func(ctx context.Context, op string, terminal bool, _ []byte) error {
		record(ctx, op, terminal)
		if op == "start" {
			refused := false
			refuse.Do(func() { refused = true })
			if refused {
				return errRefused // the run is handed back
			}
		}
		return nil
	})
	r := submit(t, s, request("quick"))
	waitFor(t, "the run to start", func() bool { return r.View(false).State == StateRunning })
	s.tendLeases(clock.Advance(2 * DefaultLease))
	close(release)
	waitRun(t, r)
	transition := s.transitionTimeout()
	for _, tc := range []struct {
		write write
		bound time.Duration
	}{
		{write{"start", false}, transition},
		{write{"save", false}, transition},
		{write{"requeue", false}, transition},
		{write{"release", false}, releaseTimeout},
		{write{"save", true}, publishTimeout},
	} {
		mu.Lock()
		d, seen := left[tc.write]
		mu.Unlock()
		if !seen {
			t.Errorf("%+v was not written", tc.write)
			continue
		}
		if d <= 0 || d > tc.bound {
			t.Errorf("%+v had %v left, want a deadline within %v", tc.write, d, tc.bound)
		}
	}
}

// TestAnotherInstancesRequeueRefusesTheOutcome: instances sharing a
// repository race on one run. b requeues the run whose lease a let expire
// and runs it again while a still executes it; a's late outcome is refused
// as a conflict, and both instances' handles follow the repository's record
// of b's attempt.
func TestAnotherInstancesRequeueRefusesTheOutcome(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	started, release := make(chan engine.Request, 1), make(chan struct{})
	a := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		<-release // a worker that never learns its lease ended
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	run := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)

	executed := make(chan engine.Request, 1)
	b := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return report(req, runresult.StatusCompleted), nil
	}})
	b.heartbeat(clock.Advance(DefaultLease + time.Second))
	if req := receive(t, executed); req.RunID != run.ID() {
		t.Fatalf("b executed %q", req.RunID)
	}
	onB, err := b.Get(run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, onB); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("b's run = %s attempt %d, want done on attempt 2", v.State, v.Attempt)
	}
	close(release)
	if v := waitRun(t, run); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("a's run = %s attempt %d, want b's attempt", v.State, v.Attempt)
	}
	agrees(t, a, run.ID())
	agrees(t, b, run.ID())
	rec, err := a.repository.Run(context.Background(), run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if want := onB.View(false); rec.Generation == 0 || !rec.Terminal || want.Attempt != 2 {
		t.Fatalf("record = %+v", rec)
	}
}
