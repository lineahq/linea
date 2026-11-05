package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestEvaluateAssertionsUsesRecordedObservationsAndPreservesRun(t *testing.T) {
	emptyTarget := strings.Replace(runScenario(1), "signal: logs", "signal: traces", 1)
	emptyTarget = strings.Replace(emptyTarget, "count: {equals: 1}", "attributes: {absent: [secret]}", 1)
	ignoredInput := strings.Replace(runScenario(1), "data: '"+previewPayload+"'", "file: never-read.json", 1)
	for _, mode := range []platform.InputMode{platform.ModeQuick, platform.ModeScenario} {
		for _, tt := range []struct {
			name, yaml string
			outcome    assertion.Outcome
			counts     string
		}{
			{"pass", runScenario(1), assertion.Pass, "1 pass, 0 fail, 0 inconclusive, 0 error"},
			{"fail", runScenario(2), assertion.Fail, "0 pass, 1 fail, 0 inconclusive, 0 error"},
			{"inconclusive", emptyTarget, assertion.Inconclusive, "0 pass, 0 fail, 1 inconclusive, 0 error"},
			{"inputs ignored", ignoredInput, assertion.Pass, "1 pass, 0 fail, 0 inconclusive, 0 error"},
		} {
			t.Run(string(mode)+"/"+tt.name, func(t *testing.T) {
				var executions atomic.Int32
				ctx, session, service := newTestClient(t, platform.Options{Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
					executions.Add(1)
					return engine.Execute(ctx, req)
				}})
				args := runArgs(mode)
				if tt.outcome == assertion.Inconclusive {
					// The real Collector receives logs only; this configured traces
					// target receives no records. Attribute checks stay inconclusive.
					args["config"] = map[string]string{"text": runConfig + "    traces: {receivers: [otlp], exporters: [otlp/backend]}\n"}
				}
				r, out := callTool(t, ctx, session, "linea_run", args)
				id := runID(t, r, out)
				if out["state"] != "done" {
					t.Fatal(out)
				}
				before, err := service.GetRun(ctx, caller, id)
				if err != nil {
					t.Fatal(err)
				}
				beforeJSON, err := json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				r, recordedBefore := callTool(t, ctx, session, "linea_get_test_results", map[string]any{"run_id": id})
				runID(t, r, recordedBefore)
				beforeReport, err := service.GetRunReport(ctx, caller, id, false)
				if err != nil {
					t.Fatal(err)
				}
				beforeReportJSON, err := json.Marshal(beforeReport)
				if err != nil {
					t.Fatal(err)
				}

				r, out = callTool(t, ctx, session, "linea_evaluate_assertions", map[string]any{"run_id": id, "scenario_yaml": tt.yaml})
				if runID(t, r, out) != id || len(out) != 3 {
					t.Fatalf("evaluation = %+v", out)
				}
				recorded, err := service.GetEvaluation(ctx, caller, out["evaluation_id"].(string))
				if err != nil || !recorded.Recorded || recorded.RunID != id || recorded.Outcome != tt.outcome {
					t.Fatalf("recorded evaluation = %+v, %v", recorded, err)
				}
				evaluation, err := service.EvaluateAssertions(ctx, caller, id, platform.AssertionsRequest{ScenarioYAML: tt.yaml})
				if err != nil {
					t.Fatal(err)
				}
				want := evaluation.Result
				if err != nil || want == nil || want.Outcome != tt.outcome {
					t.Fatalf("evaluation = %+v, %v; want %s", want, err, tt.outcome)
				}
				encoded, err := json.Marshal(want)
				if err != nil {
					t.Fatal(err)
				}
				var expected any
				if err := json.Unmarshal(encoded, &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(out["assertions"], expected) || !strings.Contains(string(encoded), "ignore previous instructions") {
					t.Fatalf("assertions differ from the platform: %+v", out)
				}
				text := "Evaluated assertions: outcome " + string(tt.outcome) + "; " + tt.counts + "."
				if got := r.Content[0].(*sdk.TextContent).Text; got != text {
					t.Fatalf("evaluation text = %q, want %q", got, text)
				}
				if tt.outcome != assertion.Pass && (len(want.Results) != 1 || len(want.Results[0].Checks) != 1 || want.Results[0].Checks[0].Message == "") {
					t.Fatal("evaluation lost the failed or inconclusive check message")
				}
				if tt.outcome == assertion.Inconclusive && !strings.Contains(want.Results[0].Checks[0].Message, "no records reached the target") {
					t.Fatalf("inconclusive must come from an empty target: %+v", want)
				}

				r, recordedAfter := callTool(t, ctx, session, "linea_get_test_results", map[string]any{"run_id": id})
				runID(t, r, recordedAfter)
				after, err := service.GetRun(ctx, caller, id)
				if err != nil {
					t.Fatal(err)
				}
				afterJSON, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				if string(beforeJSON) != string(afterJSON) || !reflect.DeepEqual(recordedBefore, recordedAfter) {
					t.Fatal("evaluation changed the run or its recorded assertions")
				}
				afterReport, err := service.GetRunReport(ctx, caller, id, false)
				if err != nil {
					t.Fatal(err)
				}
				afterReportJSON, err := json.Marshal(afterReport)
				if err != nil || string(beforeReportJSON) != string(afterReportJSON) {
					t.Fatal("evaluation changed the full report: ", err)
				}
				if got := executions.Load(); got != 1 {
					t.Fatalf("evaluation re-executed the run: %d executor calls", got)
				}
			})
		}
	}
}

