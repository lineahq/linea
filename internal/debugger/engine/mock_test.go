package engine

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/topology"
)

func receiverScenario(name string) (config, scenario string) {
	dir := filepath.Join("..", "..", "..", "testdata", "receivers", name)
	return filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
}

// mockedScenarios are the receiver scenarios: container logs read by the real
// filelog receiver from mocks.files, spans enriched by the real
// k8sattributes processor from mocks.kubernetes, and span metrics the real
// spanmetrics connector flushes on a simulated clock.
var mockedScenarios = []string{"filelog", "kubernetes", "spanmetrics"}

// TestMockedScenariosPass runs each scenario through the engine: the
// assertions hold without a node's filesystem or a cluster.
func TestMockedScenariosPass(t *testing.T) {
	for _, name := range mockedScenarios {
		t.Run(name, func(t *testing.T) {
			config, scenario := receiverScenario(name)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Test(ctx, TestRequest{Config: config, Scenarios: []string{scenario}})
			if err != nil {
				t.Fatal(err)
			}
			if rep.Outcome != assertion.Pass {
				t.Fatalf("outcome = %s, want pass: %+v", rep.Outcome, rep)
			}
		})
	}
}

// TestMockedNormalizedReportsAreIdentical: filelog stamps observed times from
// the clock and mocked components add attributes in arbitrary order;
// normalized reports are still byte-identical.
func TestMockedNormalizedReportsAreIdentical(t *testing.T) {
	for _, name := range mockedScenarios {
		t.Run(name, func(t *testing.T) {
			config, scenario := receiverScenario(name)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var first []byte
			for i := range 3 {
				rep, err := Execute(ctx, Request{Config: config, Scenario: scenario, Normalize: true})
				if err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				if err := Encode(&buf, rep); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = buf.Bytes()
				} else if !bytes.Equal(buf.Bytes(), first) {
					t.Fatalf("run %d report differs from the first", i+1)
				}
			}
		})
	}
}

// TestMockedScenariosMatchTheReference: the reference Collector runs the
// unmodified components against the same mocks (files, Kubernetes objects,
// clock), receives inputs over loopback OTLP as from their declared client
// addresses, and delivers the same output as the instrumented run.
func TestMockedScenariosMatchTheReference(t *testing.T) {
	for _, name := range mockedScenarios {
		t.Run(name, func(t *testing.T) {
			config, scenario := receiverScenario(name)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{Config: config, Scenario: scenario, Compare: true})
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Comparison.Equal {
				t.Fatalf("comparison: %v", rep.Comparison.Differences)
			}
			if len(rep.Reference.Mocks) == 0 || len(rep.Reference.Outputs) == 0 {
				t.Fatalf("reference: %d mock declarations, %d outputs", len(rep.Reference.Mocks), len(rep.Reference.Outputs))
			}
			if !slices.ContainsFunc(rep.Comparison.Notes, func(n string) bool { return strings.Contains(n, "mocked components") }) {
				t.Fatalf("notes = %v", rep.Comparison.Notes)
			}
		})
	}
}

