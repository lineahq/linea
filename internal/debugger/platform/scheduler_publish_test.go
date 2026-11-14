package platform

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// stalledEvidence is a repository whose writes of a run's evidence wait
// until resume is closed, as a stalled object store would.
type stalledEvidence struct {
	artifact.Repository
	entered chan struct{}
	resume  chan struct{}
}

func (r *stalledEvidence) SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence []byte) (*artifact.RunRecord, error) {
	if len(evidence) != 0 {
		r.entered <- struct{}{}
		select {
		case <-r.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.Repository.SaveRun(ctx, id, project, expected, terminal, index, metadata, evidence)
}

// TestAStalledPublicationKeepsItsLease: while a finished run's outcome is
// being written, the service keeps renewing the run's lease and answering
// other calls, and the run stays running until the write completes.
func TestAStalledPublicationKeepsItsLease(t *testing.T) {
	clock := newTestClock()
	repo := &stalledEvidence{Repository: memory.New(artifact.Limits{}), entered: make(chan struct{}, 1), resume: make(chan struct{})}
	s, err := Open(repo, Options{Now: clock.Now, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	t.Cleanup(func() {
		select {
		case <-repo.resume:
		default:
			close(repo.resume)
		}
	})
	r := submit(t, s, request("quick"))
	receive(t, repo.entered)

	// Without the write's end, three heartbeats go past the lease taken at
	// the claim.
	for range 3 {
		beat := make(chan struct{})
		go func() {
			s.heartbeat(clock.Advance(DefaultLease / 2))
			close(beat)
		}()
		receive(t, beat)
	}
	if _, err := s.Get(r.ID()); err != nil {
		t.Fatal(err)
	}
	if v := r.View(false); v.State != StateRunning || v.Attempt != 1 {
		t.Fatalf("run during the write = %s attempt %d, want running attempt 1", v.State, v.Attempt)
	}
	if expired, err := s.runQueue.Expired(context.Background(), clock.Now()); err != nil || len(expired) != 0 {
		t.Fatalf("expired = %+v, %v; want the lease renewed", expired, err)
	}

	close(repo.resume)
	if v := waitRun(t, r); v.State != StateDone || v.Attempt != 1 || v.Report == nil {
		t.Fatalf("run = %s attempt %d, report %v; want done on attempt 1", v.State, v.Attempt, v.Report != nil)
	}
	rec, err := repo.Run(context.Background(), r.ID())
	if err != nil || !rec.Terminal || rec.Bytes == 0 {
		t.Fatalf("record = %+v, %v; want terminal with evidence", rec, err)
	}
}

// stalledRepo opens a service on a repository whose evidence writes stall
// until resume is closed; the cleanup closes it if the test did not.
func stalledRepo(t *testing.T, clock *testClock) (*Service, *stalledEvidence) {
	t.Helper()
	repo := &stalledEvidence{Repository: memory.New(artifact.Limits{}), entered: make(chan struct{}, 1), resume: make(chan struct{})}
	s, err := Open(repo, Options{Now: clock.Now, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	t.Cleanup(func() {
		select {
		case <-repo.resume:
		default:
			close(repo.resume)
		}
	})
	return s, repo
}

// TestAnExpiryDuringThePublicationWins: a run whose lease ends while its
// outcome is being written is requeued, the late write is refused by the
// run's generation, and only the next attempt's outcome is published.
func TestAnExpiryDuringThePublicationWins(t *testing.T) {
	clock := newTestClock()
	s, repo := stalledRepo(t, clock)
	r := submit(t, s, request("quick"))
	receive(t, repo.entered)

	// No renewal: the lease taken at the claim ends, and this instance's
	// expiry requeues the run while finish still waits on the write.
	s.tendLeases(clock.Advance(2 * DefaultLease))
	if v := r.View(false); v.State != StateQueued {
		t.Fatalf("run after its lease ended = %s, want queued", v.State)
	}

	close(repo.resume)
	if v := waitRun(t, r); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("run = %s attempt %d; want done on attempt 2", v.State, v.Attempt)
	}
	rec, err := repo.Run(context.Background(), r.ID())
	if err != nil || !rec.Terminal {
		t.Fatalf("record = %+v, %v; want terminal", rec, err)
	}
	var m runMetadata
	if err := decodeStored(rec.Metadata, &m); err != nil || m.Attempt != 2 || m.State != StateDone {
		t.Fatalf("stored = %+v, %v; want done on attempt 2", m, err)
	}
}

// TestACancelDuringThePublicationLosesToIt: a cancel that arrives while a
// finished run's outcome is being written is accepted by the queue, and the
// written outcome stands.
func TestACancelDuringThePublicationLosesToIt(t *testing.T) {
	clock := newTestClock()
	s, repo := stalledRepo(t, clock)
	r := submit(t, s, request("quick"))
	receive(t, repo.entered)

	if err := r.Cancel(); err != nil {
		t.Fatalf("cancel during the write = %v", err)
	}
	if v := r.View(false); v.State != StateRunning {
		t.Fatalf("run during the write = %s, want running", v.State)
	}
	close(repo.resume)
	if v := waitRun(t, r); v.State != StateDone || v.Report == nil {
		t.Fatalf("run = %s, report %v; want done", v.State, v.Report != nil)
	}
	if rec, err := repo.Run(context.Background(), r.ID()); err != nil || !rec.Terminal || rec.Bytes == 0 {
		t.Fatalf("record = %+v, %v; want terminal with evidence", rec, err)
	}
}

// TestClosingDuringThePublicationKeepsTheOutcome: a shared instance that
// closes while a finished run's outcome is being written leaves the run to
// that write instead of handing it back, so the outcome is kept.
func TestClosingDuringThePublicationKeepsTheOutcome(t *testing.T) {
	data := memory.NewData()
	repo := &stalledEvidence{Repository: memory.Open(data, artifact.Limits{}), entered: make(chan struct{}, 1), resume: make(chan struct{})}
	s, err := Open(repo, Options{Shared: true, Now: newTestClock().Now, Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := submit(t, s, request("quick"))
	receive(t, repo.entered)

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	// Close hands back what it executes before it waits for the executions,
	// so by the time it waits on this one any handback has been written.
	waitFor(t, "close to stop the service", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closed
	})
	close(repo.resume)
	receive(t, closed)
	rec, err := memory.Open(data, artifact.Limits{}).Run(context.Background(), r.ID())
	if err != nil || !rec.Terminal || rec.Bytes == 0 {
		t.Fatalf("record = %+v, %v; want the outcome written", rec, err)
	}
}
