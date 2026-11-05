package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestEnvironmentPolicyAppliesToQuickAndScenarioRuns(t *testing.T) {
	const name = "OTEL_PLATFORM_BOUND"
	t.Setenv(name, "private-host-value")
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"quick", "scenario"} {
		for _, binding := range []string{"missing", "declared", "default", "allowlisted"} {
			t.Run(mode+"/"+binding, func(t *testing.T) {
				uri := "${env:" + name + "}"
				if binding == "default" {
					uri = "${env:" + name + ":-declared-default}"
				}
				collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: captured, value: '" + uri + "', action: upsert}]}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
				req := engine.Request{ConfigYAML: collectorYAML, FixtureJSON: string(payload), Compare: true}
				setup := ""
				if binding == "declared" {
					setup = "environment: {" + name + ": declared-value}\n"
				}
				if mode == "quick" {
					req.SetupYAML = setup
				} else {
					req.FixtureJSON = ""
					req.ScenarioYAML = "version: 1\n" + setup + fmt.Sprintf("inputs: [{data: %q}]\n", string(payload))
				}
				policy := config.EnvironmentPolicy{}
				if binding == "allowlisted" {
					policy.Host = []string{name}
				}
				s := New(Options{Environment: policy})
				t.Cleanup(s.Close)
				if len(policy.Host) != 0 {
					policy.Host[0] = "mutated"
				}
				r, err := s.Submit(req)
				if err != nil {
					t.Fatal(err)
				}
				view := waitRun(t, r)
				if view.State != StateDone || !view.Report.Comparison.Equal {
					t.Fatalf("bound run: %+v", view)
				}
				want, envMode := "declared-value", "declared_only"
				if binding == "missing" {
					// An unbound name stands in, never reading the host.
					want = "linea-stand-in-"
				}
				if binding == "default" {
					want = "declared-default"
				}
				if binding == "allowlisted" {
					want, envMode = "private-host-value", "allowlisted_host"
				}
				if view.Bundle.Environment != envMode {
					t.Fatalf("policy not disclosed: %+v", view.Bundle)
				}
				if len(view.Report.Result.Outputs) == 0 {
					t.Fatal("missing output")
				}
				for _, output := range view.Report.Result.Outputs {
					if !bytes.Contains(output.Data, []byte(want)) || binding != "allowlisted" && bytes.Contains(output.Data, []byte("private-host-value")) {
						t.Fatal("wrong environment binding", string(output.Data))
					}
				}
			})
		}
	}
}

func TestPreparedHostBindingsCannotBypassServicePolicyAfterRestart(t *testing.T) {
	const name = "OTEL_PLATFORM_RESTART_BOUND"
	t.Setenv(name, "original:4317")
	collectorYAML := strings.Replace(bundleConfig, "example.invalid:4317", "'${env:"+name+"}'", 1)
	req := engine.Request{ConfigYAML: collectorYAML, FixtureJSON: `{"resourceSpans":[]}`}
	data := memory.NewData()
	s := openPlatform(t, data, Options{Environment: config.EnvironmentPolicy{LegacyHost: true}})
	r := submit(t, s, req)
	if view := waitRun(t, r); view.Bundle.Environment != "legacy_host" {
		t.Fatal("legacy policy hidden")
	}
	s.Close()
	// History inspection does not re-authorize imports or read the new host value.
	t.Setenv(name, "changed:4317")
	s = openPlatform(t, data, Options{})
	raw, err := s.GetRunBundle(context.Background(), testCaller, r.ID())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.DecodeBundle(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(prepared); !errors.Is(err, config.ErrEnvironmentBinding) || len(s.List()) != 1 {
		t.Fatal("prepared request bypassed admitting policy", err)
	}
	allowed := New(Options{Environment: config.EnvironmentPolicy{Host: []string{name}}})
	t.Cleanup(allowed.Close)
	retry := submit(t, allowed, prepared)
	if view := waitRun(t, retry); view.State != StateDone {
		t.Fatal("authorized captured replay failed", view.Error)
	}
}
