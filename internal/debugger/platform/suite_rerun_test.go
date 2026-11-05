package platform

import (
	"context"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
)

const rerunConfig = "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"

// rerunService prepares requests for real, so runs capture bundles, and
// executes them with a fake that records each request and reports one
// assertion per scenario: "pass" scenarios pass, others fail.
func rerunService(t *testing.T, env config.EnvironmentPolicy) (*Service, func() []engine.Request) {
	t.Helper()
	var mu sync.Mutex
	var seen []engine.Request
	s := New(Options{Environment: env, Paths: PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		mu.Lock()
		seen = append(seen, req)
		mu.Unlock()
		outcome := assertion.Fail
		if strings.Contains(req.ScenarioYAML, "name: pass") {
			outcome = assertion.Pass
		}
		rep := report(req, runresult.StatusCompleted)
		rep.Runtime = &engine.ReportRuntime{Kind: engine.RuntimeBuiltIn, CollectorVersion: "0.162.0", Verified: true}
		rep.Assertions = &assertion.Report{Outcome: outcome, Results: []assertion.Result{{Name: "arrives", Target: "exporter otlp", Outcome: outcome}}}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	return s, func() []engine.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]engine.Request(nil), seen...)
	}
}

// inputDir is a directory holding input.json, a recorded trace payload.
func inputDir(t *testing.T) string {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRerunSuiteAdmitsTheMembersRecordedInputs(t *testing.T) {
	dir := inputDir(t)
	input := filepath.Join(dir, "input.json")
	ctx := context.Background()
	s, seen := rerunService(t, config.EnvironmentPolicy{})
	member := func(name string) ScenarioInput {
		return ScenarioInput{Scenario: Source{Text: "version: 1\nname: " + name + "\ninputs:\n  - file: input.json\n", Label: "tests/" + name + ".yaml"}, Dir: dir}
	}
	first, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Text: rerunConfig, Label: "collector.yaml"}, Members: []ScenarioInput{member("pass"), member("broken")}})
	if err != nil {
		t.Fatal(err)
	}
	waitSuite(t, s, first)
	// The rerun reads nothing from the host: the input file is gone.
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}

	again, err := s.RerunSuite(ctx, testCaller, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	status := waitSuite(t, s, again)
	if again.ID == first.ID || again.Config != "collector.yaml" || len(status.Members) != 2 || status.Outcome != assertion.Fail {
		t.Fatalf("rerun = %+v, status = %+v", again, status)
	}
	for i, m := range status.Members {
		if m.Label != first.Members[i].Label || m.RunID == "" || m.RunID == first.Members[i].RunID {
			t.Fatalf("member %d = %+v, first = %+v", i, m, first.Members[i])
		}
	}
	reqs := seen()
	if len(reqs) != 4 {
		t.Fatalf("executed %d requests", len(reqs))
	}
	for _, req := range reqs[2:] {
		if req.ConfigYAML != rerunConfig || req.ScenarioDir != "" || strings.Contains(req.ScenarioYAML, "file: input.json") {
			t.Fatalf("rerun request = config %q dir %q scenario %q", req.ConfigYAML, req.ScenarioDir, req.ScenarioYAML)
		}
	}

	raw, err := s.GetSuiteJUnit(ctx, testCaller, again.ID)
	if err != nil {
		t.Fatal(err)
	}
	var junit engine.JUnitSuites
	if err := xml.Unmarshal(raw, &junit); err != nil {
		t.Fatalf("junit %s: %v", raw, err)
	}
	if junit.Tests != 2 || junit.Failures != 1 || junit.Errors != 0 || len(junit.Suites) != 2 ||
		junit.Suites[0].Name != "tests/pass.yaml" || junit.Suites[0].Cases[0].Name != "arrives" || junit.Suites[0].Cases[0].Failure != nil ||
		junit.Suites[1].Cases[0].Failure == nil {
		t.Fatalf("junit = %s", raw)
	}

	_, err = s.RerunSuite(ctx, testCaller, "suite-unknown")
	wantCode(t, err, CodeNotFound)
	_, err = s.GetSuiteJUnit(ctx, testCaller, "suite-unknown")
	wantCode(t, err, CodeNotFound)
}

func TestRerunSuiteRefusesMembersWithoutRecordedInputs(t *testing.T) {
	const name = "OTEL_RERUN_ENDPOINT"
	t.Setenv(name, "private-host-value")
	ctx := context.Background()
	s, seen := rerunService(t, config.EnvironmentPolicy{Host: []string{name}})
	scenario := ScenarioInput{Scenario: Source{Text: "version: 1\nname: pass\ninputs:\n  - file: input.json\n", Label: "inline.yaml"}, Dir: inputDir(t)}

	missing, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Text: rerunConfig},
		Members: []ScenarioInput{scenario, {Scenario: Source{Path: filepath.Join(t.TempDir(), "missing.yaml")}}}})
	if err != nil {
		t.Fatal(err)
	}
	waitSuite(t, s, missing)
	_, err = s.RerunSuite(ctx, testCaller, missing.ID)
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), "missing.yaml was never admitted") {
		t.Fatalf("error = %v", err)
	}

	withEnv := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: captured, value: '${env:" + name + "}', action: upsert}]}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	env, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Text: withEnv}, Members: []ScenarioInput{scenario}})
	if err != nil {
		t.Fatal(err)
	}
	waitSuite(t, s, env)
	_, err = s.RerunSuite(ctx, testCaller, env.ID)
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "private-host-value") {
		t.Fatalf("error = %v", err)
	}
	if n := len(seen()); n != 2 {
		t.Fatalf("a refused rerun executed: %d requests", n)
	}
}
