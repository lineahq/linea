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
