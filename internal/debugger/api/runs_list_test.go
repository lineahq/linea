package api

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// The list summary is derived from the retained report, so a run admitted
// before a restart lists with one too.
func TestListRunsRouteSummarizesAndFiltersAcrossRestart(t *testing.T) {
	data := memory.NewData()
	service, _, srv := workspaceServer(t, data)
	tracesDir := filepath.Join("..", "..", "..", "testdata", "traces")
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlinedFixture(t, filepath.Join(tracesDir, "config.yaml"), filepath.Join(tracesDir, "traces.json"))), http.StatusAccepted)
	decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+created.ID+"/report?wait=true"), http.StatusOK)
	srv.Close()
	service.Close()
	_, _, srv = workspaceServer(t, data)

	runs := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs?result=completed&runtime=built-in"), http.StatusOK).Runs
	if len(runs) != 1 || runs[0].ID != created.ID || runs[0].Report != nil {
		t.Fatalf("runs = %+v", runs)
	}
	s := runs[0].Summary
	if s == nil || s.Status != "completed" || s.Inputs != 1 || s.Injected != 1 || s.Runtime == nil || s.Runtime.Name != platform.BuiltInRuntime || s.Assertions != nil {
		t.Fatalf("summary = %+v", s)
	}
	if one := decodeBody[Run](t, get(t, srv.URL+"/api/v1/runs/"+created.ID), http.StatusOK); one.Summary != nil {
		t.Fatal("GET /runs/{id} carries the list summary beside its report")
	}
	if runs := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs?scenario=other"), http.StatusOK).Runs; len(runs) != 0 {
		t.Fatalf("scenario filter = %+v", runs)
	}
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs?result=passed"), http.StatusBadRequest)

	second := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlinedFixture(t, filepath.Join(tracesDir, "config.yaml"), filepath.Join(tracesDir, "traces.json"))), http.StatusAccepted)
	decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+second.ID+"/report?wait=true"), http.StatusOK)
	page := decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs?limit=1&result=completed"), http.StatusOK)
	if len(page.Runs) != 1 || page.Runs[0].ID != second.ID || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	page = decodeBody[RunPage](t, get(t, srv.URL+"/api/v1/runs?limit=1&result=completed&cursor="+url.QueryEscape(page.NextCursor)), http.StatusOK)
	if len(page.Runs) != 1 || page.Runs[0].ID != created.ID || page.NextCursor != "" {
		t.Fatalf("last page = %+v", page)
	}
	for _, query := range []string{"limit=0", "limit=x", "limit=501", "cursor=x"} {
		decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs?"+query), http.StatusBadRequest)
	}
}
