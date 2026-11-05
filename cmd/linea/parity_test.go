package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/samples"
)

// TestCrossInterfaceParity exercises the production serve composition. Each
// mode keeps the MCP-created and HTTP-created runs on the same platform.
func TestCrossInterfaceParity(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	fixturePath := filepath.Join(root, "input.json")
	scenarioPath := filepath.Join(root, "scenario.yaml")
	input, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	// MCP sends the scenario with its input file inlined.
	scenarioYAML := []byte(mcpScenario(t, scenarioPath)["text"])
	index, err := samples.List()
	if err != nil {
		t.Fatal(err)
	}
	var expect string
	for _, sample := range index {
		if sample.ID == "logs-cleanup" {
			expect = sample.Expect
		}
	}
	if expect == "" {
		t.Fatal("logs-cleanup has no declared expect in samples/index.yaml")
	}

	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			ctx, baseURL, _ := startMCPServe(t)
			client := sdk.NewClient(&sdk.Implementation{Name: "parity-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, mcpTransport(baseURL), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			mcpArgs := map[string]any{"config": mcpFile(t, configPath)}
			httpArgs := api.Request{ConfigYAML: mcpFile(t, configPath)["text"], ConfigLabel: configPath}
			cliArgs := []string{"run", "--config", configPath}
			fixtureSHA := ""
			if mode == "quick" {
				mcpArgs["input_json"] = string(input)
				httpArgs.FixtureJSON = string(input)
				cliArgs = append(cliArgs, "--fixture", fixturePath)
				fixtureSHA = fmt.Sprintf("%x", sha256.Sum256(input))
			} else {
				mcpArgs["scenario"] = mcpScenario(t, scenarioPath)
				httpArgs.ScenarioYAML, httpArgs.ScenarioLabel = mcpScenario(t, scenarioPath)["text"], scenarioPath
				cliArgs = append(cliArgs, "--scenario", scenarioPath)
			}
			var mcpRun, httpRun parityRun
			var mcpReport, httpReport *engine.Report

			if !t.Run("A_MCPToHTTPAndCLI", func(t *testing.T) {
				parityTool(t, ctx, session, "linea_run", mcpArgs, &mcpRun)
				assertParityRun(t, mcpRun, mcpRun.RunID, mode)
				raw := parityHTTPReport(t, ctx, baseURL, mcpRun.RunID)
				mcpReport = decodeParityReport(t, raw)
				assertParitySummary(t, mcpRun, mcpReport)
				assertParityPinnedInputs(t, ctx, baseURL, mcpRun.RunID, mode, configPath, scenarioPath, input, scenarioYAML)
				cliRaw := parityCLI(t, ctx, cliArgs)
				if mode == "quick" {
					cliFixture := decodeParityReport(t, cliRaw).Provenance.Fixture
					httpFixture := mcpReport.Provenance.Fixture
					if cliFixture == nil || httpFixture == nil {
						t.Fatal("quick reports must both identify the fixture")
					}
					if cliFixture.SHA256 != httpFixture.SHA256 || cliFixture.Path != fixturePath || httpFixture.Path != "" {
						t.Fatalf("fixture provenance before clearing labels: CLI = %+v, HTTP = %+v", cliFixture, httpFixture)
					}
				}
				// MCP sent the scenario with its files inlined, as a connected CLI does.
				sent := asSent(t, raw, append(append([]string{}, cliArgs...), "--server"))
				assertParityBytes(t, "HTTP / CLI normalized reports",
					normalizeParityReport(t, withCLIEvidenceBudget(t, sent, cliRaw), fixtureSHA), normalizeParityReport(t, cliRaw, fixtureSHA))
				if mode == "scenario" {
					// The CLI performs its own normalization before encoding. Do
					// not normalize that output again: this checks the helper.
					cliNormalized := parityCLI(t, ctx, append(append([]string{}, cliArgs...), "--normalize"))
					var encoded bytes.Buffer
					if err := engine.Encode(&encoded, decodeParityReport(t, cliNormalized)); err != nil {
						t.Fatal(err)
					}
					assertParityBytes(t, "helper / CLI --normalize honesty",
						normalizeParityReport(t, cliRaw, fixtureSHA), encoded.Bytes())
				}
			}) {
				return
			}

			if !t.Run("B_HTTPToMCPAndSharedHistory", func(t *testing.T) {
				var created api.Run
				decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, baseURL+"/api/v1/runs", httpArgs, http.StatusAccepted), &created)
				if created.ID == "" {
					t.Fatal("HTTP submission returned no run ID")
				}
				httpReport = decodeParityReport(t, parityHTTPReport(t, ctx, baseURL, created.ID))
				parityTool(t, ctx, session, "linea_get_run", map[string]string{"run_id": created.ID}, &httpRun)
				assertParityRun(t, httpRun, created.ID, mode)
				assertParitySummary(t, httpRun, httpReport)
				var page api.RunPage
				decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, baseURL+"/api/v1/runs", nil, http.StatusOK), &page)
				history := page.Runs
				for _, entry := range history {
					if entry.ID == mcpRun.RunID {
						if entry.State != api.StateDone {
							t.Fatalf("MCP-created run in HTTP history: state = %s", entry.State)
						}
						return
					}
				}
				t.Fatal("HTTP history does not list the MCP-created run")
			}) {
				return
			}

			if mode == "scenario" {
				t.Run("C_AssertionEvaluation", func(t *testing.T) {
					for _, source := range []struct {
						name   string
						run    parityRun
						report *engine.Report
					}{{"MCP-created", mcpRun, mcpReport}, {"HTTP-created", httpRun, httpReport}} {
						t.Run(source.name, func(t *testing.T) {
							url := baseURL + "/api/v1/runs/" + source.run.RunID + "/assertions"
							httpRaw := parityHTTP(t, ctx, http.MethodPost, url, api.AssertionsRequest{ScenarioYAML: string(scenarioYAML)}, http.StatusOK)
							var evaluated struct {
								RunID      string          `json:"run_id"`
								Assertions json.RawMessage `json:"assertions"`
							}
							parityTool(t, ctx, session, "linea_evaluate_assertions", map[string]string{
								"run_id": source.run.RunID, "scenario_yaml": string(scenarioYAML),
							}, &evaluated)
							if evaluated.RunID != source.run.RunID {
								t.Fatalf("evaluation run ID = %q, want %q", evaluated.RunID, source.run.RunID)
							}
							assertParityAssertions(t, httpRaw, evaluated.Assertions, expect)
							var recorded struct {
								RunID      string          `json:"run_id"`
								Mode       string          `json:"mode"`
								Assertions json.RawMessage `json:"assertions"`
							}
							parityTool(t, ctx, session, "linea_get_test_results", map[string]string{"run_id": source.run.RunID}, &recorded)
							if recorded.RunID != source.run.RunID || recorded.Mode != mode {
								t.Fatalf("test results identity = %q/%q", recorded.RunID, recorded.Mode)
							}
							reportRaw, err := json.Marshal(source.report.Assertions)
							if err != nil {
								t.Fatal(err)
							}
							assertParityAssertions(t, reportRaw, recorded.Assertions, expect)
							assertParityAssertions(t, httpRaw, recorded.Assertions, expect)
						})
					}
				})
			}
		})
	}
}

