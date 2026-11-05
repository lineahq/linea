package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// The suite routes submit scenarios as ordinary runs, derive their status and
// return the report test prints once every member has finished.
func TestSuiteRoutesSubmitAndReport(t *testing.T) {
	_, srv := newServer(t)
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	collectorYAML, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	req := SuiteRequest{ConfigYAML: string(collectorYAML), ConfigLabel: "config.yaml", Scenarios: []SuiteScenario{
		{ScenarioYAML: scenarioText(t, filepath.Join(dir, "scenario.yaml")), ScenarioLabel: "scenario.yaml"},
		{ScenarioYAML: "version: 1\nname: broken\ninputs: [{file: missing.json}]\n", ScenarioLabel: "broken.yaml"},
	}}
	resp := post(t, srv.URL+"/api/v1/suites", req)
	if resp.StatusCode == http.StatusAccepted && resp.Header.Get("Location") == "" {
		t.Fatal("no Location header")
	}
	suite := decodeBody[Suite](t, resp, http.StatusAccepted)
	if suite.Config != "config.yaml" || len(suite.Members) != 2 || suite.Members[0].RunID == "" || suite.Members[0].Label != "scenario.yaml" {
		t.Fatalf("suite = %+v", suite)
	}

	rep := decodeBody[struct {
		Config    string            `json:"config"`
		Outcome   assertion.Outcome `json:"outcome"`
		Scenarios []struct {
			Path    string            `json:"path"`
			RunID   string            `json:"run_id"`
			Outcome assertion.Outcome `json:"outcome"`
		} `json:"scenarios"`
	}](t, get(t, srv.URL+"/api/v1/suites/"+suite.ID+"/report?wait=true"), http.StatusOK)
	if rep.Config != "config.yaml" || rep.Outcome != assertion.Error || len(rep.Scenarios) != 2 ||
		rep.Scenarios[0].Path != "scenario.yaml" || rep.Scenarios[0].Outcome != assertion.Pass || rep.Scenarios[0].RunID != suite.Members[0].RunID ||
		rep.Scenarios[1].Outcome != assertion.Error {
		t.Fatalf("report = %+v", rep)
	}

	status := decodeBody[SuiteStatus](t, get(t, srv.URL+"/api/v1/suites/"+suite.ID), http.StatusOK)
	if !status.Done || status.Outcome != assertion.Error || status.Members[0].State != StateDone || status.Members[0].Outcome != assertion.Pass {
		t.Fatalf("status = %+v", status)
	}

	decodeBody[Error](t, get(t, srv.URL+"/api/v1/suites/missing"), http.StatusNotFound)
	decodeBody[Error](t, get(t, srv.URL+"/api/v1/suites/missing/report"), http.StatusNotFound)
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/suites", SuiteRequest{ConfigYAML: string(collectorYAML)}), http.StatusBadRequest)
}

// GET /api/v1/suites maps ListSuites: a bare array of admission metadata,
// newest first, holding only the Suite fields and none of the submitted text.
func TestListSuitesRouteReturnsAdmissionMetadataNewestFirst(t *testing.T) {
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
	}})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})

	empty := get(t, srv.URL+"/api/v1/suites")
	defer empty.Body.Close()
	if body, _ := io.ReadAll(empty.Body); empty.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("empty list = %d %q, want 200 []", empty.StatusCode, body)
	}

	const canary = "canary-secret-7f3a"
	configText, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, label := range []string{"first.yaml", "second.yaml"} {
		suite := decodeBody[Suite](t, post(t, srv.URL+"/api/v1/suites", SuiteRequest{
			ConfigYAML: string(configText) + "\n# " + canary + "\n", ConfigLabel: label,
			Scenarios: []SuiteScenario{
				{ScenarioYAML: scenarioText(t, multisignalScenario) + "\n# " + canary + "\n", ScenarioLabel: "ok.yaml"},
				{ScenarioYAML: "version: 1\nname: broken\ninputs: [{file: missing.json}]\n", ScenarioLabel: "broken.yaml"},
			}}), http.StatusAccepted)
		ids = append(ids, suite.ID)
	}

	resp := get(t, srv.URL+"/api/v1/suites")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %v", resp.StatusCode, err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("list exposes submitted text: %s", raw)
	}
	var generic []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil || len(generic) != 2 {
		t.Fatalf("list = %s (%v), want a two-element array", raw, err)
	}
	for _, item := range generic {
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !slices.Equal(keys, []string{"config", "created", "id", "members", "project_id"}) {
			t.Fatalf("suite fields = %v", keys)
		}
	}
	var list []Suite
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if list[0].ID != ids[1] || list[1].ID != ids[0] || list[0].Config != "second.yaml" || list[1].Config != "first.yaml" {
		t.Fatalf("order = %q %q, want newest (%s) first", list[0].ID, list[1].ID, ids[1])
	}
	if m := list[0].Members; len(m) != 2 || m[0].Label != "ok.yaml" || m[0].RunID == "" || m[1].Label != "broken.yaml" || m[1].RunID != "" || m[1].Error == "" {
		t.Fatalf("members = %+v", m)
	}
}