// TestMockedRunsReachNoInfrastructure checks that the
// filelog and Kubernetes enrichment scenarios run without production
// infrastructure, even when that infrastructure is there. A real file sits
// at the path the receiver's include pattern matches, and the service
// variables point at a listening "cluster"; the runs read only the
// scenario's fixtures and never touch either.
func TestMockedRunsReachNoInfrastructure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	t.Run("filelog", func(t *testing.T) {
		dir := t.TempDir()
		host := filepath.Join(dir, "pods", "shop_a_1", "app", "0.log")
		if err := os.MkdirAll(filepath.Dir(host), 0o700); err != nil {
			t.Fatal(err)
		}
		const decoy = "DECOY: a host file must never be read\n"
		if err := os.WriteFile(host, []byte(decoy), 0o600); err != nil {
			t.Fatal(err)
		}
		pattern := filepath.ToSlash(filepath.Join(dir, "pods")) + "/*/*/*.log"
		config := "receivers:\n  filelog:\n    include: ['" + pattern + "']\n    start_at: beginning\n" +
			"exporters:\n  otlp/backend:\n    endpoint: backend.example.com:4317\n" +
			"service:\n  pipelines:\n    logs:\n      receivers: [filelog]\n      exporters: [otlp/backend]\n"
		scenario := "version: 1\nmocks:\n  files:\n    - path: '" + filepath.ToSlash(host) + "'\n      content: |\n        from the scenario\n"
		// The reference run reads the same mock, never the host file.
		rep, err := Execute(ctx, Request{ConfigYAML: config, ScenarioYAML: scenario, Compare: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range []*runresult.Result{rep.Result, rep.Reference} {
			out := outputText(res)
			if !strings.Contains(out, "from the scenario") || strings.Contains(out, "DECOY") {
				t.Fatalf("%s run exported %s, want only the scenario's file", res.Mode, out)
			}
		}
		if b, err := os.ReadFile(host); err != nil || string(b) != decoy {
			t.Fatalf("host file changed: %q, %v", b, err)
		}
		assertMocked(t, rep, "receiver/filelog")
	})

	t.Run("kubernetes", func(t *testing.T) {
		cluster := listen(t)
		h, p, _ := net.SplitHostPort(cluster.Addr().String())
		t.Setenv("KUBERNETES_SERVICE_HOST", h)
		t.Setenv("KUBERNETES_SERVICE_PORT", p)
		config, scenario := receiverScenario("kubernetes")
		rep, err := Test(ctx, TestRequest{Config: config, Scenarios: []string{scenario}})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Outcome != assertion.Pass {
			t.Fatalf("outcome = %s: %+v", rep.Outcome, rep)
		}
		if n := cluster.conns.Load(); n != 0 {
			t.Fatalf("the cluster at KUBERNETES_SERVICE_HOST received %d connection(s)", n)
		}
		if os.Getenv("KUBERNETES_SERVICE_HOST") != h || os.Getenv("KUBERNETES_SERVICE_PORT") != p {
			t.Fatal("the service variables were not restored")
		}
		run, err := Execute(ctx, Request{Config: config, Scenario: scenario, Compare: true})
		if err != nil {
			t.Fatal(err)
		}
		if n := cluster.conns.Load(); n != 0 {
			t.Fatalf("the reference run connected to the cluster %d time(s)", n)
		}
		assertMocked(t, run, "processor/k8sattributes")
	})
}

func outputText(res *runresult.Result) string {
	var b strings.Builder
	for _, o := range res.Outputs {
		b.Write(o.Data)
	}
	return b.String()
}

// assertMocked checks that the report declares ref as dependency_mocked in
// both the topology and the mock declarations.
func assertMocked(t *testing.T, rep *Report, ref string) {
	t.Helper()
	if c, ok := rep.Topology.Component(ref); !ok || c.Mode != topology.ModeDependencyMocked {
		t.Errorf("topology %s = %+v, want dependency_mocked", ref, c)
	}
	if len(rep.Result.Mocks) != 1 || rep.Result.Mocks[0].Ref != ref || len(rep.Result.Mocks[0].Supplied) == 0 {
		t.Errorf("mocks %+v, want a declaration of what %s was supplied", rep.Result.Mocks, ref)
	}
}

// TestQuickRunAppliesSetup runs the pod-enrichment sample's input alone, as
// the web client's quick mode does, with the scenario's mocks as its setup:
// the span is enriched as in the scenario, and the scenario's assertions hold
// on the quick run. Without the setup nothing is associated.
func TestQuickRunAppliesSetup(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "pod-enrichment")
	cfg := filepath.Join(dir, "config.yaml")
	input, err := os.ReadFile(filepath.Join(dir, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := scenario.Load(filepath.Join(dir, "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	setup, err := yaml.Marshal(map[string]any{"mocks": s.Mocks})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tt := range []struct {
		name string
		req  Request
		want assertion.Outcome
	}{
		{"with setup", Request{Config: cfg, FixtureJSON: string(input), ClientAddress: "10.42.0.17", SetupYAML: string(setup)}, assertion.Pass},
		{"without setup", Request{Config: cfg, FixtureJSON: string(input), ClientAddress: "10.42.0.17"}, assertion.Fail},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := Execute(ctx, tt.req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := assertion.Evaluate(s.Assertions, rep.Topology, rep.Result)
			if err != nil {
				t.Fatal(err)
			}
			if got.Outcome != tt.want {
				t.Fatalf("outcome = %s, want %s: %+v", got.Outcome, tt.want, got)
			}
			if (rep.Provenance.Setup != nil) != (tt.req.SetupYAML != "") {
				t.Fatalf("provenance setup = %v", rep.Provenance.Setup)
			}
		})
	}
}

// TestQuickRunRejectsScenarioOnlySetup: a setup with inputs or assertions is
// refused, and client_address and setup_yaml need a fixture.
func TestQuickRunRejectsScenarioOnlySetup(t *testing.T) {
	cfg := filepath.Join("..", "..", "..", "samples", "pod-enrichment", "config.yaml")
	const fx = `{"resourceSpans":[]}`
	for _, req := range []Request{
		{Config: cfg, FixtureJSON: fx, SetupYAML: "inputs: [{receiver: otlp, file: a.json}]\n"},
		{Config: cfg, ScenarioYAML: "version: 1\n", SetupYAML: "environment: {A: b}\n"},
		{Config: cfg, FixtureJSON: fx, ClientAddress: "pod-a"},
	} {
		_, err := Execute(context.Background(), req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%+v: err = %v, want ErrInvalidRequest", req, err)
		}
	}
}

// TestSpanMetricsOutputIsAttributedToTheConnector: the spanmetrics connector
// emits its metrics on its (simulated) timer, outside any consume call. The
// spans it consumed are deferred rather than lost, and each span's path ends
// there as aggregated, while it is still delivered to the trace exporter.
func TestSpanMetricsOutputIsAttributedToTheConnector(t *testing.T) {
	config, scenario := receiverScenario("spanmetrics")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: config, Scenario: scenario})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Result.Mocks) != 1 || len(rep.Result.Mocks[0].Supplied) != 3 {
		t.Fatalf("mocks = %+v, want the clock start and two flushes", rep.Result.Mocks)
	}
	for _, s := range rep.Stages {
		if s.Emission != analysis.EmissionAsynchronous {
			t.Errorf("%s %s stage emission = %s", s.Component, s.Signal, s.Emission)
		}
		for _, in := range s.Inputs {
			if in.Outcome != analysis.OutcomeDeferred {
				t.Errorf("%s %s input %d outcome = %s", s.Component, s.Signal, in.Seq, in.Outcome)
			}
		}
	}
	if len(rep.Lineage) != 3 {
		t.Fatalf("lineage has %d records, want the 3 spans", len(rep.Lineage))
	}
	for _, rl := range rep.Lineage {
		var ends []analysis.End
		for _, s := range rl.Stops {
			ends = append(ends, s.End)
		}
		if len(rl.Deliveries) != 1 || len(ends) != 1 || ends[0] != analysis.EndAggregated {
			t.Errorf("%s: deliveries %+v, stops %v", rl.Name, rl.Deliveries, ends)
		}
	}
}