// The suite tool and CLI test use the same ordered admission path; compare
// outcomes through their public results, including the CLI's failing exit code.
func TestMCPSuiteAndCLITestParity(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.yaml")
	paths := []string{filepath.Join(root, "pass.yaml"), filepath.Join(root, "fail.yaml")}
	configYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp/backend: {endpoint: example.invalid:4317}}\nservice:\n  pipelines:\n    logs: {receivers: [otlp], exporters: [otlp/backend]}\n"
	if err := os.WriteFile(config, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		scenario := fmt.Sprintf("version: 1\nname: member %d\ninputs:\n  - receiver: otlp\n    data: '{\"resourceLogs\":[{\"scopeLogs\":[{\"logRecords\":[{\"body\":{\"stringValue\":\"parity fixture\"}}]}]}]}'\nassertions:\n  - target: {exporter: otlp/backend, signal: logs}\n    count: {equals: %d}\n", i+1, i+1)
		if err := os.WriteFile(path, []byte(scenario), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, baseURL, _ := startMCPServe(t)
	client := sdk.NewClient(&sdk.Implementation{Name: "suite-parity-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	var suite struct {
		SuiteID string            `json:"suite_id"`
		Done    bool              `json:"done"`
		Outcome assertion.Outcome `json:"outcome"`
		Members []struct {
			Label   string            `json:"label"`
			Name    string            `json:"name"`
			RunID   string            `json:"run_id"`
			Outcome assertion.Outcome `json:"outcome"`
		} `json:"members"`
	}
	parityTool(t, ctx, session, "linea_run_scenarios", map[string]any{
		"config":    mcpFile(t, config),
		"scenarios": []map[string]string{mcpScenario(t, paths[0]), mcpScenario(t, paths[1])},
	}, &suite)
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"test", "--config", config, "--scenario", paths[0], "--scenario", paths[1], "--format", "json"}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("CLI exit %d, want %d: %s", code, exitFail, stderr.String())
	}
	var cliReport engine.TestReport
	decodeParityJSON(t, stdout.Bytes(), &cliReport)
	if !suite.Done || suite.SuiteID == "" || suite.Outcome != assertion.Fail || suite.Outcome != cliReport.Outcome || len(suite.Members) != 2 || len(cliReport.Scenarios) != 2 {
		t.Fatalf("suite = %+v, CLI = %+v", suite, cliReport)
	}
	for i, member := range suite.Members {
		cli := cliReport.Scenarios[i]
		want := []assertion.Outcome{assertion.Pass, assertion.Fail}[i]
		if member.Label != paths[i] || cli.Path != paths[i] || member.Name != cli.Name || member.Outcome != want || member.Outcome != cli.Outcome || member.RunID == "" || cli.RunID == "" {
			t.Fatalf("member %d = %+v, CLI = %+v", i, member, cli)
		}
	}
}

