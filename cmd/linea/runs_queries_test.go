package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// withoutURIs drops the evidence resource URIs only MCP sets (uri, *_uri).
func withoutURIs(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, item := range v {
			if k == "uri" || strings.HasSuffix(k, "_uri") {
				delete(v, k)
			} else {
				v[k] = withoutURIs(item)
			}
		}
	case []any:
		for i := range v {
			v[i] = withoutURIs(v[i])
		}
	}
	return v
}

func generic(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	decodeParityJSON(t, raw, &v)
	return withoutURIs(v)
}

// A run's summary, one evidence page and its cursor, and recorded
// evaluations read the same through HTTP, MCP and the CLI.
func TestRunSummaryEvidenceAndEvaluationsParity(t *testing.T) {
	ctx, server, _ := startMCPServe(t)
	session, err := sdk.NewClient(&sdk.Implementation{Name: "queries-parity", Version: "1"}, nil).
		Connect(ctx, mcpTransport(server), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	scenario := filepath.Join(dir, "scenario.yaml")
	if code, _, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", scenario, "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	var page api.RunPage
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs", nil, http.StatusOK), &page)
	runs := page.Runs
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	id := runs[0].ID
	run := server + "/api/v1/runs/" + id
	cli := func(args ...string) []byte {
		return parityCLI(t, ctx, append(append([]string{"runs"}, args...), "--server", server, "--run", id, "--format", "json"))
	}

	// Summary: the CLI prints HTTP's bytes; MCP's linea_get_run carries it.
	summary := parityHTTP(t, ctx, http.MethodGet, run+"/summary", nil, http.StatusOK)
	assertParityBytes(t, "summary", cli("summary"), summary)
	var mcpRun struct {
		Summary json.RawMessage `json:"summary"`
	}
	parityTool(t, ctx, session, "linea_get_run", map[string]any{"run_id": id}, &mcpRun)
	if got, want := generic(t, mcpRun.Summary), generic(t, summary); !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP summary = %v, HTTP = %v", got, want)
	}
	var s platform.RunSummary
	decodeParityJSON(t, summary, &s)
	if len(s.Stages) == 0 {
		t.Fatal("no stages")
	}
	stage := s.Stages[0]

	// One evidence page and the page its cursor continues.
	query := url.Values{"family": {"records"}, "pipeline": {stage.Pipeline}, "component": {stage.Component}, "side": {"before"}, "page_items": {"1"}}
	args := []string{"evidence", "--family", "records", "--pipeline", stage.Pipeline, "--component", stage.Component, "--side", "before", "--page-items", "1"}
	tool := map[string]any{"run_id": id, "family": "records", "pipeline": stage.Pipeline, "component": stage.Component, "side": "before", "page_items": 1}
	for page := 0; page < 2; page++ {
		raw := parityHTTP(t, ctx, http.MethodGet, run+"/evidence?"+query.Encode(), nil, http.StatusOK)
		assertParityBytes(t, "evidence page "+strconv.Itoa(page), cli(args...), raw)
		var mcpPage any
		parityTool(t, ctx, session, "linea_get_execution_trace", tool, &mcpPage)
		if got, want := withoutURIs(mcpPage), generic(t, raw); !reflect.DeepEqual(got, want) {
			t.Fatalf("page %d: MCP = %v, HTTP = %v", page, got, want)
		}
		var result platform.EvidenceResult
		decodeParityJSON(t, raw, &result)
		if len(result.Items) != 1 || result.NextCursor == "" {
			t.Fatalf("page %d = %s", page, raw)
		}
		query.Set("cursor", result.NextCursor)
		args = append(args, "--cursor", result.NextCursor)
		tool["cursor"] = result.NextCursor
	}
	var refused api.OperationError
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, run+"/evidence?family=bogus", nil, http.StatusBadRequest), &refused)
	if refused.Code != string(platform.CodeInvalidRequest) {
		t.Fatalf("bad family = %+v", refused)
	}
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, run+"/evidence?family=records&page_items=x", nil, http.StatusBadRequest), &refused)
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs/missing/summary", nil, http.StatusNotFound), &refused)
	if refused.Code != string(platform.CodeNotFound) {
		t.Fatalf("missing run = %+v", refused)
	}

	// An evaluation MCP records lists and reads the same everywhere.
	scenarioYAML, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	var evaluated struct {
		EvaluationID string `json:"evaluation_id"`
	}
	parityTool(t, ctx, session, "linea_evaluate_assertions", map[string]any{"run_id": id, "scenario_yaml": string(scenarioYAML)}, &evaluated)
	list := parityHTTP(t, ctx, http.MethodGet, run+"/evaluations", nil, http.StatusOK)
	assertParityBytes(t, "evaluations", cli("evaluations"), list)
	var mcpList struct {
		Evaluations any `json:"evaluations"`
	}
	parityTool(t, ctx, session, "linea_list_evaluations", map[string]any{"run_id": id}, &mcpList)
	if got, want := mcpList.Evaluations, generic(t, list); !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP evaluations = %v, HTTP = %v", got, want)
	}
	var infos []api.EvaluationInfo
	decodeParityJSON(t, list, &infos)
	if len(infos) != 1 || infos[0].ID != evaluated.EvaluationID || infos[0].Client != "mcp" || infos[0].Outcome != "pass" {
		t.Fatalf("evaluations = %s", list)
	}
	one := parityHTTP(t, ctx, http.MethodGet, run+"/evaluations/"+evaluated.EvaluationID, nil, http.StatusOK)
	assertParityBytes(t, "evaluation", cli("evaluations", "--evaluation", evaluated.EvaluationID), one)
	var mcpOne any
	parityTool(t, ctx, session, "linea_get_evaluation", map[string]any{"run_id": id, "evaluation_id": evaluated.EvaluationID}, &mcpOne)
	if want := generic(t, one); !reflect.DeepEqual(mcpOne, want) {
		t.Fatalf("MCP evaluation = %v, HTTP = %v", mcpOne, want)
	}
	if strings.Contains(string(one), "scenario_yaml") {
		t.Fatalf("evaluation returns its source: %s", one)
	}
	parityHTTP(t, ctx, http.MethodGet, run+"/evaluations/eval-missing", nil, http.StatusNotFound)

	// Text forms.
	for _, args := range [][]string{{"summary"}, {"evidence", "--family", "records", "--pipeline", stage.Pipeline, "--component", stage.Component, "--side", "before"}, {"evaluations"}, {"evaluations", "--evaluation", evaluated.EvaluationID}} {
		code, out, stderr := runCLI(t, append(append([]string{"runs"}, args...), "--server", server, "--run", id)...)
		if code != exitOK || out == "" {
			t.Fatalf("%v: exit %d, %s%s", args, code, out, stderr)
		}
	}
	if code, _, stderr := runCLI(t, "runs", "evidence", "--server", server, "--run", id, "--family", "bogus"); code != exitError || !strings.Contains(stderr, "400") {
		t.Fatalf("bad family: exit %d, %s", code, stderr)
	}
}
