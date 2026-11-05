package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Saved revisions list, export for CI and import back through the CLI,
// as through HTTP and MCP.
func TestArtifactsCommandsListExportAndImport(t *testing.T) {
	server := startServe(t)
	resp := labDo(t, http.MethodGet, server+"/api/v1/artifacts", "", nil)
	var list artifact.Page
	err := json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if err != nil || list.Workspace.ID == "" {
		t.Fatalf("artifacts = %+v: %v", list, err)
	}
	info := list.Workspace
	save := func(req api.SaveRequest) artifact.Saved {
		t.Helper()
		req.WorkspaceID = info.ID
		body, _ := json.Marshal(req)
		resp := labDo(t, http.MethodPost, server+"/api/v1/artifacts", "application/json", bytes.NewReader(body))
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var saved artifact.Saved
		if err := json.Unmarshal(raw, &saved); err != nil || resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			t.Fatalf("save: %d %s", resp.StatusCode, raw)
		}
		return saved
	}
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := save(api.SaveRequest{Kind: artifact.Configuration, Label: "gateway.yaml", Content: config})
	scenario := save(api.SaveRequest{Kind: artifact.Scenario, Label: "checkout.yaml", Content: []byte("version: 1\nname: first\n")})
	revised := save(api.SaveRequest{ArtifactID: scenario.Artifact.ID, ExpectedGeneration: scenario.Artifact.Generation, ExpectedHead: scenario.Artifact.Head, Kind: artifact.Scenario, Content: []byte("version: 1\nname: second\n")})

	code, out, stderr := runCLI(t, "artifacts", "revisions", "--server", server, "--artifact", scenario.Artifact.ID, "--limit", "1")
	if code != exitOK || !strings.Contains(out, revised.Revision.Ref.RevisionID+" (head)") || strings.Contains(out, scenario.Revision.Ref.RevisionID) || !strings.Contains(out, "more: --cursor ") {
		t.Fatalf("revisions: exit %d, %s%s", code, out, stderr)
	}
	cursor := strings.TrimSpace(out[strings.LastIndex(out, "--cursor ")+len("--cursor "):])
	code, out, stderr = runCLI(t, "artifacts", "revisions", "--server", server, "--artifact", scenario.Artifact.ID, "--cursor", cursor, "--format", "json")
	var page platform.ArtifactRevisionPage
	if code != exitOK || json.Unmarshal([]byte(out), &page) != nil || len(page.Items) != 1 || page.Items[0].Ref.RevisionID != scenario.Revision.Ref.RevisionID || page.NextCursor != "" {
		t.Fatalf("second page: exit %d, %s%s", code, out, stderr)
	}

	archive := filepath.Join(t.TempDir(), "ci.zip")
	if code, out, stderr = runCLI(t, "artifacts", "export", "--server", server, "--config", cfg.Artifact.ID, "--scenario", scenario.Artifact.ID, "--out", archive); code != exitOK || !strings.Contains(out, "wrote") {
		t.Fatalf("export: exit %d, %s%s", code, out, stderr)
	}
	if zr, err := zip.OpenReader(archive); err != nil || len(zr.File) < 3 {
		t.Fatalf("archive: %v", err)
	} else {
		zr.Close()
	}
	if code, _, stderr = runCLI(t, "artifacts", "export", "--server", server, "--config", cfg.Artifact.ID, "--scenario", scenario.Artifact.ID, "--out", archive); code != exitError || !strings.Contains(stderr, "never overwritten") {
		t.Fatalf("existing --out: exit %d, %s", code, stderr)
	}

	code, out, stderr = runCLI(t, "artifacts", "import", "--server", server, "--archive", archive)
	if code != exitOK || strings.Count(out, "unchanged") != 2 || !strings.Contains(out, "collector/config.yaml") {
		t.Fatalf("import: exit %d, %s%s", code, out, stderr)
	}
	bogus := filepath.Join(t.TempDir(), "bogus.zip")
	if err := os.WriteFile(bogus, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr = runCLI(t, "artifacts", "import", "--server", server, "--archive", bogus); code != exitError || !strings.Contains(stderr, "import refused") {
		t.Fatalf("bogus archive: exit %d, %s", code, stderr)
	}

	if code, _, stderr = runCLI(t, "artifacts", "revisions", "--server", server, "--artifact", "missing"); code != exitError || !strings.Contains(stderr, "404") {
		t.Fatalf("unknown artifact: exit %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "artifacts", "export", "--server", server, "--config", cfg.Artifact.ID, "--out", archive); code != exitError {
		t.Fatalf("export without --scenario: exit %d", code)
	}
	if code, _, _ = runCLI(t, "artifacts", "bogus"); code != exitError {
		t.Fatalf("unknown subcommand: exit %d", code)
	}
}

// runs log prints a run's kept log, and says when a completed run kept none.
func TestRunsLogReportsWhenNoLogWasKept(t *testing.T) {
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	if code, _, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"), "--server", server); code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	code, out, stderr := runCLI(t, "runs", "--server", server, "--format", "json")
	var page api.RunPage
	if code != exitOK || json.Unmarshal([]byte(out), &page) != nil || len(page.Runs) != 1 {
		t.Fatalf("runs: exit %d, %s%s", code, out, stderr)
	}
	id := page.Runs[0].ID

	if code, out, stderr = runCLI(t, "runs", "log", "--server", server, "--run", id); code != exitOK || out != "" || !strings.Contains(stderr, "kept no log") {
		t.Fatalf("log: exit %d, %q %s", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "runs", "log", "--server", server, "--run", id, "--format", "json")
	var log platform.RunLog
	if code != exitOK || json.Unmarshal([]byte(out), &log) != nil || log != (platform.RunLog{RunID: id}) {
		t.Fatalf("json log: exit %d, %s%s", code, out, stderr)
	}
	if code, _, stderr = runCLI(t, "runs", "log", "--server", server, "--run", "missing"); code != exitError || !strings.Contains(stderr, "404") {
		t.Fatalf("unknown run: exit %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "runs", "log", "--run", id); code != exitError {
		t.Fatalf("log without --server: exit %d", code)
	}
}