// Only the fields being checked are decoded from MCP's structured result.
// Times are deliberately omitted; run IDs are used solely to select one run.
type parityRun struct {
	RunID   string `json:"run_id"`
	Mode    string `json:"mode"`
	State   string `json:"state"`
	Summary *struct {
		ExecutionStatus string `json:"execution_status"`
		Complete        bool   `json:"complete"`
		Stages          []struct {
			Component  string `json:"component"`
			Removed    int    `json:"removed"`
			Added      int    `json:"added"`
			Reconciled bool   `json:"reconciled"`
		} `json:"stages"`
		Outputs []struct {
			Exporter string `json:"exporter"`
			Records  int    `json:"records"`
		} `json:"outputs"`
	} `json:"summary"`
}

func assertParityRun(t *testing.T, got parityRun, id, mode string) {
	t.Helper()
	if id == "" || got.RunID != id || got.Mode != mode || got.State != "done" {
		t.Fatalf("MCP run = %q/%q, state %q; want %q/%q, state done", got.RunID, got.Mode, got.State, id, mode)
	}
}

func assertParitySummary(t *testing.T, run parityRun, report *engine.Report) {
	t.Helper()
	s := run.Summary
	if s == nil || report.Result == nil {
		t.Fatal("terminal run is missing summary or execution result")
	}
	if s.ExecutionStatus != string(report.Result.Status) || s.Complete != report.Result.Completed() {
		t.Fatalf("summary execution = %s/%t, report = %s/%t", s.ExecutionStatus, s.Complete, report.Result.Status, report.Result.Completed())
	}
	if len(s.Outputs) != len(report.Result.Outputs) {
		t.Fatalf("summary outputs = %d, report = %d", len(s.Outputs), len(report.Result.Outputs))
	}
	for i, output := range report.Result.Outputs {
		if got := s.Outputs[i]; got.Exporter != output.Exporter || got.Records != output.Counts.Items {
			t.Fatalf("summary output %d = %+v, report exporter/count = %s/%d", i, got, output.Exporter, output.Counts.Items)
		}
	}
	if len(s.Stages) != len(report.Stages) || len(report.Stages) == 0 {
		t.Fatalf("summary stages = %d, report = %d", len(s.Stages), len(report.Stages))
	}
	for i, stage := range report.Stages {
		removed, added := 0, 0
		count := func(diff *analysis.Diff) {
			if diff != nil {
				removed += len(diff.Removed)
				added += len(diff.Added)
			}
		}
		if stage.Reconciliation != nil {
			count(stage.Reconciliation)
		} else {
			for _, input := range stage.Inputs {
				count(input.Diff)
			}
		}
		got := s.Stages[i]
		if got.Component != stage.Component || got.Removed != removed || got.Added != added || got.Reconciled != (stage.Reconciliation != nil) {
			t.Fatalf("summary stage %d = %+v, report component/removed/added/reconciled = %s/%d/%d/%t", i, got, stage.Component, removed, added, stage.Reconciliation != nil)
		}
	}
}

