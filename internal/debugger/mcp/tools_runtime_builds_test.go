package mcp

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const (
	plainBuildManifest = "dist:\n  otelcol_version: 0.162.0\n"
	// localBuildManifest builds a module from a local directory, which MCP
	// cannot upload.
	localBuildManifest = "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n"
)

// TestRuntimeBuildToolsQueueListAndCancel queues builds on a team server's
// build queue, reads, pages and cancels them; the text states counts and
// states only, never the name or log.
func TestRuntimeBuildToolsQueueListAndCancel(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, session := connectDevelopmentService(t, service)

	r, first := callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "edge", "manifest_yaml": plainBuildManifest, "wait_seconds": 0})
	text := textOf(t, r)
	if r.IsError || first["state"] != "queued" || first["reused"] != false || first["verified"] != true || first["build_id"] == "" ||
		text != "Runtime build: state queued, not finished within the wait; verified true; 5 manifest warnings; 0 log lines returned of 0." || strings.Contains(text, "edge") {
		t.Fatalf("build = %s, %+v", text, first)
	}
	r, got := callTool(t, ctx, session, "linea_get_runtime_build", map[string]any{"build_id": first["build_id"], "log_from": 0})
	if r.IsError || got["build_id"] != first["build_id"] || got["state"] != "queued" {
		t.Fatalf("get = %+v", got)
	}
	r, cancelled := callTool(t, ctx, session, "linea_cancel_runtime_build", map[string]any{"build_id": first["build_id"]})
	if r.IsError || cancelled["state"] != "cancelled" || textOf(t, r) != "Runtime build: state cancelled; verified true; 5 manifest warnings; 0 log lines returned of 0." {
		t.Fatalf("cancel = %s, %+v", textOf(t, r), cancelled)
	}
	r, second := callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "other", "manifest": map[string]any{"text": plainBuildManifest}, "wait_seconds": 0})
	if r.IsError || second["state"] != "queued" {
		t.Fatalf("second build = %+v", second)
	}

	r, page := callTool(t, ctx, session, "linea_list_runtime_builds", map[string]any{"limit": 1})
	builds, _ := page["builds"].([]any)
	if r.IsError || len(builds) != 1 || builds[0].(map[string]any)["build_id"] != second["build_id"] || page["next_cursor"] == nil ||
		textOf(t, r) != "Runtime builds: 1 listed, newest first; 1 queued or running; more true." {
		t.Fatalf("first page = %s, %+v", textOf(t, r), page)
	}
	r, rest := callTool(t, ctx, session, "linea_list_runtime_builds", map[string]any{"cursor": page["next_cursor"]})
	builds, _ = rest["builds"].([]any)
	if r.IsError || len(builds) != 1 || builds[0].(map[string]any)["build_id"] != first["build_id"] || rest["next_cursor"] != nil ||
		textOf(t, r) != "Runtime builds: 1 listed, newest first; 0 queued or running; more false." {
		t.Fatalf("second page = %s, %+v", textOf(t, r), rest)
	}
	if r, out := callTool(t, ctx, session, "linea_list_runtime_builds", map[string]any{"limit": 501}); !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("limit past the maximum = %+v", out)
	}
}

// TestRuntimeBuildToolsMapRefusals keeps manifest diagnostics: MCP uploads
// no sources, so a manifest with a local directory is refused, and the
// shapes and unknown builds map to their codes.
func TestRuntimeBuildToolsMapRefusals(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, session := connectDevelopmentService(t, service)

	r, out := callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "edge", "manifest_yaml": localBuildManifest})
	diags, _ := out["diagnostics"].([]any)
	if !r.IsError || out["code"] != string(platform.CodeValidationFailed) || len(diags) != 1 {
		t.Fatalf("manifest with a local directory = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "edge"})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("build without a manifest = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "Bad Name", "manifest_yaml": plainBuildManifest})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("a bad name = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_build_runtime", map[string]any{"name": "edge", "manifest": map[string]any{"path": "manifest.yaml"}})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("a manifest path = %+v", out)
	}
	for _, tool := range []string{"linea_get_runtime_build", "linea_cancel_runtime_build"} {
		r, out = callTool(t, ctx, session, tool, map[string]any{"build_id": "missing"})
		if !r.IsError || out["code"] != string(platform.CodeNotFound) {
			t.Fatalf("%s of a missing build = %+v", tool, out)
		}
	}
}
