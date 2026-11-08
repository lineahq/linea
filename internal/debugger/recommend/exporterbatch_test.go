package recommend

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// withExporters gives each pipeline of g the exporter refs listed for it.
func withExporters(g *topology.Graph, exporters map[string][]string) *topology.Graph {
	for i, p := range g.Pipelines {
		g.Pipelines[i].Exporters = exporters[p.ID]
	}
	return g
}

// TestBatchBeforeExporterBatching: the rule fires only when every exporter
// of the pipeline batches in its sending queue and a processor batches
// before it.
func TestBatchBeforeExporterBatching(t *testing.T) {
	otlp := ExporterBatch{Exporter: "otlp_grpc", MinSize: 8192, FlushTimeout: "200ms", Sizer: "items"}
	for _, tt := range []struct {
		name       string
		processors []string
		exporters  []string
		batching   []*analysis.Batching
		want       int
	}{
		{"batch before a batching exporter", []string{"batch"}, []string{"exporter/otlp_grpc"}, nil, 1},
		{"queue_batch before a batching exporter", []string{"queue_batch"}, []string{"exporter/otlp_grpc"}, nil, 1},
		{"no batching processor", []string{"transform"}, []string{"exporter/otlp_grpc"}, nil, 0},
		{"an exporter without batching", []string{"batch"}, []string{"exporter/otlp_grpc", "exporter/debug"}, nil, 0},
		{"a connector", []string{"batch"}, []string{"exporter/otlp_grpc", "connector/routing"}, nil, 0},
		{"unbatched queue_batch", []string{"queue_batch"}, []string{"exporter/otlp_grpc"},
			[]*analysis.Batching{{Processor: "queue_batch", Pipeline: "logs", Settings: analysis.BatchSettings{Type: analysis.TypeQueueBatch, Unbatched: true}}}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := withExporters(graph(map[string][]string{"logs": tt.processors}), map[string][]string{"logs": tt.exporters})
			recs := batchBeforeExporterBatching(Input{Topology: g, Batching: tt.batching, ExporterBatches: []ExporterBatch{otlp}})
			if len(recs) != tt.want {
				t.Fatalf("recs = %+v", recs)
			}
		})
	}

	g := withExporters(graph(map[string][]string{"logs": {"batch"}}), map[string][]string{"logs": {"exporter/otlp_grpc"}})
	b := &analysis.Batching{Processor: "batch", Pipeline: "logs", Settings: analysis.BatchSettings{MetadataKeys: []string{"tenant"}}}
	recs := batchBeforeExporterBatching(Input{Topology: g, Batching: []*analysis.Batching{b}, ExporterBatches: []ExporterBatch{otlp}})
	if len(recs) != 1 {
		t.Fatalf("recs = %+v", recs)
	}
	r := recs[0]
	if strings.Join(r.Components, ",") != "processor/batch,exporter/otlp_grpc" || r.Change != nil {
		t.Fatalf("rec = %+v", r)
	}
	containsAll(t, r.Summary, "batches in processor batch and again in the sending queue of exporter otlp_grpc", "the exporter regroups what the processor sent and adds its own wait",
		"sending_queue::batch::min_size 8192, flush_timeout 200ms, sizer items", "it is the processor that can go")
	containsAll(t, strings.Join(r.Coverage, " "), "before its sending queue", "would mix the metadata values")
}
