package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// The run, suite, OTTL and candidate reads print the server's JSON as HTTP
// sends it, and local OTTL and candidate output matches connected.
func TestRunAndSuiteReadsMatchHTTP(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	config, scenario, input := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml"), filepath.Join(dir, "input.json")
	if code, _, stderr := runCLI(t, "test", "--config", config, "--scenario", scenario, "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	var suites []api.Suite
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites", nil, http.StatusOK), &suites)
	if len(suites) != 1 || len(suites[0].Members) == 0 {
		t.Fatalf("suites = %+v", suites)
	}
	suite, id := suites[0].ID, suites[0].Members[0].RunID
	run := server + "/api/v1/runs/" + id
	scenarioYAML, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		http []byte
	}{
		{[]string{"runs", "show", "--run", id}, parityHTTP(t, ctx, http.MethodGet, run, nil, http.StatusOK)},
		{[]string{"runs", "show", "--run", id, "--report"}, parityHTTP(t, ctx, http.MethodGet, run+"/report", nil, http.StatusOK)},
		{[]string{"runs", "inputs", "--run", id}, parityHTTP(t, ctx, http.MethodGet, run+"/inputs", nil, http.StatusOK)},
		{[]string{"runs", "diffs", "--run", id}, parityHTTP(t, ctx, http.MethodGet, run+"/diffs", nil, http.StatusOK)},
		{[]string{"runs", "assert", "--run", id, "--scenario", scenario},
			parityHTTP(t, ctx, http.MethodPost, run+"/assertions", api.AssertionsRequest{ScenarioYAML: string(scenarioYAML)}, http.StatusOK)},
		{[]string{"suite", "list"}, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites", nil, http.StatusOK)},
		{[]string{"suite", "show", "--suite", suite}, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites/"+suite, nil, http.StatusOK)},
		{[]string{"suite", "report", "--suite", suite}, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites/"+suite+"/report", nil, http.StatusOK)},
	} {
		got := parityCLI(t, ctx, append(tc.args, "--server", server, "--format", "json"))
		assertParityBytes(t, strings.Join(tc.args[:2], " "), got, tc.http)
		if code, out, stderr := runCLI(t, append(tc.args, "--server", server)...); code != exitOK || out == "" {
			t.Fatalf("%v text: exit %d, %s%s", tc.args, code, out, stderr)
		}
	}
	if code, out, _ := runCLI(t, "runs", "show", "--server", server, "--run", id); !strings.HasPrefix(out, "run "+id+": state done") || code != exitOK {
		t.Fatalf("show text = %q", out)
	}

	// A snapshot is written as the server sends it, never over a file.
	var report struct {
		Result struct {
			Events []struct {
				Snapshot string `json:"snapshot"`
			} `json:"events"`
		} `json:"result"`
	}
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, run+"/report", nil, http.StatusOK), &report)
	var ref string
	for _, e := range report.Result.Events {
		if ref = e.Snapshot; ref != "" {
			break
		}
	}
	out := filepath.Join(t.TempDir(), "snapshot.json")
	if code, _, stderr := runCLI(t, "runs", "snapshot", "--server", server, "--run", id, "--ref", ref, "--out", out); code != exitOK {
		t.Fatalf("snapshot: exit %d, %s", code, stderr)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	assertParityBytes(t, "snapshot", written, parityHTTP(t, ctx, http.MethodGet, run+"/snapshots/"+url.PathEscape(ref), nil, http.StatusOK))
	if code, _, stderr := runCLI(t, "runs", "snapshot", "--server", server, "--run", id, "--ref", ref, "--out", out); code != exitError || !strings.Contains(stderr, "never overwritten") {
		t.Fatalf("existing --out: exit %d, %s", code, stderr)
	}

	// OTTL preview and candidates print the same JSON locally and connected.
	preview := []string{"ottl", "preview", "--config", config, "--processor", "transform/severity", "--input", input, "--format", "json"}
	// Each names the runtime that answered: this process's, or the lab's.
	localPreview, connectedPreview := parityCLI(t, ctx, preview), parityCLI(t, ctx, append(preview, "--server", server))
	var localRuntime, connectedRuntime struct {
		Runtime struct{ Kind, Name string } `json:"runtime"`
	}
	decodeParityJSON(t, localPreview, &localRuntime)
	decodeParityJSON(t, connectedPreview, &connectedRuntime)
	if localRuntime.Runtime.Kind != engine.RuntimeBuiltIn || connectedRuntime.Runtime.Name != "lab" {
		t.Fatalf("preview runtimes = %+v locally, %+v connected", localRuntime.Runtime, connectedRuntime.Runtime)
	}
	assertParityBytes(t, "ottl preview", previewWithoutRuntime(t, localPreview), previewWithoutRuntime(t, connectedPreview))
	candidate := []string{"statement", "candidate", "--config", config, "--processor", "transform/severity", "--signal", "logs", "--op", "replace",
		"--expected", `set(log.severity_text, ConvertCase(log.severity_text, "upper"))`, "--text", `set(log.severity_text, ConvertCase(log.severity_text, "lower"))`, "--format", "json"}
	local := parityCLI(t, ctx, candidate)
	assertParityBytes(t, "candidate", local, parityCLI(t, ctx, append(candidate, "--server", server)))
	var cand api.Candidate
	if err := json.Unmarshal(local, &cand); err != nil || !cand.Valid || !strings.Contains(cand.ProposedYAML, `"lower"`) {
		t.Fatalf("candidate = %s: %v", local, err)
	}
	if code, _, stderr := runCLI(t, append(candidate[:len(candidate)-2], "--expected", "stale")...); code != exitError || !strings.Contains(stderr, "has changed since it was read") {
		t.Fatalf("stale edit: exit %d, %s", code, stderr)
	}

	for _, args := range [][]string{
		{"runs", "show", "--server", server, "--run", "missing"},
		{"suite", "show", "--server", server, "--suite", "suite-missing"},
	} {
		if code, _, stderr := runCLI(t, args...); code != exitError || !strings.Contains(stderr, "404") {
			t.Fatalf("%v: exit %d, %s", args, code, stderr)
		}
	}
	for _, args := range [][]string{{"runs", "show", "--run", id}, {"suite", "list"}, {"statement", "candidate", "--config", config}, {"ottl", "bogus"}} {
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

// previewWithoutRuntime is a preview's JSON without the runtime that answered, the
// one field a local and a connected preview differ in.
func previewWithoutRuntime(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v map[string]json.RawMessage
	decodeParityJSON(t, raw, &v)
	delete(v, "runtime")
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
