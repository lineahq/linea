package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
)

// statement test runs a proposal on a run's recorded inputs through serve,
// prints the server's JSON or the candidate and its queued run, and exits 1
// for an invalid candidate.
func TestStatementTestOnRunThroughServe(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	config, scenario := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	if code, _, stderr := runCLI(t, "run", "--config", config, "--scenario", scenario, "--server", server); code != exitOK {
		t.Fatalf("run: exit %d, %s", code, stderr)
	}
	var page api.RunPage
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs", nil, http.StatusOK), &page)
	runs := page.Runs
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	id := runs[0].ID

	var test api.StatementTest
	decodeParityJSON(t, parityCLI(t, ctx, []string{"statement", "test", "--server", server, "--run", id, "--proposed", config, "--format", "json"}), &test)
	if !test.Candidate.Valid || test.Run == nil || test.Run.ID == id {
		t.Fatalf("test = %+v", test)
	}
	code, out, stderr := runCLI(t, "runs", "show", "--server", server, "--run", test.Run.ID, "--report", "--wait")
	if code != exitOK || !strings.Contains(out, `"assertions"`) {
		t.Fatalf("tested run report: exit %d, %s", code, stderr)
	}
	if code, out, stderr := runCLI(t, "statement", "test", "--server", server, "--run", id, "--proposed", config); code != exitOK || !strings.HasPrefix(out, "VALID  candidate") || !strings.Contains(out, "runs show --server "+server+" --run ") {
		t.Fatalf("text: exit %d, %s%s", code, out, stderr)
	}

	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("service: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := runCLI(t, "statement", "test", "--server", server, "--run", id, "--proposed", broken); code != exitError || !strings.HasPrefix(out, "INVALID  candidate") || strings.Contains(out, "test run") {
		t.Fatalf("invalid: exit %d, %s", code, out)
	}
	for _, args := range [][]string{
		{"statement", "test", "--server", server, "--run", "missing", "--proposed", config},
		{"statement", "test", "--run", id, "--proposed", config},
		{"statement", "test", "--server", server, "--run", id},
		{"statement", "test", "--server", server, "--run", id, "--proposed", config, "--op", "delete"},
	} {
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
