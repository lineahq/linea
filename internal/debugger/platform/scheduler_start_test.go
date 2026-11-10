package platform

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// startFailing is a repository whose queue refuses the first starts and
// releases it is asked for, as a store with a passing fault would.
type startFailing struct {
	artifact.Repository
	mu                    sync.Mutex
	failStarts, failFrees int
	starts, releases      int
}

func (f *startFailing) Start(ctx context.Context, a artifact.Assignment) error {
	f.mu.Lock()
	f.starts++
	fail := f.starts <= f.failStarts
	f.mu.Unlock()
	if fail {
		return artifact.ErrStorage
	}
	return f.Repository.Start(ctx, a)
}

func (f *startFailing) Release(ctx context.Context, a artifact.Assignment) (uint64, error) {
	f.mu.Lock()
	f.releases++
	fail := f.releases <= f.failFrees
	f.mu.Unlock()
	if fail {
		return 0, artifact.ErrStorage
	}
	return f.Repository.Release(ctx, a)
}

// TestRunWhoseStartFailsReturnsToTheQueue: a store error while recording a
// claimed run's start hands the run back instead of leaving its lease to be
// renewed forever, and a failed hand-back is tried again on renewal. The
// run then completes on a later claim, without spending an attempt.
func TestRunWhoseStartFailsReturnsToTheQueue(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		failStarts, failFrees int
	}{
		{"released at once", 1, 0},
		{"released on renewal", 1, 1},
		{"start fails twice", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &startFailing{Repository: memory.New(artifact.Limits{}), failStarts: tc.failStarts, failFrees: tc.failFrees}
			s, err := Open(repo, Options{Prepare: unprepared, Lease: 300 * time.Millisecond,
				Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
					return report(req, runresult.StatusCompleted), nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			r := submit(t, s, request("quick"))
			if v := waitRun(t, r); v.State != StateDone || v.Attempt != tc.failStarts+1 {
				t.Fatalf("run = %s on attempt %d, want done on attempt %d", v.State, v.Attempt, tc.failStarts+1)
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if repo.starts != tc.failStarts+1 || repo.releases != tc.failStarts+tc.failFrees {
				t.Errorf("starts %d, releases %d", repo.starts, repo.releases)
			}
		})
	}
}
