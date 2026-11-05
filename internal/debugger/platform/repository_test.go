package platform

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// countingRepository is another Repository implementation: it forwards to a
// file store and counts run writes, so the test sees the platform use only
// the interface.
type countingRepository struct {
	artifact.Repository
	runWrites atomic.Int32
}

func (r *countingRepository) AdmitRun(ctx context.Context, a artifact.Admission) (*artifact.RunRecord, error) {
	r.runWrites.Add(1)
	return r.Repository.AdmitRun(ctx, a)
}

func (r *countingRepository) SaveRun(ctx context.Context, id, project string, expected uint64, terminal bool, index artifact.RunIndex, metadata, evidence []byte) (*artifact.RunRecord, error) {
	r.runWrites.Add(1)
	return r.Repository.SaveRun(ctx, id, project, expected, terminal, index, metadata, evidence)
}

// TestOpenRunsOnAnyRepository opens the platform on a repository wrapped by a
// test double, runs and saves through it, and recovers from it.
func TestOpenRunsOnAnyRepository(t *testing.T) {
	data := memory.NewData()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}
	open := func() (*Service, *countingRepository) {
		store := memory.Open(data, artifact.Limits{})
		repository := &countingRepository{Repository: store}
		s, err := Open(repository, Options{Prepare: unprepared, Execute: execute})
		if err != nil {
			t.Fatal(err)
		}
		return s, repository
	}
	s, repository := open()
	run := waitRun(t, submit(t, s, request("quick")))
	if _, err := s.SaveArtifact(context.Background(), testCaller, SaveArtifactRequest{WorkspaceID: workspaceID(t, s), Kind: artifact.Scenario, Content: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if repository.runWrites.Load() == 0 {
		t.Fatal("the run was not written through the repository")
	}
	s.Close()
	s, _ = open()
	t.Cleanup(s.Close)
	if got, err := s.GetRun(context.Background(), testCaller, run.ID); err != nil || got.State != StateDone {
		t.Fatalf("recovered run = %+v, %v", got, err)
	}
	if page, err := s.ListArtifacts(context.Background(), testCaller, ListArtifactsRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("recovered artifacts = %+v, %v", page, err)
	}
}
