package mcp

import (
	"encoding/base64"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Saved revisions list, export for CI and import back over MCP, as
// over HTTP.
func TestArtifactRevisionExportAndImportTools(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	ctx, session := connectDevelopmentService(t, service)
	info, err := service.GetWorkspaceInfo(ctx, caller)
	if err != nil {
		t.Fatal(err)
	}
	save := func(kind platform.ArtifactKind, label, content string) *platform.SavedArtifact {
		t.Helper()
		saved, err := service.SaveArtifact(ctx, caller, platform.SaveArtifactRequest{WorkspaceID: info.ID, Kind: kind, Label: label, Content: []byte(content)})
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	config := save(artifact.Configuration, "gateway.yaml", runConfig)
	scenario := save(artifact.Scenario, "checkout.yaml", "version: 1\nname: first\n")
	if _, err := service.SaveArtifact(ctx, caller, platform.SaveArtifactRequest{WorkspaceID: info.ID, ArtifactID: scenario.Artifact.ID, ExpectedGeneration: scenario.Artifact.Generation, ExpectedHead: scenario.Artifact.Head, Kind: artifact.Scenario, Content: []byte("version: 1\nname: second\n")}); err != nil {
		t.Fatal(err)
	}

	r, page := callTool(t, ctx, session, "linea_list_artifact_revisions", map[string]any{"artifact_id": scenario.Artifact.ID, "limit": 1})
	assertDevelopmentText(t, r, page)
	if r.IsError || len(page["items"].([]any)) != 1 || page["next_cursor"] == "" {
		t.Fatal(page)
	}
	r, rest := callTool(t, ctx, session, "linea_list_artifact_revisions", map[string]any{"artifact_id": scenario.Artifact.ID, "cursor": page["next_cursor"]})
	if r.IsError || len(rest["items"].([]any)) != 1 || rest["next_cursor"] != nil {
		t.Fatal(rest)
	}
	if first := rest["items"].([]any)[0].(map[string]any)["ref"].(map[string]any); first["revision_id"] != scenario.Revision.Ref.RevisionID {
		t.Fatalf("oldest revision = %v", first)
	}

	r, exp := callTool(t, ctx, session, "linea_export_artifacts_for_ci", map[string]any{"configuration_id": config.Artifact.ID, "scenario_ids": []string{scenario.Artifact.ID}})
	assertDevelopmentText(t, r, exp)
	archive, err := base64.StdEncoding.DecodeString(exp["archive_base64"].(string))
	if r.IsError || err != nil || exp["name"] != "collector-ci.zip" || int(exp["bytes"].(float64)) != len(archive) {
		t.Fatalf("export = %v: %v", exp, err)
	}

	r, imp := callTool(t, ctx, session, "linea_import_artifacts", map[string]any{"archive_base64": exp["archive_base64"]})
	assertDevelopmentText(t, r, imp)
	items := imp["items"].([]any)
	if r.IsError || len(items) != 2 {
		t.Fatal(imp)
	}
	for _, item := range items {
		if item.(map[string]any)["status"] != "unchanged" {
			t.Fatalf("import = %v", imp)
		}
	}

	r, out := callTool(t, ctx, session, "linea_import_artifacts", map[string]any{"archive_base64": "not base64!"})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
	r, out = callTool(t, ctx, session, "linea_list_artifact_revisions", map[string]any{"artifact_id": "missing"})
	assertToolError(t, r, out, platform.CodeNotFound, false)
	r, out = callTool(t, ctx, session, "linea_export_artifacts_for_ci", map[string]any{"configuration_id": config.Artifact.ID, "scenario_ids": []string{}})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
}
