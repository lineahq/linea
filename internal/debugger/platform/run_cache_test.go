package platform

import (
	"context"
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// historyBlindRepository fails a test that reads the whole run history.
type historyBlindRepository struct {
	artifact.Repository
	t *testing.T
}

func (r historyBlindRepository) Runs(context.Context) ([]artifact.RunRecord, error) {
	r.t.Error("the service read every run")
	return nil, errors.New("history read")
}

func (s *Service) cachedRuns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// TestRunCacheStaysBoundedAsHistoryGrows is the run cache gate: while
// history grows to 10,000 runs a service holds at most its cache, a cold
// run is read on demand, and a service opened on that history reads only
// its unfinished runs.
func TestRunCacheStaysBoundedAsHistoryGrows(t *testing.T) {
	history, cache := 10_000, 16
	if testing.Short() {
		history = 1_000
	}
	ctx := context.Background()
	data := memory.NewData()
	limits := artifact.Limits{Runs: history, StorageBytes: 1 << 40, CatalogBytes: 1 << 30}
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	opts := Options{Prepare: unprepared, Execute: execute, RunCache: cache}
	s := openPlatformLimits(t, data, limits, opts)
	var first string
	for n := 1; n <= history; n++ {
		r := submit(t, s, request("x"))
		waitRun(t, r)
		if n == 1 {
			first = r.ID()
		}
		if held := s.cachedRuns(); held > cache {
			t.Fatalf("after %d runs the service holds %d, more than its cache of %d", n, held, cache)
		}
	}
	if run, err := s.GetRun(ctx, testCaller, first); err != nil || run.State != StateDone {
		t.Fatalf("cold run = %+v, %v", run, err)
	}
	s.Close()

	reopened, err := Open(historyBlindRepository{memory.Open(data, limits), t}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	if held := reopened.cachedRuns(); held != 0 {
		t.Fatalf("reopened service holds %d runs, want only unfinished ones", held)
	}
	page, err := reopened.ListRuns(ctx, testCaller, RunQuery{Limit: 1})
	if err != nil || len(page.Runs) != 1 || page.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	if held := reopened.cachedRuns(); held != 0 {
		t.Fatalf("listing cached %d runs", held)
	}
	if _, err := reopened.GetRun(ctx, testCaller, first); err != nil {
		t.Fatal(err)
	}
	if held := reopened.cachedRuns(); held != 1 {
		t.Fatalf("after one read the service holds %d runs", held)
	}
}

// TestRunCacheReadsDeletedAndUnknownRunsAsBefore keeps the answers for runs
// the cache does not hold.
func TestRunCacheReadsDeletedAndUnknownRunsAsBefore(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	opts := Options{Prepare: unprepared, Execute: execute, RunCache: 1}
	s := openPlatform(t, data, opts)
	a, b := waitRun(t, submit(t, s, request("a"))), waitRun(t, submit(t, s, request("b")))
	if held := s.cachedRuns(); held != 1 {
		t.Fatalf("cache holds %d runs", held)
	}
	// a was evicted; deleting it reads it first.
	if err := s.DeleteRun(ctx, testCaller, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, testCaller, a.ID); codeOf(err) != CodeNotFound || err.Error() != "run was deleted" {
		t.Fatalf("deleted run = %v", err)
	}
	if _, err := s.GetRun(ctx, testCaller, "run-0000000000000000"); codeOf(err) != CodeNotFound {
		t.Fatalf("unknown run = %v", err)
	}
	if evaluations, err := s.ListEvaluations(ctx, testCaller, b.ID); err != nil || len(evaluations) != 0 {
		t.Fatalf("evaluations = %v, %v", evaluations, err)
	}
}
