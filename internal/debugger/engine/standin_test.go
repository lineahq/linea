package engine

import (
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

func TestLineageEndsAtStandInConnectorsSaySo(t *testing.T) {
	g := &topology.Graph{Components: []topology.Component{
		{Ref: "connector/routing", Kind: topology.KindConnector, ID: "routing", Mode: topology.ModeStandIn},
		{Ref: "connector/forward", Kind: topology.KindConnector, ID: "forward", Mode: topology.ModeReal},
		{Ref: "processor/routing", Kind: topology.KindProcessor, ID: "routing", Mode: topology.ModeReal},
	}}
	lineage := []analysis.RecordLineage{{Hops: []analysis.Hop{
		{Component: "routing", End: analysis.EndNotRouted, Note: "no route delivered this record"},
		{Component: "routing"},
		{Component: "forward", End: analysis.EndNotRouted, Note: "no route delivered this record"},
	}}}
	noteStandInConnectors(lineage, g)
	hops := lineage[0].Hops
	tests := []struct {
		name    string
		hop     analysis.Hop
		standIn bool
	}{
		{"end at a stand-in", hops[0], true},
		{"no end", hops[1], false},
		{"end at a real connector", hops[2], false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.Contains(tt.hop.Note, standInConnectorNote); got != tt.standIn {
				t.Fatalf("note %q: stand-in note = %v, want %v", tt.hop.Note, got, tt.standIn)
			}
		})
	}
	if !strings.HasPrefix(hops[0].Note, "no route delivered this record; ") {
		t.Fatalf("note %q lost the original reason", hops[0].Note)
	}
}
