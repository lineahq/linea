package engine

import (
	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// standInConnectorNote is appended to a lineage end at a stand-in connector:
// the real connector's routing was not run, so the end describes the stand-in.
const standInConnectorNote = "this connector is a stand-in (its type is not in the distribution): it forwards data of its own signal and emits nothing into other signals, so this end does not show the real connector's routing"

// noteStandInConnectors marks lineage ends at stand-in connectors.
func noteStandInConnectors(lineage []analysis.RecordLineage, g *topology.Graph) {
	if g == nil {
		return
	}
	for i := range lineage {
		for j := range lineage[i].Hops {
			h := &lineage[i].Hops[j]
			if h.End == "" || h.Component == "" {
				continue
			}
			c, ok := g.Component(string(topology.KindConnector) + "/" + h.Component)
			if !ok || c.Mode != topology.ModeStandIn {
				continue
			}
			if h.Note == "" {
				h.Note = standInConnectorNote
			} else {
				h.Note += "; " + standInConnectorNote
			}
		}
	}
}
