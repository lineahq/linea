package main

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
)

func TestSuiteRerunsAServerSuiteAndWritesItsJUnit(t *testing.T) {
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	if code, _, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"), "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	resp := labDo(t, http.MethodGet, server+"/api/v1/suites", "", nil)
	var suites []struct {
		ID string `json:"id"`
	}
	err := json.NewDecoder(resp.Body).Decode(&suites)
	resp.Body.Close()
	if err != nil || len(suites) != 1 {
		t.Fatalf("suites = %+v: %v", suites, err)
	}
	first := suites[0].ID

	report := filepath.Join(t.TempDir(), "rerun.json")
	code, out, stderr := runCLI(t, "suite", "rerun", "--server", server, "--suite", first, "--report", report)
	if code != exitOK || !strings.Contains(out, "PASS") {
		t.Fatalf("rerun: exit %d, %s%s", code, out, stderr)
	}
	if raw, err := os.ReadFile(report); err != nil || !strings.Contains(string(raw), `"outcome": "pass"`) {
		t.Fatalf("report %s: %v", raw, err)
	}

	junit := filepath.Join(t.TempDir(), "first.xml")
	if code, out, stderr = runCLI(t, "suite", "junit", "--server", server, "--suite", first, "--out", junit); code != exitOK || !strings.Contains(out, "wrote") {
		t.Fatalf("junit: exit %d, %s%s", code, out, stderr)
	}
	raw, err := os.ReadFile(junit)
	if err != nil {
		t.Fatal(err)
	}
	var parsed engine.JUnitSuites
	if err := xml.Unmarshal(raw, &parsed); err != nil || parsed.Tests == 0 || parsed.Failures != 0 || len(parsed.Suites) != 1 {
		t.Fatalf("junit %s: %v", raw, err)
	}
	if code, _, stderr = runCLI(t, "suite", "junit", "--server", server, "--suite", first, "--out", junit); code != exitError || !strings.Contains(stderr, "never overwritten") {
		t.Fatalf("existing --out: exit %d, %s", code, stderr)
	}

	if code, _, stderr = runCLI(t, "suite", "rerun", "--server", server, "--suite", "suite-unknown"); code != exitError || !strings.Contains(stderr, "404") {
		t.Fatalf("unknown suite: exit %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "suite", "rerun", "--suite", first); code != exitError {
		t.Fatalf("rerun without --server: exit %d", code)
	}
	if code, _, _ = runCLI(t, "suite", "bogus"); code != exitError {
		t.Fatalf("unknown subcommand: exit %d", code)
	}
}
