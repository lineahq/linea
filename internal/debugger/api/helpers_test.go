package api

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func listRuns(t *testing.T, s *platform.Service) []platform.Run {
	t.Helper()
	page, err := s.ListRuns(context.Background(), caller, platform.RunQuery{Limit: platform.MaxRunPage})
	if err != nil {
		t.Fatal(err)
	}
	return page.Runs
}

// waitRun waits for a run to end, bounded by ctx, and returns its final view.
func waitRun(ctx context.Context, t *testing.T, s *platform.Service, id string) platform.Run {
	t.Helper()
	if _, err := s.GetRunReport(ctx, caller, id, true); platform.ErrorCode(err) == platform.CodeCancelled {
		t.Fatal(err)
	}
	run, err := s.GetRun(ctx, caller, id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// caller is the local owner via HTTP, as ServeHTTP authenticates requests in
// local mode, for tests that call the platform directly.
var caller = platform.Local("http")
