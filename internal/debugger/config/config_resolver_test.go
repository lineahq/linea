package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/registry"
)

func TestResolveValidatesThroughCollectorRegistry(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "..", "testdata", "traces", "config.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := Resolve(context.Background(), path, factories, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := resolved.Config.Service.Pipelines[pipeline.NewID(pipeline.SignalTraces)]
	if p == nil {
		t.Fatal("traces pipeline missing")
	}
	want := []component.ID{
		component.MustNewID("transform"),
		component.MustNewID("filter"),
		component.MustNewID("batch"),
	}
	if !slices.Equal(p.Processors, want) {
		t.Fatalf("processors = %v, want %v", p.Processors, want)
	}
	if _, ok := resolved.Config.Exporters[component.MustNewID("otlp")]; !ok {
		t.Fatal("otlp exporter (deprecated alias of otlp_grpc) not resolved")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("resolution modified the input configuration")
	}
}

func TestResolveRejectsInvalidConfig(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "invalid OTTL",
			yaml: "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {trace_statements: [\"set(\"]}}\nexporters: {nop: {}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n",
			want: "transform",
		},
		{
			name: "invalid YAML",
			yaml: "receivers:\n  otlp: [\n",
			want: "line",
		},
		{
			name: "undefined pipeline reference",
			yaml: "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {nop: {}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [batch], exporters: [nop]}}}\n",
			want: "batch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Resolve(context.Background(), path, factories, nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

// TestResolveDiagnosesUnsupportedComponents checks that every component type
// missing from the distribution is named, with the supported alternatives.
// Each is a warning and runs as a stand-in.
func TestResolveDiagnosesUnsupportedComponents(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `receivers: {kafka: {}, otlp: {protocols: {grpc: {}}}}
processors: {groupbytrace: {}, batch: {}}
exporters: {nop: {}}
service: {pipelines: {traces: {receivers: [kafka, otlp], processors: [groupbytrace, batch], exporters: [nop]}}}
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := Resolve(context.Background(), path, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []Diagnostic{
		{Severity: SeverityWarning, Code: CodeUnsupportedComponent, Component: "receiver/kafka",
			Message: `receiver type "kafka" is not part of the debugger distribution; it runs as a stand-in that receives the scenario's inputs`,
			Hint:    "supported receivers: file_log, filelog, otlp"},
		{Severity: SeverityWarning, Code: CodeUnsupportedComponent, Component: "processor/groupbytrace",
			Message: `processor type "groupbytrace" is not part of the debugger distribution; it runs as a stand-in that passes data through unchanged`,
			Hint:    "supported processors: attributes, batch, cumulative_to_delta, cumulativetodelta, delta_to_cumulative, deltatocumulative, filter, groupbyattrs, k8s_attributes, k8sattributes, memory_limiter, metrics_transform, metricstransform, probabilistic_sampler, redaction, resource, span, tail_sampling, transform; assertions on data that passes this stand-in are inconclusive"},
	}
	if !slices.Equal(resolved.Warnings, want) {
		t.Fatalf("warnings = %+v\nwant %+v", resolved.Warnings, want)
	}
}

func TestResolveUsesScenarioEnvironment(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"exporters: {otlp: {endpoint: \"${env:LINEA_TEST_ENDPOINT}\"}}\n" +
		"service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEA_TEST_ENDPOINT", "from-process:4317")

	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "process environment", env: nil, want: "from-process:4317"},
		{name: "scenario overrides", env: map[string]string{"LINEA_TEST_ENDPOINT": "from-scenario:4317"}, want: "from-scenario:4317"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := Resolve(context.Background(), path, factories, tt.env)
			if err != nil {
				t.Fatal(err)
			}
			cfg := resolved.Config.Exporters[component.MustNewID("otlp")]
			if !strings.Contains(fmt.Sprintf("%+v", cfg), tt.want) {
				t.Fatalf("exporter config does not use %q: %+v", tt.want, cfg)
			}
		})
	}
}