// A suite reruns from its members' recorded inputs and exports JUnit; a
// member that was never admitted refuses the rerun.
func TestSuiteRerunAndJUnitRoutes(t *testing.T) {
	_, srv := newServer(t)
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	collectorYAML, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	member := SuiteScenario{ScenarioYAML: scenarioText(t, filepath.Join(dir, "scenario.yaml")), ScenarioLabel: "scenario.yaml"}
	first := decodeBody[Suite](t, post(t, srv.URL+"/api/v1/suites", SuiteRequest{ConfigYAML: string(collectorYAML), ConfigLabel: "config.yaml", Scenarios: []SuiteScenario{member}}), http.StatusAccepted)
	decodeBody[engine.TestReport](t, get(t, srv.URL+"/api/v1/suites/"+first.ID+"/report?wait=true"), http.StatusOK)

	resp := post(t, srv.URL+"/api/v1/suites/"+first.ID+"/rerun", nil)
	again := decodeBody[Suite](t, resp, http.StatusAccepted)
	if again.ID == first.ID || resp.Header.Get("Location") != "/api/v1/suites/"+again.ID || again.Config != "config.yaml" ||
		len(again.Members) != 1 || again.Members[0].Label != "scenario.yaml" || again.Members[0].RunID == "" {
		t.Fatalf("rerun = %+v", again)
	}
	rep := decodeBody[engine.TestReport](t, get(t, srv.URL+"/api/v1/suites/"+again.ID+"/report?wait=true"), http.StatusOK)
	if rep.Outcome != assertion.Pass {
		t.Fatalf("rerun report = %+v", rep)
	}

	resp = get(t, srv.URL+"/api/v1/suites/"+again.ID+"/junit")
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var junit engine.JUnitSuites
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/xml") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename=`+again.ID+`.junit.xml` || xml.Unmarshal(raw, &junit) != nil ||
		len(junit.Suites) != 1 || junit.Tests != len(rep.Scenarios[0].Assertions.Results) || junit.Failures != 0 {
		t.Fatalf("junit %d %v: %s", resp.StatusCode, resp.Header, raw)
	}

	broken := decodeBody[Suite](t, post(t, srv.URL+"/api/v1/suites", SuiteRequest{ConfigYAML: string(collectorYAML), Scenarios: []SuiteScenario{
		member, {ScenarioYAML: "version: 1\nname: broken\ninputs: [{file: missing.json}]\n", ScenarioLabel: "broken.yaml"},
	}}), http.StatusAccepted)
	decodeBody[engine.TestReport](t, get(t, srv.URL+"/api/v1/suites/"+broken.ID+"/report?wait=true"), http.StatusOK)
	if e := decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/suites/"+broken.ID+"/rerun", nil), http.StatusConflict); !strings.Contains(e.Error, "broken.yaml was never admitted") {
		t.Fatalf("refusal = %+v", e)
	}
	decodeBody[OperationError](t, post(t, srv.URL+"/api/v1/suites/missing/rerun", nil), http.StatusNotFound)
	decodeBody[OperationError](t, get(t, srv.URL+"/api/v1/suites/missing/junit"), http.StatusNotFound)
}
