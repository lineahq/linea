package platform

import (
	"bytes"
	"context"
	"maps"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func completedExecutor(_ context.Context, req engine.Request) (*engine.Report, error) {
	return report(req, runresult.StatusCompleted), nil
}

// TestStorageUsageReportsLimitsAndBytes checks the startup facts: the
// limits a workspace was opened with and the bytes it stores; an ephemeral
// service has none.
func TestStorageUsageReportsLimitsAndBytes(t *testing.T) {
	ctx := context.Background()
	if _, err := New(Options{}).GetStorageUsage(ctx, testCaller); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("ephemeral usage = %v, want unavailable", err)
	}
	data := memory.NewData()
	s := openPlatformLimits(t, data, artifact.Limits{StorageBytes: 64 << 20, RunBytes: 4 << 20}, Options{})
	u, err := s.GetStorageUsage(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	if u.Limit != 64<<20 || u.RunLimit != 4<<20 || u.Used <= 0 {
		t.Fatalf("usage = %+v", u)
	}
}

// TestLimitBelowUsageRefusesNewWorkWithoutWrites: reopening a workspace with
// a storage limit below what it stores opens it, reads its runs and sources,
// reports usage over the limit and refuses new runs and sources without
// changing a byte.
func TestLimitBelowUsageRefusesNewWorkWithoutWrites(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	s := openPlatform(t, data, Options{Prepare: unprepared, Execute: completedExecutor})
	info, err := s.GetWorkspaceInfo(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Setup, Content: bytes.Repeat([]byte("x"), 3<<20)})
	if err != nil {
		t.Fatal(err)
	}
	done := waitRun(t, submit(t, s, request("before")))
	before := repositorySnapshot(t, s)
	s.Close()

	limits := artifact.Limits{StorageBytes: 2 << 20, RunBytes: 2 << 20}
	s = openPlatformLimits(t, data, limits, Options{Prepare: unprepared, Execute: completedExecutor})
	u, err := s.GetStorageUsage(ctx, testCaller)
	if err != nil || u.Used <= u.Limit {
		t.Fatalf("usage = %+v, %v; want over the limit", u, err)
	}
	if h, err := s.Get(done.ID); err != nil || h.View(false).State != StateDone {
		t.Fatalf("existing run = %v", err)
	}
	if _, err := s.GetArtifact(ctx, testCaller, saved.Artifact.ID); err != nil {
		t.Fatalf("existing source = %v", err)
	}
	run := RunRequest{Mode: ModeQuick, Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "after"}}}
	if _, err := s.SubmitRun(ctx, testCaller, run); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("run over the limit = %v, want resource_exhausted", err)
	}
	if _, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Setup, Content: []byte("y")}); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("source over the limit = %v, want resource_exhausted", err)
	}
	after := repositorySnapshot(t, s)
	s.Close()
	if !maps.Equal(before, after) {
		t.Fatal("refused work changed the workspace")
	}
}
