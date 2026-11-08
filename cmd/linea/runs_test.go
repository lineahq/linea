package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunsListsServerRunsWithSummariesAndFilters(t *testing.T) {
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	if code, _, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"), "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}

	code, out, stderr := runCLI(t, "runs", "--server", server, "--format", "json")
	var page struct {
		Runs []struct {
			ID      string `json:"id"`
			Summary *struct {
				Scenario string `json:"scenario"`
				Status   string `json:"status"`
				Runtime  struct {
					Name string `json:"name"`
				} `json:"runtime"`
				Assertions *struct {
					Outcome string `json:"outcome"`
				} `json:"assertions"`
			} `json:"summary"`
		} `json:"runs"`
		NextCursor string `json:"next_cursor"`
	}
	err := json.Unmarshal([]byte(out), &page)
	runs := page.Runs
	if code != exitOK || err != nil || len(runs) != 1 || page.NextCursor != "" {
		t.Fatalf("runs: exit %d, %s%s: %v", code, out, stderr, err)
	}
	s := runs[0].Summary
	if s == nil || s.Scenario != "logs-cleanup" || s.Status != "completed" || s.Runtime.Name != "lab" || s.Assertions == nil || s.Assertions.Outcome != "pass" {
		t.Fatalf("summary = %+v", s)
	}

	code, out, _ = runCLI(t, "runs", "--server", server, "--scenario", "logs-cleanup", "--result", "completed")
	if code != exitOK || !strings.Contains(out, runs[0].ID) || !strings.Contains(out, "logs-cleanup") || !strings.Contains(out, "PASS (") {
		t.Fatalf("filtered text: exit %d, %s", code, out)
	}
	code, out, _ = runCLI(t, "runs", "--server", server, "--runtime", "acme")
	if code != exitOK || strings.Contains(out, runs[0].ID) {
		t.Fatalf("other runtime: exit %d, %s", code, out)
	}
	if code, _, stderr = runCLI(t, "runs", "--server", server, "--result", "passed"); code != exitError || !strings.Contains(stderr, "400") {
		t.Fatalf("unknown result: exit %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "runs"); code != exitError {
		t.Fatalf("runs without --server: exit %d", code)
	}

	// A second run makes two pages of one.
	if code, _, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"), "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	code, out, stderr = runCLI(t, "runs", "--server", server, "--limit", "1")
	table, cursor, _ := strings.Cut(out, "\n\nnext page: --cursor ")
	cursor = strings.TrimSpace(cursor)
	if code != exitOK || strings.Contains(table, runs[0].ID) || cursor == "" || strings.Contains(cursor, " ") || stderr != "" {
		t.Fatalf("first page: exit %d, %s%s", code, out, stderr)
	}
	if code, out, stderr = runCLI(t, "runs", "--server", server, "--limit", "1", "--cursor", cursor); code != exitOK || !strings.Contains(out, runs[0].ID) || stderr != "" {
		t.Fatalf("second page: exit %d, %s%s", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "runs", "--server", server, "--limit", "1", "--all", "--format", "json")
	var all struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(out), &all); code != exitOK || err != nil || len(all.Runs) != 2 || all.Runs[1].ID != runs[0].ID || all.NextCursor != "" {
		t.Fatalf("--all: exit %d, %s%s", code, out, stderr)
	}
	if code, _, _ = runCLI(t, "runs", "--server", server, "--all", "--cursor", cursor); code != exitError {
		t.Fatalf("--all with --cursor: exit %d", code)
	}
}
