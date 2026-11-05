package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/scenario"
)

// An export is the derived scenario after a one-fact-per-line header; it never
// carries host environment values or provider-captured file contents.
func TestExportScenarioHeadsTheScenarioWithoutPrivateValues(t *testing.T) {
	const hostSecret, fileSecret = "distinctive-f5-host-value", "distinctive-f5-file-value"
	t.Setenv("OTEL_F5_HOST", hostSecret)
	secretFile := filepath.Join(t.TempDir(), "value.txt")
	if err := os.WriteFile(secretFile, []byte(fileSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	config := strings.Replace(bundleConfig, "exporters:", "processors: {resource: {attributes: [{key: host, value: '${env:OTEL_F5_HOST}', action: upsert}, {key: file, value: '${file:"+filepath.ToSlash(secretFile)+"}', action: upsert}, {key: declared, value: '${env:OTEL_F5_DECLARED}', action: upsert}]}}\nexporters:", 1)
	config = strings.Replace(config, "receivers: [otlp], exporters:", "receivers: [otlp], processors: [resource], exporters:", 1)
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			s := New(Options{Environment: EnvironmentPolicy{Host: []string{"OTEL_F5_HOST"}}, Paths: PathPolicy{Unrestricted: true}})
			defer s.Close()
			req := RunRequest{Mode: mode, Config: Source{Text: config}}
			setup := "environment: {OTEL_F5_DECLARED: 'user-declared-value'}\n"
			if mode == ModeQuick {
				req.Quick = &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}, Setup: setup}
			} else {
				req.Scenario = &ScenarioInput{Scenario: Source{Text: "version: 1\n" + setup + "inputs: [{data: '" + strings.TrimSpace(scenarioRunFixture(t)) + "'}]\n" + scenarioRunAssertion}}
			}
			id, rep := scenarioRunSubmit(t, s, req)
			if data := string(rep.Result.Outputs[0].Data); !strings.Contains(data, hostSecret) || !strings.Contains(data, fileSecret) {
				t.Fatal("private values were not used by the run")
			}
			out, err := s.ExportScenario(context.Background(), testCaller, id, ScenarioFromRunRequest{Name: "exported"})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.Export, hostSecret) || strings.Contains(out.Export, fileSecret) || !strings.Contains(out.Export, "user-declared-value") {
				t.Fatalf("export boundary violated:\n%s", out.Export)
			}
			header, body, ok := strings.Cut(out.Export, "\nversion: 1")
			if !ok || "version: 1"+body != out.YAML || out.Saved != nil {
				t.Fatalf("export is not header + scenario:\n%s", out.Export)
			}
			for _, want := range []string{"run " + id + ".", "linea " + release.ProductVersion + " with Collector " + release.CollectorVersion,
				"Build: ", "Required environment: OTEL_F5_HOST (names only", "Reproducibility: unverified.", "Not included: the Collector configuration"} {
				if !strings.Contains(header, want) {
					t.Errorf("header lacks %q:\n%s", want, header)
				}
			}
			for _, line := range strings.Split(header, "\n") {
				if !strings.HasPrefix(line, "# ") {
					t.Fatalf("header line %q is not a comment", line)
				}
			}
			sc, err := scenario.Parse([]byte(out.Export), "")
			if err != nil || sc.Name != "exported" {
				t.Fatalf("export does not parse: %v", err)
			}
		})
	}
}

func TestExportScenarioIsNeverSaved(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	id, _ := scenarioRunSubmit(t, s, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig}, Quick: &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}}})
	for _, req := range []ScenarioFromRunRequest{{Save: true}, {Label: "x.yaml"}} {
		if _, err := s.ExportScenario(context.Background(), testCaller, id, req); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("ExportScenario(%+v) = %v", req, err)
		}
	}
	if _, err := s.ExportScenario(context.Background(), testCaller, "missing", ScenarioFromRunRequest{}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown run = %v", err)
	}
}

func TestExportHeaderKeepsEachFactOnItsLine(t *testing.T) {
	header := exportHeader("run-1", &engine.BundleParts{HostEnvironment: []string{"A\nversion: 2", "B\u2028C"}, ProductVersion: "1\r\n2", Build: "b"})
	if strings.Count(header, "\n") != 7 || strings.Contains(header, "\nversion") || strings.Contains(header, "\u2028") {
		t.Fatalf("header:\n%s", header)
	}
}
