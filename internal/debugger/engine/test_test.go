package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

// writeScenario writes a scenario that injects the multisignal traces fixture
// and declares the given assertions YAML.
func writeScenario(t *testing.T, name, assertions string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "multisignal", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	yaml := "version: 1\nname: " + name + "\ninputs: [{receiver: otlp, file: '" + filepath.ToSlash(fixture) + "'}]\n" +
		"environment: {DEPLOY_ENV: test}\nassertions:\n" + assertions
	path := filepath.Join(t.TempDir(), name+".yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExecuteEvaluatesAssertions runs assertions against processor and
// exporter targets of real runs, including per-pipeline attribution of a
// shared processor and fan-in at a shared exporter.
func TestExecuteEvaluatesAssertions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fanin := filepath.Join("..", "..", "..", "testdata", "multisignal", "fanin.yaml")
	tests := []struct {
		name       string
		config     string
		assertions string
		want       []assertion.Outcome
		message    string
	}{
		{
			name:   "drops and counts",
			config: multisignalConfig,
			assertions: `  - {target: {processor: filter/health, signal: traces}, dropped: {equals: 1}, count: {equals: 1}}
  - {target: {processor: attributes/scrub, signal: traces}, attributes: {absent: [user.password], types: {http.route: string}}}
`,
			want: []assertion.Outcome{assertion.Pass, assertion.Pass},
		},
		{
			name:       "unexpected drop count",
			config:     multisignalConfig,
			assertions: "  - {target: {processor: filter/health, signal: traces}, dropped: {equals: 0}}\n",
			want:       []assertion.Outcome{assertion.Fail},
			message:    "processor filter/health in traces dropped 1 of 2 span(s), expected == 0",
		},
		{
			name:   "shared processor per pipeline",
			config: fanin,
			assertions: `  - {target: {processor: batch, pipeline: traces/web, signal: traces}, count: {equals: 1}}
  - {target: {processor: batch, pipeline: traces/all, signal: traces}, count: {equals: 2}}
  - {target: {exporter: otlp/backend, signal: traces}, count: {equals: 3}}
`,
			want: []assertion.Outcome{assertion.Pass, assertion.Pass, assertion.Pass},
		},
		{
			name:       "ambiguous shared processor",
			config:     fanin,
			assertions: "  - {target: {processor: batch, signal: traces}, count: {min: 1}}\n",
			want:       []assertion.Outcome{assertion.Error},
			message:    "set target.pipeline",
		},
		{
			name:       "fan-in count names every pipeline",
			config:     fanin,
			assertions: "  - {target: {exporter: otlp/backend, signal: traces}, count: {equals: 4}}\n",
			want:       []assertion.Outcome{assertion.Fail},
			message:    "traces/all: in 2 -> attributes/all 2 -> batch 2; traces/web: in 2 -> filter/health 1 -> attributes/web 1 -> batch 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeScenario(t, "assertions", tt.assertions)
			rep, err := Execute(ctx, Request{Config: tt.config, Scenario: path})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Assertions == nil || len(rep.Assertions.Results) != len(tt.want) {
				t.Fatalf("assertions = %+v", rep.Assertions)
			}
			found := tt.message == ""
			for i, r := range rep.Assertions.Results {
				if r.Outcome != tt.want[i] {
					t.Errorf("%s: outcome %s, want %s: %+v", r.Name, r.Outcome, tt.want[i], r.Checks)
				}
				for _, c := range r.Checks {
					found = found || strings.Contains(c.Message, tt.message)
				}
			}
			if !found {
				t.Errorf("no message contains %q: %+v", tt.message, rep.Assertions.Results)
			}
		})
	}
}

func TestTestReportsEachScenario(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	multisignalDir := filepath.Join("..", "..", "..", "testdata", "multisignal")
	failing := writeScenario(t, "failing", "  - {target: {exporter: otlp/backend, signal: traces}, count: {equals: 5}}\n")
	rep, err := Test(ctx, TestRequest{Config: multisignalConfig, Scenarios: []string{
		filepath.Join(multisignalDir, "tests", "scrubbing.yaml"),
		failing,
		filepath.Join(multisignalDir, "scenario.yaml"),
		filepath.Join(multisignalDir, "missing.yaml"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name    string
		outcome assertion.Outcome
		err     string
	}{
		{"scrubbing", assertion.Pass, ""},
		{"failing", assertion.Fail, ""},
		{"accounts", assertion.Error, "no assertions"},
		{"", assertion.Error, "missing.yaml"},
	}
	if rep.Schema != TestSchemaVersion || rep.Outcome != assertion.Error || len(rep.Scenarios) != len(want) {
		t.Fatalf("report = %+v", rep)
	}
	for i, w := range want {
		s := rep.Scenarios[i]
		if s.Name != w.name || s.Outcome != w.outcome || !strings.Contains(s.Error, w.err) || (w.err == "") != (s.Assertions != nil) {
			t.Errorf("scenario %d = %+v, want %+v", i, s, w)
		}
	}
	if rep.Scenarios[0].RunID == "" {
		t.Error("passing scenario lacks its run ID")
	}

	if _, err := Test(ctx, TestRequest{Config: multisignalConfig}); err == nil {
		t.Fatal("a request without scenarios was accepted")
	}
}
