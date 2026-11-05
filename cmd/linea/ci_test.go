package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

const ciChildEnv = "LINEA_CI_CHILD"

// TestCLIChildProcessHelper enters the ordinary CLI only in an explicitly
// selected child process. The child owns a fresh local platform, with no server.
func TestCLIChildProcessHelper(t *testing.T) {
	if os.Getenv(ciChildEnv) != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(run(context.Background(), os.Args[i+1:], os.Stdout, os.Stderr))
		}
	}
	t.Fatal("CI child has no command")
}

func TestCIRunsAnExportedScenario(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample string
		quick  bool
		want   assertion.Outcome
	}{
		{"scenario_pass", "logs-cleanup", false, assertion.Pass},
		{"scenario_fail", "broken-routing", false, assertion.Fail},
		{"scenario_mocks", "container-logs", false, assertion.Pass},
		{"quick", "logs-cleanup", true, assertion.Pass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startServe(t)
			dir := filepath.Join("..", "..", "samples", tc.sample)
			config := filepath.Join(dir, "config.yaml")
			file := filepath.Join(t.TempDir(), "exported.scenario.yaml")
			var source *platform.AssertionReport
			var sourceCode int
			if tc.quick {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				var admitted api.Run
				decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, server+"/api/v1/runs", api.Request{
					ConfigYAML: ciReadFile(t, config), FixtureJSON: ciReadFile(t, filepath.Join(dir, "input.json")),
				}, http.StatusAccepted), &admitted)
				if admitted.ID == "" {
					t.Fatal("quick admission has no run ID")
				}
				// Wait through the server's report route before evaluating its
				// recorded source observations. The test never evaluates assertions.
				parityHTTP(t, ctx, http.MethodGet, server+"/api/v1/runs/"+admitted.ID+"/report?wait=true", nil, http.StatusOK)
				scenario := ciReadFile(t, filepath.Join(dir, "scenario.yaml"))
				var evaluated platform.AssertionReport
				decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, server+"/api/v1/runs/"+admitted.ID+"/assertions",
					api.AssertionsRequest{ScenarioYAML: scenario}, http.StatusOK), &evaluated)
				source, sourceCode = &evaluated, outcomeCode(evaluated.Outcome)
				// This sample ends with its assertions. Use those exact bytes for
				// export, and the same assertions in the evaluation scenario above.
				_, assertions, found := strings.Cut(scenario, "\nassertions:")
				if !found {
					t.Fatal("sample has no assertions")
				}
				var exported api.ScenarioExport
				decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, server+"/api/v1/runs/"+admitted.ID+"/export",
					api.ExportRequest{AssertionsYAML: "assertions:" + assertions}, http.StatusOK), &exported)
				if exported.RunID != admitted.ID || exported.File == "" {
					t.Fatalf("export = %+v", exported)
				}
				ciWriteFile(t, file, exported.File)
			} else {
				var id string
				sourceCode, id, source = ciSourceScenario(t, server, config, filepath.Join(dir, "scenario.yaml"))
				ciExportScenario(t, server, id, file)
			}
			if source.Outcome != tc.want || sourceCode != outcomeCode(tc.want) || len(source.Results) == 0 {
				t.Fatalf("source: exit %d, assertions %+v; want %s", sourceCode, source, tc.want)
			}
			code, child := ciReplayScenario(t, config, file, "")
			ciCompareOutcomes(t, sourceCode, source, code, child)
		})
	}
}

func ciSourceScenario(t *testing.T, server, config, scenario string) (int, string, *platform.AssertionReport) {
	t.Helper()
	code, stdout, stderr := runCLI(t, "test", "--server", server, "--config", config, "--scenario", scenario, "--format", "json")
	report := ciDecodeSuite(t, stdout)
	member := report.Scenarios[0]
	if member.RunID == "" || member.Error != "" || member.Assertions == nil || member.Outcome != report.Outcome || member.Assertions.Outcome != report.Outcome {
		t.Fatalf("source: exit %d, stderr %s, report %+v", code, stderr, report)
	}
	return code, member.RunID, member.Assertions
}

func ciExportScenario(t *testing.T, server, id, file string) {
	t.Helper()
	code, stdout, stderr := runCLI(t, "export", "--server", server, "--run", id, "--out", file)
	if code != exitOK {
		t.Fatalf("export: exit %d, stdout %s, stderr %s", code, stdout, stderr)
	}
}

func ciReplayScenario(t *testing.T, config, file, removeEnv string, imports ...string) (int, *platform.SuiteReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := []string{"-test.run=^TestCLIChildProcessHelper$", "--", "test", "--config", config, "--scenario", file, "--format", "json"}
	for _, name := range imports {
		args = append(args, "--import-env", name)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	// Windows environment names are case insensitive. Explicitly remove a
	// binding, rather than replacing it with an empty value.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, ciChildEnv) || (removeEnv != "" && strings.EqualFold(name, removeEnv)) {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, ciChildEnv+"=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("CI child timed out: %v; stdout %s; stderr %s", ctx.Err(), &stdout, &stderr)
	}
	code := exitOK
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("start CI child: %v; stderr %s", err, &stderr)
		}
		code = exit.ExitCode()
	}
	report := ciDecodeSuite(t, stdout.String())
	t.Logf("CI child: removed environment %q, exit %d, outcome %s", removeEnv, code, report.Outcome)
	return code, report
}

func ciCompareOutcomes(t *testing.T, sourceCode int, source *platform.AssertionReport, childCode int, child *platform.SuiteReport) {
	t.Helper()
	member := child.Scenarios[0]
	if childCode != sourceCode || child.Outcome != source.Outcome || member.Outcome != source.Outcome || member.Error != "" || member.Assertions == nil {
		t.Fatalf("source exit %d, outcome %s; child exit %d, report %+v", sourceCode, source.Outcome, childCode, child)
	}
	got := member.Assertions
	if got.Outcome != source.Outcome || len(got.Results) != len(source.Results) {
		t.Fatalf("source assertions %+v; child assertions %+v", source, got)
	}
	for i, want := range source.Results {
		if got.Results[i].Name != want.Name || got.Results[i].Outcome != want.Outcome {
			t.Errorf("assertion %d: source %q %s; child %q %s", i, want.Name, want.Outcome, got.Results[i].Name, got.Results[i].Outcome)
		}
	}
}

func ciDecodeSuite(t *testing.T, stdout string) *platform.SuiteReport {
	t.Helper()
	var report platform.SuiteReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil || len(report.Scenarios) != 1 || report.Outcome == "" {
		t.Fatalf("test report %s: %v", stdout, err)
	}
	return &report
}

func ciReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func ciWriteFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
