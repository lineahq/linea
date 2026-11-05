package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// A run's inputs come back from its bundle in both modes, with root text,
// labels and references, and never with environment values or the contents of
// captured dependencies. They come back unchanged after a workspace restart.
func TestRunInputsReturnAdmittedSourcesWithoutPrivateValues(t *testing.T) {
	const name, secret = "OTEL_RUN_INPUTS_ENDPOINT", "private-host-value"
	t.Setenv(name, secret)
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: captured, value: '${env:" + name + "}', action: upsert}]}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	workspace, opts := memory.NewData(), Options{Environment: config.EnvironmentPolicy{Host: []string{name}}, Paths: PathPolicy{Roots: []string{dir}}}
	s := openPlatform(t, workspace, opts)
	configRef := saveRevision(t, s, artifact.Configuration, collectorYAML)
	setup := "environment: {OTEL_RUN_INPUTS_UNUSED: declared}\n"
	assertions := "assertions: [{name: one, target: {exporter: otlp, signal: traces}, count: {min: 1}}]\n"
	scenarioYAML := "version: 1\ninputs:\n  - file: input.json\n"

	quick, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Artifact: configRef, Label: "saved.yaml"},
		Quick: &QuickInput{Fixture: Source{Text: string(payload), Label: "traces.json"}, Receiver: "otlp", ClientAddress: "10.1.2.3", Setup: setup, Assertions: assertions}})
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeScenario, Config: Source{Text: collectorYAML},
		Scenario: &ScenarioInput{Scenario: Source{Text: scenarioYAML, Label: "tests/files.yaml"}, Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}

	quickIn, err := s.GetRunInputs(ctx, testCaller, quick.ID)
	if err != nil {
		t.Fatal(err)
	}
	in := quickIn
	if in.Mode != ModeQuick || in.Config.Text != collectorYAML || in.Config.Label != "saved.yaml" || in.Config.Artifact == nil || *in.Config.Artifact != *configRef ||
		in.Config.SHA256 != digest(collectorYAML) || in.Scenario != nil || in.Fixture == nil || in.Fixture.Text != string(payload) || in.Fixture.Label != "traces.json" ||
		in.Fixture.Artifact != nil || in.Setup == nil || in.Setup.Text != setup || in.Assertions == nil || in.Assertions.Text != assertions ||
		in.Receiver != "otlp" || in.ClientAddress != "10.1.2.3" || !slices.Equal(in.RequiredEnvironment, []string{name}) {
		t.Fatalf("quick inputs = %+v", in)
	}
	assertNoSecret(t, in, secret)

	scenarioIn, err := s.GetRunInputs(ctx, testCaller, scenario.ID)
	if err != nil {
		t.Fatal(err)
	}
	in = scenarioIn
	if in.Mode != ModeScenario || in.Config.Label != "" || in.Config.Artifact != nil || in.Fixture != nil || in.Setup != nil || in.Assertions != nil ||
		in.Scenario == nil || in.Scenario.Text != scenarioYAML || in.Scenario.Label != "tests/files.yaml" || !slices.Equal(in.Files, []string{"input.json"}) {
		t.Fatalf("scenario inputs = %+v", in)
	}
	raw := assertNoSecret(t, in, secret)
	if strings.Contains(raw, strings.TrimSpace(string(payload))[:40]) {
		t.Fatalf("captured file contents returned: %s", raw)
	}

	s.Close()
	s = openPlatform(t, workspace, opts)
	for id, want := range map[string]*RunInputs{quick.ID: quickIn, scenario.ID: scenarioIn} {
		if got, err := s.GetRunInputs(ctx, testCaller, id); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("after restart: %+v, %v; want %+v", got, err, want)
		}
	}
	_, err = s.GetRunInputs(ctx, testCaller, "missing")
	wantCode(t, err, CodeNotFound)
}

func assertNoSecret(t *testing.T, in *RunInputs, secret string) string {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("environment value returned: %s", raw)
	}
	return fmt.Sprint(string(raw))
}
