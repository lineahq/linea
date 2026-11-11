package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/topology"
)

const notShownConfig = `
receivers:
  otlp:
    protocols:
      grpc:
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: 512
  geoip:
    providers:
      maxmind:
        database_path: /tmp/geo.mmdb
connectors:
  servicegraph:
exporters:
  otlp/backend:
    endpoint: backend:4317
  debug:
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, geoip]
      exporters: [otlp/backend, debug, servicegraph]
    metrics:
      receivers: [servicegraph]
      exporters: [debug]
`

// TestTopologyListsWhatARunDoesNotShow: each component says which behaviors
// of the real component a run does not show: export outcomes of captured
// exporters, the host's memory for memory_limiter, receiver intake, and the
// processing and routing of stand-ins. Components that run as in a
// deployment list nothing.
func TestTopologyListsWhatARunDoesNotShow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ins, err := InspectWithPolicy(ctx, InspectRequest{ConfigYAML: notShownConfig}, Policy{Paths: hostfs.Policy{Unrestricted: true}})
	if err != nil || !ins.Valid {
		t.Fatalf("inspection %+v, %v", ins, err)
	}
	for ref, want := range map[string][]string{
		"receiver/otlp":            {gapReceiverIntake},
		"processor/memory_limiter": {gapHostMemory},
		"processor/geoip":          {gapStandInProcessing},
		"connector/servicegraph":   {gapStandInRouting},
		"exporter/otlp/backend":    {gapExportOutcome},
		"exporter/debug":           nil,
	} {
		c, ok := ins.Topology.Component(ref)
		if !ok {
			t.Errorf("%s not in the topology", ref)
			continue
		}
		var codes []string
		for _, g := range c.NotShown {
			if g.Detail == "" {
				t.Errorf("%s: %s has no detail", ref, g.Code)
			}
			codes = append(codes, g.Code)
		}
		if !slices.Equal(codes, want) {
			t.Errorf("%s (%s): not shown %v, want %v", ref, c.Mode, codes, want)
		}
	}
}

// TestNotShownFollowsTheMode: a component that does not run as itself
// lists nothing the mode does not explain; a stand-in memory_limiter is a
// stand-in, not a measurement of the host.
func TestNotShownFollowsTheMode(t *testing.T) {
	g := topology.Graph{Components: []topology.Component{
		{Ref: "exporter/otlp", Kind: topology.KindExporter, Type: "otlp", Mode: topology.ModeDependencyMocked},
		{Ref: "processor/memory_limiter", Kind: topology.KindProcessor, Type: "memory_limiter", Mode: topology.ModeStandIn},
		{Ref: "connector/forward", Kind: topology.KindConnector, Type: "forward", Mode: topology.ModeReal},
		{Ref: "extension/health_check", Kind: topology.KindExtension, Type: "health_check", Mode: topology.ModeSimulated},
	}}
	markNotShown(&g)
	want := map[string][]string{"processor/memory_limiter": {gapStandInProcessing}}
	for _, c := range g.Components {
		var codes []string
		for _, gap := range c.NotShown {
			codes = append(codes, gap.Code)
		}
		if !slices.Equal(codes, want[c.Ref]) {
			t.Errorf("%s: %v, want %v", c.Ref, codes, want[c.Ref])
		}
	}
}
