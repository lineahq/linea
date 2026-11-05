package mcp

import (
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/internal/debugger/platform"
)

var runtimeManagementToolText = regexp.MustCompile(`^(Runtime: one built runtime; verified (true|false); [0-9]+ custom modules?|Runtime removed|Runtime pushed|Default runtime: (none chosen, the server.s own|chosen by the team)|Runtime builds: [0-9]+ listed, newest first; [0-9]+ queued or running; more (true|false)|Manifest retargeted: [0-9]+ modules? moved, [0-9]+ modules? missing from the release, [0-9]+ custom modules? kept|Collector support: [0-9]+ releases? verified|Collector catalog: [0-9]+ components?|Runtime storage: [0-9]+ runtime images?, [0-9]+ cache volumes?, [0-9]+ left-over containers?; -?[0-9]+ bytes reclaimable|Runtime prune: [0-9]+ removed, [0-9]+ kept; -?[0-9]+ bytes reclaimed)\.$`)

// v0162 is a built runtime the test stores hold; builds are a CLI command.
var v0162 = platform.RuntimeInfo{Name: "v0162", ImageID: "sha256:v0162", Tags: []string{"linea-runtime:v0162"}, CollectorVersion: "0.162.0", Verified: true, Manifest: "dist:\n  otelcol_version: 0.162.0\n"}

// TestRuntimeManagementToolsMapThePlatform reads, pushes, measures and
// removes a built runtime through MCP; the text never names it.
func TestRuntimeManagementToolsMapThePlatform(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{Runtimes: &memoryRuntimes{images: map[string]platform.RuntimeInfo{"v0162": v0162}}})

	r, out := callTool(t, ctx, session, "linea_get_runtime", map[string]any{"name": "v0162"})
	if rt, _ := out["runtime"].(map[string]any); r.IsError || rt["name"] != "v0162" || textOf(t, r) != "Runtime: one built runtime; verified true; 0 custom modules." {
		t.Fatalf("get = %s, %+v", textOf(t, r), out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_runtime", map[string]any{"name": "gone"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("missing runtime = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_push_runtime", map[string]any{"name": "v0162", "target": "registry.example.com/team/rt:1"}); r.IsError || out["pushed"] != true {
		t.Fatalf("push = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_get_runtime_storage", map[string]any{})
	if list, _ := out["runtimes"].([]any); r.IsError || len(list) != 1 || strings.Contains(textOf(t, r), "v0162") {
		t.Fatalf("storage = %s, %+v", textOf(t, r), out)
	} else if rt, _ := list[0].(map[string]any); rt["name"] != "v0162" || rt["state"] == nil || rt["in_use"] != false {
		t.Fatalf("stored runtime = %+v", rt)
	}
	if r, out = callTool(t, ctx, session, "linea_import_runtime", map[string]any{"ref": "elsewhere:1"}); !r.IsError {
		t.Fatalf("import of an unknown image = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_delete_runtime", map[string]any{"name": "v0162"}); r.IsError || out["removed"] != true {
		t.Fatalf("delete = %+v", out)
	}
}

// TestCollectorReleaseToolsReadTheSupportTable reads the support table and
// a catalog, and moves a manifest between releases.
func TestCollectorReleaseToolsReadTheSupportTable(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_get_collector_support", map[string]any{})
	verified, _ := out["verified"].([]any)
	if r.IsError || len(verified) == 0 || out["built_in"] == "" {
		t.Fatalf("support = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_get_collector_catalog", map[string]any{})
	if components, _ := out["components"].([]any); r.IsError || len(components) == 0 || out["collector_version"] == "" {
		t.Fatalf("catalog = %s", textOf(t, r))
	}
	if r, out = callTool(t, ctx, session, "linea_get_collector_catalog", map[string]any{"version": "0.1.0"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("catalog of an unverified release = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_retarget_manifest", map[string]any{"manifest_yaml": string(build.Manifest), "version": "0.160.0"})
	if moved, _ := out["moved"].([]any); r.IsError || out["to"] != "0.160.0" || len(moved) == 0 || !strings.Contains(out["manifest"].(string), "otelcol_version: 0.160.0") {
		t.Fatalf("retarget = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_retarget_manifest", map[string]any{"version": "0.160.0"}); !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("retarget without a manifest = %+v", out)
	}
}

// TestDefaultRuntimeToolsMapThePlatform reads the default runtime before a
// choice, chooses the built-in runtime and refuses a runtime the catalog
// does not hold; the text never names a runtime.
func TestDefaultRuntimeToolsMapThePlatform(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})

	r, out := callTool(t, ctx, session, "linea_get_default_runtime", map[string]any{})
	if r.IsError || out["name"] != platform.BuiltInRuntime || out["set"] != nil || out["set_by"] != nil || textOf(t, r) != "Default runtime: none chosen, the server's own." {
		t.Fatalf("get before a choice = %s, %+v", textOf(t, r), out)
	}
	if r, out = callTool(t, ctx, session, "linea_set_default_runtime", map[string]any{"name": "gone"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("set of a runtime outside the catalog = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_set_default_runtime", map[string]any{"name": platform.BuiltInRuntime})
	if r.IsError || out["name"] != platform.BuiltInRuntime || out["set_by"] == nil || out["set"] == nil || textOf(t, r) != "Default runtime: chosen by the team." {
		t.Fatalf("set = %s, %+v", textOf(t, r), out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_default_runtime", map[string]any{}); r.IsError || out["set"] == nil {
		t.Fatalf("get after a choice = %+v", out)
	}
}
