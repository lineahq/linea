package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/server"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/store/pg"
)

// TestMain points the user's configuration directory at a temporary one, so
// no test, nor a server process a test starts, reads or writes the
// developer's own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "linea-config-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("APPDATA", dir)
	os.Setenv("XDG_CONFIG_HOME", dir)
	// Team servers keep their records in memory instead of PostgreSQL.
	server.OpenStore = func(_ context.Context, c pg.Config) (artifact.Repository, error) { return memory.New(c.Limits), nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var (
	config   = filepath.Join("..", "..", "testdata", "traces", "config.yaml")
	fixture  = filepath.Join("..", "..", "testdata", "traces", "traces.json")
	scenario = filepath.Join("..", "..", "testdata", "traces", "scenario.yaml")
)

func decodeReport(t *testing.T, out string) engine.Report {
	t.Helper()
	var rep engine.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestRunReportsFormats(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, out string)
	}{
		{
			name: "json with comparison",
			args: []string{"run", "--config", config, "--fixture", fixture, "--compare"},
			check: func(t *testing.T, out string) {
				rep := decodeReport(t, out)
				if rep.Result == nil || rep.Result.Status != runresult.StatusCompleted || rep.Comparison == nil || !rep.Comparison.Equal {
					t.Fatalf("unexpected report: %s", out)
				}
				if len(rep.Stages) != 3 || len(rep.OTTL) != 1 || rep.OTTL[0].Verification != ottldebug.VerificationReplayVerified {
					t.Fatalf("stages/ottl: %+v %+v", rep.Stages, rep.OTTL)
				}
				filter := rep.Stages[1]
				if filter.Component != "filter" || filter.Inputs[0].Outcome != analysis.OutcomeEmitted ||
					len(filter.Inputs[0].Diff.Removed) != 1 || filter.Inputs[0].Diff.Removed[0].Name != "SELECT orders" {
					t.Fatalf("filter outcome: %+v", filter)
				}
				if batch := rep.Stages[2]; batch.Emission != analysis.EmissionAsynchronous || batch.Reconciliation == nil || !batch.Reconciliation.Empty() {
					t.Fatalf("batch outcome: %+v", batch)
				}
			},
		},
		{
			name: "text",
			args: []string{"run", "--config", config, "--fixture", fixture, "--format", "text"},
			check: func(t *testing.T, out string) {
				for _, want := range []string{
					"stage.input        transform",
					`removed span "SELECT orders"`,
					"batch in traces (asynchronous)",
					"transform input seq 8: replay_verified",
					"exporter otlp (traces): 1 batch(es), 2 item(s)",
				} {
					if !strings.Contains(out, want) {
						t.Errorf("output lacks %q:\n%s", want, out)
					}
				}
			},
		},
		{
			name: "scenario",
			args: []string{"run", "--config", config, "--scenario", scenario, "--compare"},
			check: func(t *testing.T, out string) {
				rep := decodeReport(t, out)
				if rep.Scenario != "checkout-traces" || !rep.Comparison.Equal {
					t.Fatalf("unexpected report: %s", out)
				}
				// The scenario environment reached the OTTL statement.
				if !strings.Contains(string(rep.Result.Outputs[0].Data), `"scenario"`) {
					t.Fatalf("scenario environment not applied: %s", rep.Result.Outputs[0].Data)
				}
			},
		},
		{
			name: "text with mocks",
			args: []string{"run", "--format", "text",
				"--config", filepath.Join("..", "..", "testdata", "receivers", "kubernetes", "config.yaml"),
				"--scenario", filepath.Join("..", "..", "testdata", "receivers", "kubernetes", "scenario.yaml")},
			check: func(t *testing.T, out string) {
				for _, want := range []string{
					"at receiver otlp from client 10.42.0.17",
					"mocked processor/k8sattributes (kubernetes, dependency_mocked): 3 fixture(s) supplied",
					"    supplied: Pod shop/checkout-7d9f8b6c4-x2x9q",
					"    limitation: the API connection",
				} {
					if !strings.Contains(out, want) {
						t.Errorf("output lacks %q:\n%s", want, out)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tt.args, &stdout, &stderr); code != exitOK {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			tt.check(t, stdout.String())
		})
	}
}

func TestRunRejectsInvalidArguments(t *testing.T) {
	tests := [][]string{
		{"run", "--config", config},
		{"run", "--config", config, "--fixture", fixture, "--scenario", scenario},
		{"run", "--config", config, "--fixture", fixture, "--format", "yaml"},
		{"run", "--config", config, "--fixture", fixture, "extra"},
		{"inspect"},
		{"version", "--format", "yaml"},
		{"bogus"},
		{},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitError {
			t.Fatalf("%v: exit %d, want %d", args, code, exitError)
		}
		if !strings.Contains(stderr.String(), "usage:") {
			t.Fatalf("%v: stderr lacks usage: %s", args, stderr.String())
		}
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"run", "-h"}, {"inspect", "-help"}} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		if !strings.Contains(stdout.String()+stderr.String(), "usage: linea") {
			t.Fatalf("%v: no usage printed", args)
		}
	}
}

