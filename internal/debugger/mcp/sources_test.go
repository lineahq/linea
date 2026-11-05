package mcp

import (
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// A run tool takes a saved revision as a source; the run records it.
func TestRunToolsTakeSavedRevisions(t *testing.T) {
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
	saved, err := service.SaveArtifact(ctx, caller, platform.SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Configuration, Label: "config", Content: []byte(runConfig)})
	if err != nil {
		t.Fatal(err)
	}
	ref := map[string]string{"artifact_id": saved.Artifact.ID, "revision_id": saved.Revision.Ref.RevisionID}
	for _, mode := range []platform.InputMode{platform.ModeQuick, platform.ModeScenario} {
		args := runArgs(mode)
		args["config"] = map[string]any{"artifact": ref}
		r, out := callTool(t, ctx, session, "linea_run", args)
		id := runID(t, r, out)
		if out["state"] != "done" {
			t.Fatalf("%s run = %+v", mode, out)
		}
		run, err := service.GetRun(ctx, caller, id)
		if err != nil || run.Bundle == nil || run.Bundle.Sources == nil || run.Bundle.Sources.Config.RevisionID != saved.Revision.Ref.RevisionID {
			t.Fatalf("%s bundle = %+v, %v", mode, run.Bundle, err)
		}
	}

	for name, config := range map[string]any{
		"text and artifact": map[string]any{"text": runConfig, "artifact": ref},
		"no revision":       map[string]any{"artifact": map[string]string{"artifact_id": saved.Artifact.ID}},
		"unknown artifact":  map[string]any{"artifact": map[string]string{"artifact_id": "missing", "revision_id": saved.Revision.Ref.RevisionID}},
	} {
		args := runArgs(platform.ModeQuick)
		args["config"] = config
		r, out := callTool(t, ctx, session, "linea_run", args)
		code := platform.CodeInvalidRequest
		if name == "unknown artifact" {
			code = platform.CodeNotFound
		}
		assertToolError(t, r, out, code, false)
	}
}
