package mcp

import (
	"regexp"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var evaluationToolText = regexp.MustCompile(`^(Evaluations: [0-9]+ evaluations? recorded|Recorded evaluation: outcome (pass|fail|inconclusive|error); [0-9]+ pass, [0-9]+ fail, [0-9]+ inconclusive, [0-9]+ error)\.$`)

// Recorded evaluations list and read back, and an evaluation is unknown
// under another run.
func TestEvaluationToolsListAndReadRecordedEvaluations(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	id := runID(t, r, out)
	r, out = callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	other := runID(t, r, out)

	if r, out = callTool(t, ctx, session, "linea_list_evaluations", map[string]any{"run_id": id}); r.IsError || len(out["evaluations"].([]any)) != 0 || textOf(t, r) != "Evaluations: 0 evaluations recorded." {
		t.Fatalf("empty = %s, %+v", textOf(t, r), out)
	}
	r, out = callTool(t, ctx, session, "linea_evaluate_assertions", map[string]any{"run_id": id, "scenario_yaml": runScenario(1)})
	eid, _ := out["evaluation_id"].(string)
	if r.IsError || eid == "" {
		t.Fatalf("evaluate = %+v", out)
	}
	r, out = callTool(t, ctx, session, "linea_list_evaluations", map[string]any{"run_id": id})
	list, _ := out["evaluations"].([]any)
	if r.IsError || len(list) != 1 || list[0].(map[string]any)["id"] != eid || list[0].(map[string]any)["client"] != "mcp" || textOf(t, r) != "Evaluations: 1 evaluation recorded." {
		t.Fatalf("list = %s, %+v", textOf(t, r), out)
	}
	r, out = callTool(t, ctx, session, "linea_get_evaluation", map[string]any{"run_id": id, "evaluation_id": eid})
	if result, _ := out["result"].(map[string]any); r.IsError || out["outcome"] != "pass" || result["outcome"] != "pass" || textOf(t, r) != "Recorded evaluation: outcome pass; 1 pass, 0 fail, 0 inconclusive, 0 error." {
		t.Fatalf("get = %s, %+v", textOf(t, r), out)
	}
	for _, args := range []map[string]any{{"run_id": other, "evaluation_id": eid}, {"run_id": id, "evaluation_id": "eval-missing"}} {
		if r, out = callTool(t, ctx, session, "linea_get_evaluation", args); !r.IsError || out["code"] != string(platform.CodeNotFound) {
			t.Fatalf("%v = %+v", args, out)
		}
	}
	if r, out = callTool(t, ctx, session, "linea_list_evaluations", map[string]any{"run_id": "run-missing"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("missing run = %+v", out)
	}
}
