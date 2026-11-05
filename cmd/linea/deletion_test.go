package main

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
)

// runs delete and suite delete apply the HTTP rules: a lone finished run
// deletes, a suite member is refused naming its suite, and the suite deletes
// with its runs.
func TestRunsAndSuiteDeleteApplyTheHTTPRules(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	config, scenario := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	if code, _, stderr := runCLI(t, "test", "--config", config, "--scenario", scenario, "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "run", "--config", config, "--scenario", scenario, "--server", server); code != exitOK {
		t.Fatalf("run: exit %d, %s", code, stderr)
	}
	var suites []api.Suite
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites", nil, http.StatusOK), &suites)
	var page api.RunPage
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs", nil, http.StatusOK), &page)
	runs := page.Runs
	if len(suites) != 1 || len(suites[0].Members) != 1 || len(runs) != 2 {
		t.Fatalf("suites = %+v, runs = %d", suites, len(runs))
	}
	suite, member := suites[0].ID, suites[0].Members[0].RunID
	lone := runs[slices.IndexFunc(runs, func(r api.Run) bool { return r.ID != member })].ID

	if code, out, stderr := runCLI(t, "runs", "delete", "--server", server, "--run", member); code != exitError || out != "" || !strings.Contains(stderr, "409") || !strings.Contains(stderr, suite) {
		t.Fatalf("delete member: exit %d, %s%s", code, out, stderr)
	}
	var deleted struct {
		Deleted bool `json:"deleted"`
	}
	if decodeParityJSON(t, parityCLI(t, ctx, []string{"runs", "delete", "--server", server, "--run", lone, "--format", "json"}), &deleted); !deleted.Deleted {
		t.Fatal("delete run json: deleted false")
	}
	var refused api.OperationError
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs/"+lone, nil, http.StatusNotFound), &refused)
	if refused.Error != "run was deleted" {
		t.Fatalf("deleted run = %+v", refused)
	}
	if code, out, _ := runCLI(t, "suite", "delete", "--server", server, "--suite", suite); code != exitOK || out != "suite and its member runs deleted\n" {
		t.Fatalf("delete suite: exit %d, %s", code, out)
	}
	parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs/"+member, nil, http.StatusNotFound)
	parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/suites/"+suite, nil, http.StatusNotFound)

	for _, args := range [][]string{
		{"runs", "delete", "--server", server, "--run", lone},
		{"suite", "delete", "--server", server, "--suite", suite},
		{"suite", "delete", "--suite", suite},
		{"runs", "delete", "--run", lone},
	} {
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
