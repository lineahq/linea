package platform

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// gatedRuns is a repository whose run writes pass gate first, which may
// block or refuse them. op is "save" (with the written state's terminality
// and evidence), "start" or "requeue".
type gatedRuns struct {
	artifact.Repository
	gate func(ctx context.Context, op string, terminal bool, evidence []byte) error
}

var errRefused = errors.New("store refused the write")

func (g *gatedRuns) SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence []byte) (*artifact.RunRecord, error) {
	if err := g.gate(ctx, "save", terminal, evidence); err != nil {
		return nil, err
	}
	return g.Repository.SaveRun(ctx, id, project, expected, terminal, index, metadata, evidence)
}

func (g *gatedRuns) Start(ctx context.Context, a artifact.Assignment) error {
	if err := g.gate(ctx, "start", false, nil); err != nil {
		return err
	}
	return g.Repository.Start(ctx, a)
}

func (g *gatedRuns) Requeue(ctx context.Context, a artifact.Assignment, now time.Time) (uint64, error) {
	if err := g.gate(ctx, "requeue", false, nil); err != nil {
		return 0, err
	}
	return g.Repository.Requeue(ctx, a, now)
}

// openGated opens a service on a gated memory repository whose executions
// wait for release when it is not nil.
func openGated(t *testing.T, clock *testClock, release <-chan struct{}, gate func(context.Context, string, bool, []byte) error) *Service {
	t.Helper()
	repo := &gatedRuns{Repository: memory.New(artifact.Limits{}), gate: gate}
	s, err := Open(repo, Options{Now: clock.Now, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		if release != nil {
			<-release
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// unlocked fails the test unless the service's mu and run r's mu can both be
// taken, so no write holds them.
func unlocked(t *testing.T, s *Service, r *Handle) {
	t.Helper()
	taken := make(chan struct{})
	go func() {
		s.mu.Lock()
		r.mu.Lock()
		r.mu.Unlock()
		s.mu.Unlock()
		close(taken)
	}()
	receive(t, taken)
}

// TestRunWritesHoldNoLocks: starting a run, cancelling a queued run and
// requeueing a run whose lease ended write the repository while neither the
// service's mu nor the run's mu is held.
func TestRunWritesHoldNoLocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		// blocks reports whether the gate holds a write.
		blocks func(op string, terminal bool) bool
		// act brings the run to the blocked write; it may run in a goroutine.
		act func(t *testing.T, s *Service, clock *testClock, release chan struct{}) (*Handle, func())
	}{
		{
			name:   "start",
			blocks: func(op string, terminal bool) bool { return op == "save" && !terminal },
			act: func(t *testing.T, s *Service, _ *testClock, _ chan struct{}) (*Handle, func()) {
				return submit(t, s, request("quick")), func() {}
			},
		},
		{
			name:   "cancel a queued run",
			blocks: func(op string, terminal bool) bool { return op == "save" && terminal },
			act: func(t *testing.T, s *Service, _ *testClock, release chan struct{}) (*Handle, func()) {
				// The first run fills the only slot, so the second waits.
				first := submit(t, s, request("first"))
				waitFor(t, "the first run to start", func() bool { return first.View(false).State == StateRunning })
				queued := submit(t, s, request("queued"))
				go func() { _ = queued.Cancel() }()
				return queued, func() { close(release) }
			},
		},
		{
			name:   "requeue",
			blocks: func(op string, _ bool) bool { return op == "requeue" },
			act: func(t *testing.T, s *Service, clock *testClock, release chan struct{}) (*Handle, func()) {
				r := submit(t, s, request("quick"))
				waitFor(t, "the run to start", func() bool { return r.View(false).State == StateRunning })
				go s.tendLeases(clock.Advance(2 * DefaultLease))
				return r, func() { close(release) }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newTestClock()
			entered, resume := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			release := make(chan struct{})
			s := openGated(t, clock, release, func(ctx context.Context, op string, terminal bool, _ []byte) error {
				if !tc.blocks(op, terminal) {
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
			r, done := tc.act(t, s, clock, release)
			receive(t, entered)
			unlocked(t, s, r)
			close(resume)
			done()
			select {
			case <-release:
			default:
				close(release)
			}
		})
	}
}

// TestAnOutcomeTheStoreRefusesKeepsTheRunRunning: while the repository
// refuses a finished run's outcome, the run stays running rather than
// ending as an error the repository does not hold; once it accepts, the run
// ends as executed.
func TestAnOutcomeTheStoreRefusesKeepsTheRunRunning(t *testing.T) {
	var refusing atomic.Bool
	var refused atomic.Int32
	refusing.Store(true)
	s := openGated(t, newTestClock(), nil, func(_ context.Context, op string, terminal bool, _ []byte) error {
		if op == "save" && terminal && refusing.Load() {
			refused.Add(1)
			return errRefused
		}
		return nil
	})
	r := submit(t, s, request("quick"))
	// Two rounds of the outcome and its fallback.
	waitFor(t, "the outcome to be refused twice", func() bool { return refused.Load() >= 4 })
	if v := r.View(false); v.State != StateRunning || v.Error != "" {
		t.Fatalf("run while refused = %s %q, want running", v.State, v.Error)
	}
	refusing.Store(false)
	if v := waitRun(t, r); v.State != StateDone || v.Report == nil {
		t.Fatalf("run = %s, report %v; want done", v.State, v.Report != nil)
	}
	if rec, err := s.repository.Run(context.Background(), r.ID()); err != nil || !rec.Terminal || rec.Bytes == 0 {
		t.Fatalf("record = %+v, %v; want terminal with evidence", rec, err)
	}
}

// TestACancelTheStoreRefusesEndsTheRunLater: cancelling a queued run whose
// end the repository refuses reports a persistence error and leaves the run
// queued; its end is written once the repository accepts it.
func TestACancelTheStoreRefusesEndsTheRunLater(t *testing.T) {
	var refusing atomic.Bool
	refusing.Store(true)
	release := make(chan struct{})
	s := openGated(t, newTestClock(), release, func(_ context.Context, op string, terminal bool, _ []byte) error {
		if op == "save" && terminal && refusing.Load() {
			return errRefused
		}
		return nil
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	first := submit(t, s, request("first"))
	waitFor(t, "the first run to start", func() bool { return first.View(false).State == StateRunning })
	queued := submit(t, s, request("queued"))
	if err := queued.Cancel(); !errors.Is(err, ErrPersistence) {
		t.Fatalf("cancel while refused = %v, want a persistence error", err)
	}
	if v := queued.View(false); v.State != StateQueued {
		t.Fatalf("run while refused = %s, want queued", v.State)
	}
	refusing.Store(false)
	if v := waitRun(t, queued); v.State != StateCancelled {
		t.Fatalf("run = %s, want cancelled", v.State)
	}
	if rec, err := s.repository.Run(context.Background(), queued.ID()); err != nil || !rec.Terminal {
		t.Fatalf("record = %+v, %v; want terminal", rec, err)
	}
}
