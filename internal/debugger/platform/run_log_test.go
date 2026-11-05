package platform

import (
	"context"
	"errors"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// loggingExecutor logs one line per run and ends it as its fixture says.
func loggingExecutor(_ context.Context, req engine.Request) (*engine.Report, error) {
	_, _ = req.Log.Write([]byte("collector line for " + req.FixtureJSON + "\n"))
	switch req.FixtureJSON {
	case "fail":
		return nil, errors.New("collector failed to start")
	case "partial":
		return report(req, runresult.StatusPartial), nil
	}
	return report(req, runresult.StatusCompleted), nil
}

// A failed or partial run keeps its log tail with its evidence, across a
// restart; a completed run keeps none, and deletion removes it.
func TestFailedAndPartialRunsKeepTheirLog(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	opts := Options{Prepare: unprepared, Execute: loggingExecutor}
	s := openPlatform(t, data, opts)
	ids := map[string]string{}
	for _, label := range []string{"fail", "partial", "ok"} {
		ids[label] = waitRun(t, submit(t, s, request(label))).ID
	}
	check := func(s *Service) {
		t.Helper()
		for label, want := range map[string]RunLog{
			"fail":    {Kept: true, Log: "collector line for fail\n"},
			"partial": {Kept: true, Log: "collector line for partial\n"},
			"ok":      {},
		} {
			got, err := s.GetRunLog(ctx, testCaller, ids[label])
			want.RunID = ids[label]
			if err != nil || *got != want {
				t.Fatalf("%s log = %+v, %v; want %+v", label, got, err, want)
			}
		}
	}
	check(s)
	s.Close()

	s = openPlatform(t, data, opts)
	check(s)
	if err := s.DeleteRun(ctx, testCaller, ids["fail"]); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetRunLog(ctx, testCaller, ids["fail"])
	wantDeleted(t, err, "run was deleted")
	_, err = s.GetRunLog(ctx, testCaller, "missing")
	wantCode(t, err, CodeNotFound)
}