func TestEvaluateAssertionsPreservesPlatformErrors(t *testing.T) {
	ctx, session, service := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	id := runID(t, r, out)
	for _, tt := range []struct {
		name, id, yaml string
		code           platform.Code
	}{
		{"malformed YAML", id, "version: 1\nassertions: [", platform.CodeValidationFailed},
		{"empty YAML", id, "", platform.CodeInvalidRequest},
		{"unknown run", "unknown", runScenario(1), platform.CodeNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.EvaluateAssertions(ctx, caller, tt.id, platform.AssertionsRequest{ScenarioYAML: tt.yaml})
			code := platform.ErrorCode(err)
			if code != tt.code {
				t.Fatalf("platform error = %v, code %s; want %s", err, code, tt.code)
			}
			t.Logf("observed platform code: %s", code)
			r, out := callTool(t, ctx, session, "linea_evaluate_assertions", map[string]any{"run_id": tt.id, "scenario_yaml": tt.yaml})
			assertToolError(t, r, out, code, false)
			if code == platform.CodeValidationFailed {
				diagnostics, ok := out["diagnostics"].([]any)
				if !ok || len(diagnostics) == 0 {
					t.Fatal("validation_failed lacks diagnostics")
				}
			}
		})
	}
}

func TestEvaluateAssertionsRejectsUnfinishedRun(t *testing.T) {
	for _, mode := range []platform.InputMode{platform.ModeQuick, platform.ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			execute, started, release := blockedExecutor()
			ctx, session, service := newTestClient(t, platform.Options{Execute: execute})
			args := runArgs(mode)
			args["wait_seconds"] = 0
			r, out := callTool(t, ctx, session, "linea_run", args)
			id := runID(t, r, out)
			if got := receiveRun(t, ctx, started); got != id {
				t.Fatalf("worker started %s, want %s", got, id)
			}
			r, out = callTool(t, ctx, session, "linea_evaluate_assertions", map[string]any{"run_id": id, "scenario_yaml": runScenario(1)})
			assertToolError(t, r, out, platform.CodeConflict, false)
			release <- struct{}{}
			waitForRun(t, ctx, service, id)
			assertRunState(t, ctx, service, id, mode, platform.StateDone)
		})
	}
}

func TestEvaluateAssertionsMapsRecordingCap(t *testing.T) {
	ctx, session, service := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	id := runID(t, r, out)
	for range 100 {
		if _, err := service.EvaluateAssertions(ctx, caller, id, platform.AssertionsRequest{ScenarioYAML: runScenario(1), Record: true}); err != nil {
			t.Fatal(err)
		}
	}
	r, out = callTool(t, ctx, session, "linea_evaluate_assertions", map[string]string{"run_id": id, "scenario_yaml": runScenario(1)})
	assertToolError(t, r, out, platform.CodeResourceExhausted, false)
	assertDevelopmentText(t, r, out, id)
	if evaluations, err := service.ListEvaluations(ctx, caller, id); err != nil || len(evaluations) != 100 {
		t.Fatalf("recordings = %d, %v", len(evaluations), err)
	}
}
