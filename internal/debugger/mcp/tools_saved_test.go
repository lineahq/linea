package mcp

import (
	"encoding/base64"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var savedWorkToolText = regexp.MustCompile(`^(Artifacts: [0-9]+ listed; more pages (true|false)|Artifact: generation [0-9]+; (no revision read|revision of [0-9]+ bytes, base64 encoded)|Artifact saved: generation [0-9]+, [0-9]+ bytes|Samples: [0-9]+ listed)\.$`)

// Saved work saves, lists, reads and refuses a stale edit with the current
// head over MCP, as over HTTP; samples list without a workspace.
func TestSavedWorkToolsSaveListReadAndConflict(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, session := connectDevelopmentService(t, service)
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	r, out := callTool(t, ctx, session, "linea_save_artifact", map[string]any{"kind": "configuration", "label": "gateway.yaml", "content_base64": encode(runConfig)})
	if r.IsError || textOf(t, r) != "Artifact saved: generation 1, "+strconv.Itoa(len(runConfig))+" bytes." {
		t.Fatalf("save = %s, %+v", textOf(t, r), out)
	}
	first := out["artifact"].(map[string]any)
	id, head := first["id"].(string), first["head_revision_id"].(string)

	edit := map[string]any{"artifact_id": id, "expected_generation": 1, "expected_head_revision_id": head, "kind": "configuration", "label": "gateway.yaml", "content_base64": encode(runConfig + "# edited\n")}
	if r, out = callTool(t, ctx, session, "linea_save_artifact", edit); r.IsError || out["artifact"].(map[string]any)["generation"] != float64(2) {
		t.Fatalf("revise = %+v", out)
	}
	second := out["revision"].(map[string]any)["ref"].(map[string]any)["revision_id"].(string)
	// The same stale expectation now conflicts and carries the current head.
	r, out = callTool(t, ctx, session, "linea_save_artifact", edit)
	if current, _ := out["current"].(map[string]any); !r.IsError || out["code"] != string(platform.CodeConflict) || current["head_revision_id"] != second {
		t.Fatalf("stale save = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_save_artifact", map[string]any{"kind": "configuration", "label": "x", "content_base64": "%%"}); !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("bad base64 = %+v", out)
	}

	r, out = callTool(t, ctx, session, "linea_list_artifacts", map[string]any{"kind": "configuration"})
	info, _ := service.GetWorkspaceInfo(ctx, caller)
	if items, _ := out["items"].([]any); r.IsError || len(items) != 1 || out["workspace"].(map[string]any)["id"] != info.ID || textOf(t, r) != "Artifacts: 1 listed; more pages false." {
		t.Fatalf("list = %s, %+v", textOf(t, r), out)
	}

	if r, out = callTool(t, ctx, session, "linea_get_artifact", map[string]any{"artifact_id": id}); r.IsError || out["revision"] != nil || textOf(t, r) != "Artifact: generation 2; no revision read." {
		t.Fatalf("get = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_get_artifact", map[string]any{"artifact_id": id, "revision_id": head})
	content, _ := base64.StdEncoding.DecodeString(out["revision"].(map[string]any)["content_base64"].(string))
	if r.IsError || string(content) != runConfig {
		t.Fatalf("first revision = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_artifact", map[string]any{"artifact_id": "missing"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("missing = %+v", out)
	}

	// No workspace: artifacts are unavailable, samples still list.
	ctx, session, _ = newTestClient(t, platform.Options{})
	if r, out = callTool(t, ctx, session, "linea_list_artifacts", map[string]any{}); !r.IsError || out["code"] != string(platform.CodeUnavailable) {
		t.Fatalf("ephemeral list = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_list_samples", map[string]any{})
	list, _ := out["samples"].([]any)
	if r.IsError || len(list) == 0 || !strings.HasPrefix(textOf(t, r), "Samples: ") {
		t.Fatalf("samples = %+v", out)
	}
	if s := list[0].(map[string]any); s["id"] == "" || s["config_yaml"] == "" || s["scenario_yaml"] == "" {
		t.Fatalf("sample = %+v", s)
	}
}
