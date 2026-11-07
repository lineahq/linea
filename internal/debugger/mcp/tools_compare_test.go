package mcp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

const compareAfterConfig = `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform:
    log_statements:
      - context: log
        statements: ['set(attributes["secret.marker"], "telemetry value")']
exporters: {otlp/backend: {endpoint: localhost:4317, tls: {insecure: true}}}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform], exporters: [otlp/backend]}
`

func compareArgs(mode platform.InputMode) map[string]any {
	args := runArgs(mode)
	args["before"] = args["config"]
	delete(args, "config")
	args["after"] = map[string]string{"text": compareAfterConfig}
	return args
}

func TestCompareConfigsToolComparesOneInputInBothModes(t *testing.T) {
	for _, mode := range []platform.InputMode{platform.ModeQuick, platform.ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, session, _ := newTestClient(t, platform.Options{})
			r, out := callTool(t, ctx, session, "linea_compare_configs", compareArgs(mode))
			text := r.Content[0].(*sdk.TextContent).Text
			if r.IsError || text != "Run comparison: outcome different; inputs identical; 1 of 1 exporter output differ; 1 injected records aligned, 1 changed." {
				t.Fatalf("text %q, result %+v", text, out)
			}
			for _, leaked := range []string{"secret", "telemetry value", "otlp", "transform"} {
				if strings.Contains(text, leaked) {
					t.Fatalf("text %q carries %q", text, leaked)
				}
			}
			c, _ := out["comparison"].(map[string]any)
			before, _ := out["before_run"].(map[string]any)
			after, _ := out["after_run"].(map[string]any)
			markdown, _ := out["markdown"].(string)
			if c["outcome"] != "different" || c["inputs_identical"] != true || before["state"] != "done" || after["state"] != "done" ||
				before["mode"] != string(mode) || !strings.HasPrefix(markdown, "# Run comparison: different\n") || !strings.Contains(markdown, "`telemetry value`") {
				t.Fatalf("result = %+v", out)
			}

			// The edited path, ignored, is counted and the runs are the same.
			r, out = callTool(t, ctx, session, "linea_compare_runs", map[string]any{
				"before_run_id": before["run_id"], "after_run_id": after["run_id"], "ignore": []string{`log.attributes["secret.marker"]`}})
			if text := r.Content[0].(*sdk.TextContent).Text; r.IsError || text != "Run comparison: outcome same; inputs identical; 0 of 1 exporter output differ; 1 injected records aligned, 0 changed; 1 rule-matched difference." {
				t.Fatalf("text %q, result %+v", text, out)
			}
		})
	}
}

func TestCompareConfigsWaitDoesNotCancelEitherRun(t *testing.T) {
	execute, started, release := blockedExecutor()
	ctx, session, service := newTestClient(t, platform.Options{Execute: execute})
	args := compareArgs(platform.ModeQuick)
	args["wait_seconds"] = 0
	r, out := callTool(t, ctx, session, "linea_compare_configs", args)
	before, _ := out["before_run"].(map[string]any)
	after, _ := out["after_run"].(map[string]any)
	if r.IsError || out["comparison"] != nil || before["run_id"] == nil || after["run_id"] == nil ||
		!strings.HasPrefix(r.Content[0].(*sdk.TextContent).Text, "Configuration comparison not finished within the wait: ") {
		t.Fatalf("result = %+v", out)
	}
	for _, id := range []any{before["run_id"], after["run_id"]} {
		receiveRun(t, ctx, started)
		release <- struct{}{}
		waitForRun(t, ctx, service, id.(string))
		if run, err := service.GetRun(ctx, caller, id.(string)); err != nil || run.State != platform.StateDone {
			t.Fatalf("run %v = %+v, %v", id, run, err)
		}
	}
	r, out = callTool(t, ctx, session, "linea_compare_runs", map[string]any{"before_run_id": "", "after_run_id": after["run_id"]})
	assertToolError(t, r, out, platform.CodeInvalidRequest, false)
	r, out = callTool(t, ctx, session, "linea_compare_runs", map[string]any{"before_run_id": "missing", "after_run_id": after["run_id"]})
	assertToolError(t, r, out, platform.CodeNotFound, false)
}

// TestVerifyRecommendationToolRunsTheCandidate: logs-cleanup's filter move,
// verified through MCP, gives the run's output; the summary names no
// component, and an unknown recommendation is not_found.
func TestVerifyRecommendationToolRunsTheCandidate(t *testing.T) {
	ctx, session, service := newTestClient(t, platform.Options{Paths: platform.PathPolicy{Unrestricted: true}})
	dir := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.SubmitRun(ctx, caller, platform.RunRequest{Mode: platform.ModeScenario, Config: platform.Source{Text: string(config)},
		Scenario: &platform.ScenarioInput{Scenario: platform.Source{Path: filepath.Join(dir, "scenario.yaml")}}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := service.GetRunReport(ctx, caller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	move := slices.IndexFunc(rep.Recommendations, func(r platform.Recommendation) bool { return r.Rule == "filter.move_earlier" })
	if move < 0 {
		t.Fatalf("no filter.move_earlier in %+v", rep.Recommendations)
	}
	r, out := callTool(t, ctx, session, "linea_verify_recommendation", map[string]any{"run_id": run.ID, "recommendation": move})
	text := r.Content[0].(*sdk.TextContent).Text
	candidate, _ := out["candidate"].(map[string]any)
	if r.IsError || out["confidence"] != "verified_on_input" || candidate["state"] != "done" || !strings.HasSuffix(text, "(verified_on_input).") {
		t.Fatalf("text %q, result %+v", text, out)
	}
	if strings.Contains(text, "filter/health") {
		t.Fatalf("text %q names a component", text)
	}
	r, out = callTool(t, ctx, session, "linea_verify_recommendation", map[string]any{"run_id": run.ID, "recommendation": len(rep.Recommendations)})
	assertToolError(t, r, out, platform.CodeNotFound, false)
}
