package api

import (
	"encoding/json"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/platform"
)

const candidateBase = `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform:
    trace_statements:
      - context: span
        statements:
          # keep this comment
          - set(attributes["team"], "a")
exporters: {otlp: {endpoint: example.invalid:4317}}
service: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [otlp]}}}
`

// The candidates route splices a statement edit into the base text, reports
// a stale edit with the statement's current text and saves only on request.
func TestCandidatesRouteProposesStatementEdits(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	edit := &platform.StatementEdit{Processor: "transform", Signal: "traces", Group: 0, Index: 0, Op: "replace",
		Text: `set(attributes["team"], "b")`, Expected: `set(attributes["team"], "a")`}

	c := decodeBody[Candidate](t, post(t, srv.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, Edit: edit}), http.StatusOK)
	want := strings.Replace(candidateBase, `"team"], "a")`, `"team"], "b")`, 1)
	if c.ProposedYAML != want || !c.Valid || !slices.Equal(c.AffectedPipelines, []string{"traces"}) || c.Saved != nil ||
		!strings.Contains(c.Patch, `+          - set(attributes["team"], "b")`) || c.Diagnostics == nil {
		t.Fatalf("candidate = %+v", c)
	}
	if page := decodeBody[artifact.Page](t, get(t, srv.URL+"/api/v1/artifacts?kind=configuration"), http.StatusOK); len(page.Items) != 0 {
		t.Fatalf("an unsaved candidate created artifacts: %+v", page.Items)
	}

	stale := *edit
	stale.Expected = `set(attributes["team"], "old")`
	e := decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, Edit: &stale}), http.StatusConflict)
	if e.Code != "conflict" || e.CurrentStatement != `set(attributes["team"], "a")` {
		t.Fatalf("stale edit = %+v", e)
	}
	e = decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, Edit: edit, ProposedYAML: want}), http.StatusBadRequest)
	if e.Code != "invalid_request" {
		t.Fatalf("edit and YAML = %+v", e)
	}

	saved := decodeBody[Candidate](t, post(t, srv.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, ProposedYAML: want, Save: true, Label: "team-b.yaml"}), http.StatusCreated)
	if saved.Saved == nil || saved.Saved.Artifact.Kind != artifact.Configuration || saved.Saved.Artifact.Label != "team-b.yaml" {
		t.Fatalf("saved candidate = %+v", saved)
	}

	_, ephemeral := newServerOptions(t, Options{})
	e = decodeBody[OperationError](t, post(t, ephemeral.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, Edit: edit, Save: true}), http.StatusServiceUnavailable)
	if e.Code != "unavailable" {
		t.Fatalf("save without workspace = %+v", e)
	}
	decodeBody[Candidate](t, post(t, ephemeral.URL+"/api/v1/candidates", CandidateRequest{ConfigYAML: candidateBase, Edit: edit}), http.StatusOK)
}

// The scenario route derives a self-contained scenario from a run of either
// mode without copying environment values, and saves it only on request.
func TestScenarioFromRunRouteDerivesScenarios(t *testing.T) {
	const name, secret = "OTEL_API_SCENARIO_FROM_RUN", "private-host-value"
	t.Setenv(name, secret)
	traces, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	collectorYAML := strings.Replace(candidateBase, `"a")`, `"${env:`+name+`}")`, 1)
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Environment: config.EnvironmentPolicy{Host: []string{name}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	inline := strings.ReplaceAll(strings.TrimSpace(string(traces)), "\n", " ")
	for _, req := range []Request{
		{ConfigYAML: collectorYAML, FixtureJSON: string(traces)},
		{ConfigYAML: collectorYAML, ScenarioYAML: "version: 1\nname: original\ninputs:\n  - data: '" + inline + "'\n"},
	} {
		run := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", req), http.StatusAccepted)
		resp := post(t, srv.URL+"/api/v1/runs/"+run.ID+"/scenario", ScenarioFromRunRequest{Name: "kept"})
		raw := string(responseBytes(t, resp, http.StatusOK))
		if strings.Contains(raw, secret) {
			t.Fatalf("environment value copied: %s", raw)
		}
		sc := decodeBody[ScenarioFromRun](t, post(t, srv.URL+"/api/v1/runs/"+run.ID+"/scenario", ScenarioFromRunRequest{Name: "kept"}), http.StatusOK)
		if sc.RunID != run.ID || !strings.Contains(sc.YAML, "name: kept") || !strings.Contains(sc.YAML, "inputs:") ||
			!slices.Equal(sc.RequiredEnvironment, []string{name}) || sc.Saved != nil {
			t.Fatalf("scenario = %+v", sc)
		}
		saved := decodeBody[ScenarioFromRun](t, post(t, srv.URL+"/api/v1/runs/"+run.ID+"/scenario", ScenarioFromRunRequest{Save: true, Label: "from-run.yaml"}), http.StatusCreated)
		if saved.Saved == nil || saved.Saved.Artifact.Kind != artifact.Scenario || saved.Saved.Artifact.Label != "from-run.yaml" {
			t.Fatalf("saved scenario = %+v", saved)
		}
		e := decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/runs/"+run.ID+"/scenario", ScenarioFromRunRequest{AssertionsYAML: "assertions: [{nope: 1}]"}), http.StatusBadRequest)
		if e.Code != "invalid_request" {
			t.Fatalf("invalid assertions = %+v", e)
		}
	}
	e := decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/runs/missing/scenario", ScenarioFromRunRequest{}), http.StatusNotFound)
	if e.Code != "not_found" {
		t.Fatalf("unknown run = %+v", e)
	}
}

// The export route returns the derived scenario after its header; nothing is
// saved and environment values stay out.
func TestExportRouteReturnsAHeadedScenarioFile(t *testing.T) {
	const name, secret = "OTEL_API_EXPORT", "private-export-value"
	t.Setenv(name, secret)
	traces, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	service := platform.New(platform.Options{Environment: config.EnvironmentPolicy{Host: []string{name}}})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	collectorYAML := strings.Replace(candidateBase, `"a")`, `"${env:`+name+`}")`, 1)
	run := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigYAML: collectorYAML, FixtureJSON: string(traces)}), http.StatusAccepted)
	resp := post(t, srv.URL+"/api/v1/runs/"+run.ID+"/export", ExportRequest{Name: "exported"})
	raw := responseBytes(t, resp, http.StatusOK)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("environment value exported: %s", raw)
	}
	var out ScenarioExport
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.RunID != run.ID || !strings.HasPrefix(out.File, "# Scenario exported from linea run "+run.ID+".\n") ||
		!strings.Contains(out.File, "\nname: exported") || !slices.Equal(out.RequiredEnvironment, []string{name}) {
		t.Fatalf("export = %+v", out)
	}
	decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/runs/"+run.ID+"/export", map[string]any{"save": true}), http.StatusBadRequest)
	e := decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/runs/missing/export", ExportRequest{}), http.StatusNotFound)
	if e.Code != "not_found" {
		t.Fatalf("unknown run = %+v", e)
	}
}
