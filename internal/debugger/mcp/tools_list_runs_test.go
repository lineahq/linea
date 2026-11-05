package mcp

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestListRunsToolSummarizesFiltersAndLimits(t *testing.T) {
	execute := func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		status := runresult.StatusCompleted
		if req.ScenarioYAML != "" {
			status = runresult.StatusPartial
		}
		return &engine.Report{Schema: engine.ReportSchemaVersion, Scenario: "errors",
			Runtime: &engine.ReportRuntime{Kind: engine.RuntimeBuiltIn, CollectorVersion: "0.162.0", Verified: true},
			Result:  &runresult.Result{RunID: req.RunID, Status: status, Inputs: []runresult.InputResult{{Injected: true}}}}, nil
	}
	ctx, session, service := newTestClient(t, platform.Options{Execute: execute})
	var ids []string
	for _, mode := range []platform.InputMode{platform.ModeQuick, platform.ModeScenario, platform.ModeQuick} {
		r, out := callTool(t, ctx, session, "linea_run", runArgs(mode))
		id := runID(t, r, out)
		waitForRun(t, ctx, service, id)
		ids = append(ids, id)
	}

	r, out := callTool(t, ctx, session, "linea_list_runs", map[string]any{"limit": 2})
	runs, _ := out["runs"].([]any)
	cursor, _ := out["next_cursor"].(string)
	if r.IsError || len(runs) != 2 || cursor == "" {
		t.Fatalf("list = %+v", out)
	}
	_, next := callTool(t, ctx, session, "linea_list_runs", map[string]any{"limit": 2, "cursor": cursor})
	if older, _ := next["runs"].([]any); len(older) != 1 || older[0].(map[string]any)["run_id"] != ids[0] || next["next_cursor"] != nil {
		t.Fatalf("next page = %+v", next)
	}
	first := runs[0].(map[string]any)
	summary, _ := first["summary"].(map[string]any)
	if first["run_id"] != ids[2] || summary["scenario"] != "errors" || summary["status"] != "completed" || summary["injected"] != float64(1) ||
		summary["runtime"].(map[string]any)["name"] != platform.BuiltInRuntime {
		t.Fatalf("newest run = %+v", first)
	}

	_, out = callTool(t, ctx, session, "linea_list_runs", map[string]any{"result": "partial", "runtime": "built-in"})
	if runs, _ := out["runs"].([]any); len(runs) != 1 || runs[0].(map[string]any)["run_id"] != ids[1] {
		t.Fatalf("filtered = %+v", out)
	}

	r, out = callTool(t, ctx, session, "linea_list_runs", map[string]any{"result": "passed"})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
	r, out = callTool(t, ctx, session, "linea_list_runs", map[string]any{"limit": 101})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
}
