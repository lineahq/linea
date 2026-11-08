package platform

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// evidenceCountingRepository counts reads of run evidence.
type evidenceCountingRepository struct {
	artifact.Repository
	reads *atomic.Int64
}

func (r evidenceCountingRepository) RunEvidence(ctx context.Context, id string) ([]byte, error) {
	r.reads.Add(1)
	return r.Repository.RunEvidence(ctx, id)
}

// TestListRunsReadsATerminalRunsEvidenceOnce lists runs the service does not
// hold: the first list reads their evidence, later lists use the cached
// entries, which still follow Kept.
func TestListRunsReadsATerminalRunsEvidenceOnce(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	limits := artifact.Limits{Runs: 100, StorageBytes: 1 << 40, CatalogBytes: 1 << 30}
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}
	opts := Options{Prepare: unprepared, Execute: execute}
	s := openPlatformLimits(t, data, limits, opts)
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		ids = append(ids, waitRun(t, submit(t, s, request(name))).ID)
	}
	s.Close()

	var reads atomic.Int64
	reopened, err := Open(evidenceCountingRepository{memory.Open(data, limits), &reads}, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	first, err := reopened.ListRuns(ctx, testCaller, RunQuery{})
	if err != nil || len(first.Runs) != 3 || first.Runs[0].Listing == nil {
		t.Fatalf("first list = %+v, %v", first, err)
	}
	cold := reads.Load()
	if cold == 0 {
		t.Fatal("the first list read no evidence; the test does not exercise the cache")
	}
	second, err := reopened.ListRuns(ctx, testCaller, RunQuery{})
	if err != nil || len(second.Runs) != 3 {
		t.Fatalf("second list = %+v, %v", second, err)
	}
	if reads.Load() != cold {
		t.Fatalf("the second list read evidence %d more times", reads.Load()-cold)
	}
	for i := range first.Runs {
		if first.Runs[i].ID != second.Runs[i].ID || !reflect.DeepEqual(first.Runs[i].Listing, second.Runs[i].Listing) {
			t.Fatalf("entry %d changed: %+v, then %+v", i, first.Runs[i], second.Runs[i])
		}
	}

	// Keeping a run loads it; drop the handle so the list reads the record.
	if _, err := reopened.KeepRun(ctx, testCaller, ids[0]); err != nil {
		t.Fatal(err)
	}
	reopened.mu.Lock()
	delete(reopened.runs, ids[0])
	reopened.mu.Unlock()
	third, err := reopened.ListRuns(ctx, testCaller, RunQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range third.Runs {
		if r.ID == ids[0] && !r.Kept {
			t.Fatal("a kept run is listed as not kept")
		}
	}
}
