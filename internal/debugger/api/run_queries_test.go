package api

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// The summary, evidence and evaluation routes map the platform reads and
// carry its error codes.
func TestRunQueryRoutesMapPlatformReads(t *testing.T) {
	s, srv := newServerOptions(t, Options{})
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitRun(ctx, t, s.runs, created.ID)
	run := srv.URL + "/api/v1/runs/" + created.ID

	summary := decodeBody[platform.RunSummary](t, get(t, run+"/summary"), http.StatusOK)
	want, err := s.runs.GetRunSummary(ctx, caller, created.ID)
	if err != nil || summary.Complete != want.Complete || len(summary.Stages) != len(want.Stages) || len(summary.Stages) == 0 {
		t.Fatalf("summary = %+v, want %+v: %v", summary, want, err)
	}

	page := decodeBody[platform.EvidenceResult](t, get(t, run+"/evidence?family=events&page_items=1"), http.StatusOK)
	direct, err := s.runs.QueryEvidence(ctx, caller, created.ID, platform.EvidenceQuery{Family: platform.EvidenceEvents}, platform.EvidencePage{Items: 1})
	if err != nil || page.Total != direct.Total || page.NextCursor != direct.NextCursor || len(page.Items) != 1 || page.Items[0].Ref != direct.Items[0].Ref {
		t.Fatalf("evidence = %+v, want %+v: %v", page, direct, err)
	}
	for _, tc := range []struct {
		path   string
		status int
		code   platform.Code
	}{
		{"/evidence?family=bogus", http.StatusBadRequest, platform.CodeInvalidRequest},
		{"/evidence?family=events&to_seq=last", http.StatusBadRequest, platform.CodeInvalidRequest},
		{"/evidence?family=events&cursor=forged", http.StatusBadRequest, platform.CodeInvalidRequest},
		{"/evaluations/eval-missing", http.StatusNotFound, platform.CodeNotFound},
	} {
		if e := decodeBody[OperationError](t, get(t, run+tc.path), tc.status); e.Code != string(tc.code) {
			t.Fatalf("%s: code = %q", tc.path, e.Code)
		}
	}
	for _, path := range []string{"/summary", "/evidence?family=events", "/evaluations"} {
		if e := decodeBody[OperationError](t, get(t, srv.URL+"/api/v1/runs/missing"+path), http.StatusNotFound); e.Code != string(platform.CodeNotFound) {
			t.Fatalf("missing run %s: code = %q", path, e.Code)
		}
	}

	if list := decodeBody[[]EvaluationInfo](t, get(t, run+"/evaluations"), http.StatusOK); len(list) != 0 {
		t.Fatalf("evaluations = %+v", list)
	}
	scenarioYAML, err := os.ReadFile(multisignalScenario)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.runs.EvaluateAssertions(ctx, platform.Local("mcp"), created.ID, platform.AssertionsRequest{ScenarioYAML: string(scenarioYAML), Record: true})
	if err != nil {
		t.Fatal(err)
	}
	list := decodeBody[[]EvaluationInfo](t, get(t, run+"/evaluations"), http.StatusOK)
	if len(list) != 1 || list[0].ID != e.ID || list[0].RunID != created.ID || list[0].Client != "mcp" || list[0].Outcome != e.Outcome {
		t.Fatalf("evaluations = %+v", list)
	}
	got := decodeBody[Evaluation](t, get(t, run+"/evaluations/"+e.ID), http.StatusOK)
	if got.EvaluationInfo != list[0] || got.Result == nil || got.Result.Outcome != e.Outcome {
		t.Fatalf("evaluation = %+v", got)
	}

	// Another run's evaluation is unknown under this run.
	other := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	waitRun(ctx, t, s.runs, other.ID)
	decodeBody[OperationError](t, get(t, srv.URL+"/api/v1/runs/"+other.ID+"/evaluations/"+e.ID), http.StatusNotFound)
}
