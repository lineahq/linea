package platform

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// cachedIDs returns the runs the service holds.
func (s *Service) cachedIDs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make(map[string]bool, len(s.runs))
	for id := range s.runs {
		ids[id] = true
	}
	return ids
}

// TestTheRunCacheIsBoundedByBytes: the cache keeps the most recently used
// runs whose evidence fits its byte budget, never fewer than its floor; a
// run it dropped reads back in full, a run read just now stays however much
// it weighs, and a run that just ended outranks older reads.
func TestTheRunCacheIsBoundedByBytes(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	execute := func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		if req.FixtureJSON == "held" {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return report(req, "completed"), nil
	}
	s := openPlatform(t, memory.NewData(), Options{Prepare: unprepared, Execute: execute})
	var handles []*Handle
	var weights []int64
	for n := range 20 {
		r := submit(t, s, request(fmt.Sprintf("run %d", n)))
		waitRun(t, r)
		r.mu.Lock()
		w, bundle := r.weight, r.bundle
		r.mu.Unlock()
		if w <= 0 || bundle != nil {
			t.Fatalf("run %d: weight %d, bundle kept %v", n, w, bundle != nil)
		}
		handles, weights = append(handles, r), append(weights, w)
	}
	if held := s.cachedRuns(); held != 20 {
		t.Fatalf("the default budget holds %d of 20 small runs", held)
	}
	originals := map[string]string{}
	for _, r := range handles {
		r.mu.Lock()
		events := mustJSON(t, r.events)
		r.mu.Unlock()
		originals[r.id] = mustJSON(t, r.View(true)) + events
	}

	// A budget of the ten newest runs: one more run leaves the newest runs
	// that fit together.
	var budget int64
	for _, w := range weights[10:] {
		budget += w
	}
	s.mu.Lock()
	s.runCacheBytes = budget
	s.mu.Unlock()
	last := submit(t, s, request("run 20"))
	waitRun(t, last)
	handles = append(handles, last)
	last.mu.Lock()
	weights = append(weights, last.weight)
	last.mu.Unlock()
	want, sum := map[string]bool{}, int64(0)
	for i := len(handles) - 1; i >= 0 && sum+weights[i] <= budget; i-- {
		sum += weights[i]
		want[handles[i].id] = true
	}
	if got := s.cachedIDs(); !maps.Equal(got, want) {
		t.Fatalf("cached %d runs, want the newest %d that fit %d bytes", len(got), len(want), budget)
	}

	// A budget smaller than any run keeps the floor: the newest runs.
	s.mu.Lock()
	s.runCacheBytes = 1
	s.evictLocked()
	s.mu.Unlock()
	if got := s.cachedIDs(); len(got) != runCacheFloor || !got[last.id] {
		t.Fatalf("over budget the cache holds %d runs (newest kept %v), want %d", len(got), got[last.id], runCacheFloor)
	}

	// A dropped run reads back in full and stays cached after the read,
	// although it alone outweighs the budget.
	first := handles[0]
	if _, err := s.GetRun(ctx, testCaller, first.id); err != nil {
		t.Fatal(err)
	}
	if got := s.cachedIDs(); !got[first.id] || len(got) != runCacheFloor {
		t.Fatalf("a run read just now was dropped: %v", got)
	}
	reread, err := s.handle(ctx, first.id)
	if err != nil {
		t.Fatal(err)
	}
	reread.mu.Lock()
	events := mustJSON(t, reread.events)
	reread.mu.Unlock()
	if reread == first || mustJSON(t, reread.View(true))+events != originals[first.id] {
		t.Fatal("a dropped run read back differs from what it was")
	}

	// Admit a run, read the floor's worth of old runs, then end the run: the
	// run that just ended outranks the oldest read.
	fresh := submit(t, s, request("held"))
	for _, r := range handles[1 : 1+runCacheFloor] {
		if _, err := s.GetRun(ctx, testCaller, r.id); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	waitRun(t, fresh)
	got := s.cachedIDs()
	if !got[fresh.id] || got[handles[1].id] || !got[handles[runCacheFloor].id] || len(got) != runCacheFloor {
		t.Fatalf("after a run ended the cache holds %v", got)
	}
}

// TestAStoredRunDropsItsBundle: once the repository holds a run's end, the
// handle keeps no raw bundle; the bundle still reads back whole from the
// repository and the run keeps its bundle identity.
func TestAStoredRunDropsItsBundle(t *testing.T) {
	ctx := context.Background()
	s := openPlatform(t, memory.NewData(), Options{})
	id, _ := scenarioRunSubmit(t, s, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig}, Quick: &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}}})
	r, err := s.handle(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	bundle, info, stored := r.bundle, r.bundleInfo, r.stored
	r.mu.Unlock()
	if !stored || bundle != nil || info == nil {
		t.Fatalf("stored %v, bundle kept %v, identity %+v", stored, bundle != nil, info)
	}
	raw, err := s.GetRunBundle(ctx, testCaller, id)
	if err != nil {
		t.Fatal(err)
	}
	read, err := engine.InspectBundle(raw)
	if err != nil || read.Digest != info.Digest || read.Bytes != info.Bytes {
		t.Fatalf("bundle read back = %+v, %v; want %+v", read, err, info)
	}
	if run, err := s.GetRun(ctx, testCaller, id); err != nil || run.Bundle == nil || run.Bundle.Digest != info.Digest {
		t.Fatalf("run bundle = %+v, %v", run.Bundle, err)
	}
}