func TestInspectReportsValidity(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "config.yaml")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [batch], exporters: [nop]}}}\n"
	if err := os.WriteFile(invalid, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(dir, "unknown.yaml")
	yaml = "receivers: {kafka: {}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [kafka], exporters: [nop]}}}\n"
	if err := os.WriteFile(unknown, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		code int
		want []string
	}{
		{
			name: "valid text",
			args: []string{"inspect", "--config", config},
			code: exitOK,
			want: []string{": valid", "pipelines:", "receiver/otlp", "simulated", "captured"},
		},
		{
			name: "valid json",
			args: []string{"inspect", "--config", config, "--format", "json"},
			code: exitOK,
			want: []string{`"valid": true`, `"topology"`},
		},
		{
			name: "invalid",
			args: []string{"inspect", "--config", invalid},
			code: exitError,
			want: []string{": INVALID", "batch"},
		},
		{
			name: "unknown component",
			args: []string{"inspect", "--config", unknown},
			code: exitOK,
			want: []string{": valid", "warning unsupported_component", "[receiver/kafka]", "stand_in"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), tt.args, &stdout, &stderr); code != tt.code {
				t.Fatalf("exit %d, want %d: %s", code, tt.code, stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestVersionReportsPinnedVersions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version", "--format", "json"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var v engine.Versions
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Product != release.ProductVersion || v.Collector != release.CollectorVersion ||
		v.ReportSchema != engine.ReportSchemaVersion || len(v.Modules) == 0 {
		t.Fatalf("versions = %+v", v)
	}

	stdout.Reset()
	if code := run(context.Background(), []string{"version"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "linea "+release.ProductVersion+"\ncollector "+release.CollectorVersion) {
		t.Fatalf("text = %s", stdout.String())
	}
}

var multisignalDir = filepath.Join("..", "..", "testdata", "multisignal")

// testScenario writes a scenario injecting the multisignal logs fixture with one
// assertion.
func testScenario(t *testing.T, assertion string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join(multisignalDir, "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	yaml := "version: 1\nname: cli\ninputs: [{receiver: otlp, file: '" + filepath.ToSlash(fixture) + "'}]\nassertions: [" + assertion + "]\n"
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTestExitCodes checks that each test outcome has its own exit code and
// that a failure prints expected and observed values.
func TestTestExitCodes(t *testing.T) {
	cfg := filepath.Join(multisignalDir, "config.yaml")
	tests := []struct {
		name string
		args []string
		code int
		want []string
	}{
		{
			name: "pass",
			args: []string{"--tests", filepath.Join(multisignalDir, "tests")},
			code: exitOK,
			want: []string{"PASS         scrubbing", "PASS: 1 passed, 0 failed"},
		},
		{
			name: "fail",
			args: []string{"--scenario", testScenario(t, "{name: one record, target: {exporter: otlp/backend, signal: logs}, count: {equals: 2}}")},
			code: exitFail,
			want: []string{
				"FAIL         one record: exporter otlp/backend (logs)",
				"fail count: expected == 2, observed 1",
				"records per stage: logs: in 2 -> resource 2 -> attributes/scrub 2 -> filter/health 1",
			},
		},
		{
			name: "inconclusive",
			args: []string{"--scenario", testScenario(t, "{target: {exporter: otlp/backend, signal: traces}, attributes: {absent: [user.password]}}")},
			code: exitInconclusive,
			want: []string{"inconclusive attributes.absent user.password", "no records reached the target"},
		},
		{
			name: "error",
			args: []string{"--scenario", testScenario(t, "{target: {exporter: kafka, signal: logs}, count: {min: 1}}")},
			code: exitError,
			want: []string{"error target: no logs pipeline exports to kafka"},
		},
		{
			name: "error outranks fail",
			args: []string{"--scenario", testScenario(t, "{target: {exporter: otlp/backend, signal: logs}, count: {equals: 2}}"), "--scenario", filepath.Join(multisignalDir, "scenario.yaml")},
			code: exitError,
			want: []string{"scenario declares no assertions", "ERROR: 0 passed, 1 failed, 0 inconclusive, 1 errors"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append([]string{"test", "--config", cfg}, tt.args...)
			if code := run(context.Background(), args, &stdout, &stderr); code != tt.code {
				t.Fatalf("exit %d, want %d: %s%s", code, tt.code, stdout.String(), stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestTestWritesReports(t *testing.T) {
	cfg := filepath.Join(multisignalDir, "config.yaml")
	failing := testScenario(t, "{name: leak, target: {exporter: otlp/backend, signal: logs}, attributes: {present: [user.password]}}")
	dir := t.TempDir()
	junit, report := filepath.Join(dir, "junit.xml"), filepath.Join(dir, "report.json")
	for _, path := range []string{junit, report} {
		var stdout, stderr bytes.Buffer
		args := []string{"test", "--config", cfg, "--tests", filepath.Join(multisignalDir, "tests"), "--scenario", failing, "--report", path}
		if code := run(context.Background(), args, &stdout, &stderr); code != exitFail {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	}

	raw, err := os.ReadFile(junit)
	if err != nil {
		t.Fatal(err)
	}
	var suites engine.JUnitSuites
	if err := xml.Unmarshal(raw, &suites); err != nil {
		t.Fatal(err)
	}
	if suites.Tests != 5 || suites.Failures != 1 || suites.Errors != 0 || len(suites.Suites) != 2 {
		t.Fatalf("junit = %s", raw)
	}
	leak := suites.Suites[0].Cases[0]
	if leak.Name != "leak" || leak.Failure == nil || leak.Failure.Type != "fail" ||
		!strings.Contains(leak.Failure.Message, "expected present, observed 1 of 1 record(s) violate") ||
		!strings.Contains(leak.Failure.Text, "at exporter otlp/backend resource[0].scope[0].log_record[0]: absent") {
		t.Fatalf("leak case = %+v", leak)
	}

	raw, err = os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	var rep engine.TestReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != assertion.Fail || len(rep.Scenarios) != 2 || rep.Scenarios[1].Outcome != assertion.Pass {
		t.Fatalf("json report = %s", raw)
	}
}

func TestTestRejectsInvalidArguments(t *testing.T) {
	cfg := filepath.Join(multisignalDir, "config.yaml")
	tests := [][]string{
		{"test", "--config", cfg},
		{"test", "--scenario", filepath.Join(multisignalDir, "scenario.yaml")},
		{"test", "--config", cfg, "--tests", filepath.Join(multisignalDir, "tests"), "--report", "out.txt"},
		{"test", "--config", cfg, "--tests", t.TempDir()},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitError {
			t.Fatalf("%v: exit %d, want %d", args, code, exitError)
		}
	}
}

// TestAPIMatchesCLIOutput requires that the same run is consumable by the CLI
// formatter and by an API client, byte for byte. The API client sends the
// files' content, labelled with their paths.
func TestAPIMatchesCLIOutput(t *testing.T) {
	configYAML := mcpFile(t, config)["text"]
	for _, tt := range []struct {
		name string
		args []string
		req  api.Request
	}{
		{"scenario", []string{"--scenario", scenario}, api.Request{ConfigYAML: configYAML, ConfigLabel: config,
			ScenarioYAML: mcpScenario(t, scenario)["text"], ScenarioLabel: scenario, Normalize: true}},
		{"quick", []string{"--fixture", fixture}, api.Request{ConfigYAML: configYAML, ConfigLabel: config,
			FixtureJSON: mcpFile(t, fixture)["text"], FixtureLabel: fixture, Normalize: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assertAPIMatchesCLIOutput(t, tt.args, tt.req)
		})
	}
}

func assertAPIMatchesCLIOutput(t *testing.T, inputArgs []string, request api.Request) {
	t.Helper()
	var cli, stderr bytes.Buffer
	args := append([]string{"run", "--config", config, "--normalize"}, inputArgs...)
	if code := run(context.Background(), args, &cli, &stderr); code != exitOK {
		t.Fatalf("cli exit %d: %s", code, stderr.String())
	}

	h := api.NewHandler(nil)
	defer h.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/api/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created api.Run
	err = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create: status %d, %v", resp.StatusCode, err)
	}
	resp, err = http.Get(srv.URL + "/api/v1/runs/" + created.ID + "/report?wait=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	got = asSent(t, got, append(append([]string{}, inputArgs...), "--server"))
	if !bytes.Equal(got, cli.Bytes()) {
		t.Fatalf("API response differs from CLI output\napi: %.300s\ncli: %.300s", got, cli.Bytes())
	}
}

// TestServeIsNotACommand: the team server is linea-server; the CLI has no
// serve command.
func TestServeIsNotACommand(t *testing.T) {
	code, _, stderr := runCLI(t, "serve", "--config", "team.yaml")
	if code != exitError || !strings.Contains(stderr, `unknown command "serve"`) {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