func decodeParityJSON(t *testing.T, raw []byte, out any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("decode JSON: %v: %.300s", err, raw)
	}
}

func decodeParityReport(t *testing.T, raw []byte) *engine.Report {
	t.Helper()
	var report engine.Report
	decodeParityJSON(t, raw, &report)
	if report.Provenance == nil || report.Result == nil {
		t.Fatalf("missing report provenance or result: %.300s", raw)
	}
	return &report
}

// Both sides must carry the exact fixture bytes' SHA before the only permitted
// label adjustment. Config/scenario paths and every other field stay intact.
func normalizeParityReport(t *testing.T, raw []byte, fixtureSHA string) []byte {
	t.Helper()
	report := decodeParityReport(t, raw)
	// The pipeline's exporter fan-out (otlp/backend and the recorded debug
	// exporter) calls its exporters in varying order. As --normalize does,
	// the events are put in canonical order before the stages and lineage
	// are derived from them.
	report.Result.Canonicalize()
	stages, err := analysis.Analyze(report.Result.Events, report.Result.Snapshots, report.Result.Completed())
	if err != nil {
		t.Fatal(err)
	}
	report.Stages = stages
	lineage, err := analysis.Lineage(report.Result.Events, report.Result.Snapshots, report.Stages, report.Aggregations, report.Result.Completed())
	if err != nil {
		t.Fatal(err)
	}
	report.Lineage = lineage
	report.Normalize()
	// A lab runs on a registered runtime; a direct command on the built-in registry.
	if report.Runtime != nil && report.Runtime.Kind == engine.RuntimeRemote && report.Runtime.Name == "lab" {
		report.Runtime = engine.BuiltInRuntime()
	}
	if fixture := report.Provenance.Fixture; fixture != nil {
		if fixtureSHA == "" || fixture.SHA256 != fixtureSHA {
			t.Fatalf("fixture sha256 = %q, want %q before clearing path", fixture.SHA256, fixtureSHA)
		}
		fixture.Path = ""
	} else if fixtureSHA != "" {
		t.Fatal("missing fixture provenance")
	}
	var encoded bytes.Buffer
	if err := engine.Encode(&encoded, report); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// withCLIEvidenceBudget checks the one limit that differs by composition:
// serve keeps a run's evidence within its store's per-run allowance (16
// MiB by default, less 1 MiB of headroom), the CLI within the platform's 64
// MiB default. It returns serve's report with the CLI's budget, so the rest
// can be compared byte for byte.
func withCLIEvidenceBudget(t *testing.T, serveRaw, cliRaw []byte) []byte {
	t.Helper()
	serve, cli := decodeParityReport(t, serveRaw), decodeParityReport(t, cliRaw)
	if serve.Limits == nil || cli.Limits == nil || serve.Limits.EvidenceBytes != defaultRunLimit-1<<20 || cli.Limits.EvidenceBytes != 64<<20 {
		t.Fatalf("evidence budgets: serve %+v, CLI %+v", serve.Limits, cli.Limits)
	}
	serve.Limits.EvidenceBytes = cli.Limits.EvidenceBytes
	var encoded bytes.Buffer
	if err := engine.Encode(&encoded, serve); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func parityCLI(t *testing.T, ctx context.Context, args []string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, args, &stdout, &stderr); code != exitOK {
		t.Fatalf("CLI exit %d: %s", code, stderr.String())
	}
	return stdout.Bytes()
}

func parityTool(t *testing.T, ctx context.Context, session *sdk.ClientSession, name string, args, out any) {
	t.Helper()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.StructuredContent == nil {
		t.Fatalf("%s tool error or missing structured result: %.300s", name, raw)
	}
	decodeParityJSON(t, raw, out)
}

func parityHTTP(t *testing.T, ctx context.Context, method, url string, body any, want int) []byte {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token := os.Getenv(serverTokenEnv); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want != 0 && response.StatusCode != want {
		t.Fatalf("%s %s: status %d, want %d: %.300s", method, url, response.StatusCode, want, raw)
	}
	return raw
}

func parityHTTPReport(t *testing.T, ctx context.Context, baseURL, id string) []byte {
	t.Helper()
	url := baseURL + "/api/v1/runs/" + id
	// Inspect state even if report retrieval fails, so a non-done run is
	// reported with its actual state after the blocking wait.
	raw := parityHTTP(t, ctx, http.MethodGet, url+"/report?wait=true", nil, 0)
	var metadata api.Run
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, url, nil, http.StatusOK), &metadata)
	if metadata.ID != id || metadata.State != api.StateDone {
		t.Fatalf("HTTP run after wait: id = %q, state = %s, error = %q", metadata.ID, metadata.State, metadata.Error)
	}
	return raw
}

