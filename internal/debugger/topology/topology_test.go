package topology_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"go.opentelemetry.io/collector/component"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

func multisignalGraph(t *testing.T) topology.Graph {
	t.Helper()
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(context.Background(), filepath.Join("..", "..", "..", "testdata", "multisignal", "config.yaml"), factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	mode := func(kind topology.Kind, id component.ID) (topology.Mode, string) {
		if kind == topology.KindExporter && id.Type().String() == "otlp" {
			return topology.ModeCaptured, "captured"
		}
		return topology.ModeReal, ""
	}
	return topology.Build(resolved.Config, mode)
}

func TestBuildDescribesPipelinesInOrder(t *testing.T) {
	g := multisignalGraph(t)
	if len(g.Pipelines) != 2 || g.Pipelines[0].ID != "logs" || g.Pipelines[1].ID != "traces" {
		t.Fatalf("pipelines = %+v", g.Pipelines)
	}
	logs := g.Pipelines[0]
	want := []string{"processor/resource", "processor/attributes/scrub", "processor/filter/health", "processor/transform/logs", "processor/batch"}
	if !slices.Equal(logs.Processors, want) {
		t.Fatalf("logs processors = %v, want %v", logs.Processors, want)
	}
	if !slices.Equal(logs.Exporters, []string{"exporter/otlp/backend", "exporter/debug"}) {
		t.Fatalf("logs exporters = %v", logs.Exporters)
	}
}

func TestBuildMarksSharedComponents(t *testing.T) {
	g := multisignalGraph(t)
	tests := []struct {
		ref    string
		shared bool
		mode   topology.Mode
	}{
		{"receiver/otlp", true, topology.ModeReal},
		{"processor/resource", true, topology.ModeReal},
		{"processor/transform/logs", false, topology.ModeReal},
		{"exporter/otlp/backend", true, topology.ModeCaptured},
		{"exporter/debug", false, topology.ModeReal},
	}
	for _, tt := range tests {
		c, ok := g.Component(tt.ref)
		if !ok {
			t.Fatalf("%s missing from %+v", tt.ref, g.Components)
		}
		if c.Shared() != tt.shared || c.Mode != tt.mode {
			t.Errorf("%s: shared=%v mode=%s, want %v %s", tt.ref, c.Shared(), c.Mode, tt.shared, tt.mode)
		}
	}
	if len(g.Unused) != 0 {
		t.Errorf("unused = %v", g.Unused)
	}
}

func TestBuildConnectsStagesWithinEachPipeline(t *testing.T) {
	g := multisignalGraph(t)
	var traces []topology.Edge
	for _, e := range g.Edges {
		if e.Pipeline == "traces" {
			traces = append(traces, e)
		}
	}
	want := []topology.Edge{
		{"traces", "traces", "receiver/otlp", "processor/resource"},
		{"traces", "traces", "processor/resource", "processor/attributes/scrub"},
		{"traces", "traces", "processor/attributes/scrub", "processor/filter/health"},
		{"traces", "traces", "processor/filter/health", "processor/batch"},
		{"traces", "traces", "processor/batch", "exporter/otlp/backend"},
	}
	if !slices.Equal(traces, want) {
		t.Fatalf("traces edges = %+v\nwant %+v", traces, want)
	}
}

func TestMarkProvidersNamesTheDrivingProvider(t *testing.T) {
	g := multisignalGraph(t)
	g.MarkProviders(map[string]string{"processor/batch": "batcher", "receiver/missing": "ghost"})
	for _, c := range g.Components {
		want := ""
		if c.Ref == "processor/batch" {
			want = "batcher"
		}
		if c.Provider != want {
			t.Errorf("%s: provider %q, want %q", c.Ref, c.Provider, want)
		}
	}
}
