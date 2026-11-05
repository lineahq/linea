package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func del(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func wantNoContent(t *testing.T, resp *http.Response) {
	t.Helper()
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE %s: status %d, want 204", resp.Request.URL.Path, resp.StatusCode)
	}
}

// TestDeleteRoutesMapPlatformDeletion checks over HTTP that unfinished
// runs and suite members are 409 with the conflict code, finished runs and
// suites are 204, and later requests naming them are 404 "was deleted".
func TestDeleteRoutesMapPlatformDeletion(t *testing.T) {
	started, release := make(chan struct{}, 4), make(chan struct{})
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-release
		return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
	}})
	t.Cleanup(service.Close)
	s, srv := newServerOptions(t, Options{Platform: service})
	run := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	runURL := srv.URL + "/api/v1/runs/" + run.ID
	if e := decodeBody[OperationError](t, del(t, runURL), http.StatusConflict); e.Code != string(platform.CodeConflict) {
		t.Fatalf("deleting a running run = %+v", e)
	}
	suite := decodeBody[Suite](t, post(t, srv.URL+"/api/v1/suites", SuiteRequest{ConfigYAML: readText(t, multisignalConfig), Scenarios: []SuiteScenario{{ScenarioYAML: scenarioText(t, multisignalScenario)}}}), http.StatusAccepted)
	suiteURL := srv.URL + "/api/v1/suites/" + suite.ID
	decodeBody[OperationError](t, del(t, suiteURL), http.StatusConflict)
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitRun(ctx, t, s.runs, run.ID)
	member := suite.Members[0].RunID
	waitRun(ctx, t, s.runs, member)

	if e := decodeBody[OperationError](t, del(t, srv.URL+"/api/v1/runs/"+member), http.StatusConflict); !strings.Contains(e.Error, suite.ID) {
		t.Fatalf("deleting a suite member = %+v, want the suite named", e)
	}
	wantNoContent(t, del(t, runURL))
	if e := decodeBody[Error](t, get(t, runURL), http.StatusNotFound); e.Error != "run was deleted" {
		t.Fatalf("deleted run = %+v", e)
	}
	decodeBody[Error](t, get(t, runURL+"/report"), http.StatusNotFound)
	if e := decodeBody[OperationError](t, del(t, runURL), http.StatusNotFound); e.Code != string(platform.CodeNotFound) || e.Error != "run was deleted" {
		t.Fatalf("deleting twice = %+v", e)
	}
	wantNoContent(t, del(t, suiteURL))
	if e := decodeBody[Error](t, get(t, suiteURL), http.StatusNotFound); e.Error != "suite was deleted" {
		t.Fatalf("deleted suite = %+v", e)
	}
	if e := decodeBody[Error](t, get(t, srv.URL+"/api/v1/runs/"+member), http.StatusNotFound); e.Error != "run was deleted" {
		t.Fatalf("deleted member = %+v", e)
	}
	if runs := listRuns(t, service); len(runs) != 0 {
		t.Fatalf("runs after deletion = %+v", runs)
	}
	decodeBody[OperationError](t, del(t, srv.URL+"/api/v1/suites/missing"), http.StatusNotFound)
}