func assertParityAssertions(t *testing.T, left, right []byte, expect string) {
	t.Helper()
	var a, b assertion.Report
	decodeParityJSON(t, left, &a)
	decodeParityJSON(t, right, &b)
	if string(a.Outcome) != expect || b.Outcome != a.Outcome || len(a.Results) == 0 || len(a.Results) != len(b.Results) {
		t.Fatalf("assertions outcome/count = %s/%d and %s/%d, declared outcome = %s", a.Outcome, len(a.Results), b.Outcome, len(b.Results), expect)
	}
	for i, result := range a.Results {
		if got := b.Results[i]; got.Name != result.Name || got.Outcome != result.Outcome {
			t.Fatalf("assertion %d name/outcome = %q/%s and %q/%s", i, result.Name, result.Outcome, got.Name, got.Outcome)
		}
	}
	// Canonicalize the complete wire objects, including checks and messages;
	// this also detects shape differences beyond the typed fields above.
	canonical := func(raw []byte) []byte {
		var value any
		decodeParityJSON(t, raw, &value)
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	assertParityBytes(t, "full assertion report JSON", canonical(left), canonical(right))
}

func assertParityBytes(t *testing.T, label string, left, right []byte) {
	t.Helper()
	if bytes.Equal(left, right) {
		return
	}
	i := 0
	for i < len(left) && i < len(right) && left[i] == right[i] {
		i++
	}
	start := max(0, i-100)
	t.Fatalf("%s differ at byte %d (lengths %d/%d)\nleft: %q\nright: %q", label, i, len(left), len(right),
		left[start:min(len(left), i+200)], right[start:min(len(right), i+200)])
}

// assertParityPinnedInputs reads an MCP-created run's pinned inputs through
// HTTP (the web's run inputs view): the bytes MCP admitted, labelled with
// the paths the client named.
func assertParityPinnedInputs(t *testing.T, ctx context.Context, baseURL, id, mode, configPath, scenarioPath string, input, scenarioYAML []byte) {
	t.Helper()
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var pinned api.RunInputs
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodGet, baseURL+"/api/v1/runs/"+id+"/inputs", nil, http.StatusOK), &pinned)
	if pinned.RunID != id || pinned.Mode != mode || pinned.Config.Text != string(config) || pinned.Config.Label != configPath {
		t.Fatalf("pinned config of the MCP run = %+v", pinned)
	}
	if mode == "quick" && (pinned.Fixture == nil || pinned.Fixture.Text != string(input) || pinned.Scenario != nil) {
		t.Fatalf("pinned quick input = %+v", pinned)
	}
	if mode == "scenario" && (pinned.Scenario == nil || pinned.Scenario.Text != string(scenarioYAML) || pinned.Scenario.Label != scenarioPath ||
		pinned.Fixture != nil || len(pinned.Files) != 0) {
		t.Fatalf("pinned scenario = %+v", pinned)
	}
}
