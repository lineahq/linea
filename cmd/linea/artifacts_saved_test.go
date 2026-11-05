package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// artifacts save, list and show print the server's JSON, refuse a stale edit
// with the current head, and samples print the same JSON locally and
// connected.
func TestArtifactsSaveListShowAndSamplesMatchHTTP(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(file, []byte("receivers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw := parityCLI(t, ctx, []string{"artifacts", "save", "--server", server, "--file", file, "--kind", "configuration", "--format", "json"})
	var saved platform.SavedArtifact
	decodeParityJSON(t, raw, &saved)
	if saved.Artifact.Label != file || saved.Artifact.Generation != 1 {
		t.Fatalf("saved = %+v", saved)
	}
	id, first := saved.Artifact.ID, saved.Artifact.Head
	if err := os.WriteFile(file, []byte("receivers: {otlp: {}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	revise := []string{"artifacts", "save", "--server", server, "--file", file, "--kind", "configuration", "--artifact", id, "--generation", "1", "--head", first}
	if code, out, stderr := runCLI(t, revise...); code != exitOK || !strings.Contains(out, "generation 2") {
		t.Fatalf("revise: exit %d, %s%s", code, out, stderr)
	}
	var meta platform.ArtifactMetadata
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/artifacts/"+id, nil, http.StatusOK), &meta)
	if code, _, stderr := runCLI(t, revise...); code != exitError || !strings.Contains(stderr, "409") || !strings.Contains(stderr, "head "+meta.Head) {
		t.Fatalf("stale revise: exit %d, %s", code, stderr)
	}

	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"artifacts", "list"}, "/api/v1/artifacts"},
		{[]string{"artifacts", "list", "--kind", "configuration", "--limit", "1"}, "/api/v1/artifacts?kind=configuration&limit=1"},
		{[]string{"artifacts", "show", "--artifact", id}, "/api/v1/artifacts/" + id},
		{[]string{"artifacts", "show", "--artifact", id, "--revision", first}, "/api/v1/artifacts/" + id + "/revisions/" + first},
		{[]string{"samples"}, "/api/v1/samples"},
	} {
		got := parityCLI(t, ctx, append(tc.args, "--server", server, "--format", "json"))
		assertParityBytes(t, strings.Join(tc.args, " "), got, parityHTTP(t, ctx, http.MethodGet, server+tc.path, nil, http.StatusOK))
	}
	assertParityBytes(t, "samples local", parityCLI(t, ctx, []string{"samples", "--format", "json"}), parityCLI(t, ctx, []string{"samples", "--server", server, "--format", "json"}))
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(parityCLI(t, ctx, []string{"samples", "--format", "json"}), &listed); err != nil || len(listed) == 0 {
		t.Fatalf("samples: %v", err)
	}
	if out := string(parityCLI(t, ctx, []string{"samples"})); !strings.Contains(out, listed[0].ID) {
		t.Fatalf("samples text = %s", out)
	}

	// The head's content prints as saved; --out writes it once.
	if out := string(parityCLI(t, ctx, []string{"artifacts", "show", "--server", server, "--artifact", id, "--revision", "head"})); out != "receivers: {otlp: {}}\n" {
		t.Fatalf("head content = %q", out)
	}
	out := filepath.Join(dir, "first.yaml")
	show := []string{"artifacts", "show", "--server", server, "--artifact", id, "--revision", first, "--out", out}
	if code, _, stderr := runCLI(t, show...); code != exitOK {
		t.Fatalf("show --out: exit %d, %s", code, stderr)
	}
	if content, err := os.ReadFile(out); err != nil || string(content) != "receivers: {}\n" {
		t.Fatalf("first revision = %q: %v", content, err)
	}
	if code, _, stderr := runCLI(t, show...); code != exitError || !strings.Contains(stderr, "never overwritten") {
		t.Fatalf("existing --out: exit %d, %s", code, stderr)
	}
	if code, out, _ := runCLI(t, "artifacts", "list", "--server", server); code != exitOK || !strings.Contains(out, id) || !strings.Contains(out, "configuration") {
		t.Fatalf("list text: exit %d, %s", code, out)
	}

	if code, _, stderr := runCLI(t, "artifacts", "show", "--server", server, "--artifact", "missing"); code != exitError || !strings.Contains(stderr, "server refused") {
		t.Fatalf("missing artifact: exit %d, %s", code, stderr)
	}
	for _, args := range [][]string{{"artifacts", "list"}, {"artifacts", "save", "--server", server, "--file", file}, {"artifacts", "show", "--server", server, "--artifact", id, "--out", out}} {
		if code, _, _ := runCLI(t, args...); code != exitError {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}
