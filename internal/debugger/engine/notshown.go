package engine

import "github.com/lineahq/linea/internal/debugger/topology"

// Codes of topology.Gap: behaviors of the real component a run does not show.
const (
	gapExportOutcome     = "export_outcome"
	gapHostMemory        = "host_memory"
	gapReceiverIntake    = "receiver_intake"
	gapStandInProcessing = "stand_in_processing"
	gapStandInRouting    = "stand_in_routing"
)

var gapDetails = map[string]string{
	gapExportOutcome: "Every export succeeds in a run: export errors, retries, the sending queue, " +
		"backpressure on the pipeline and failover to another exporter are not shown.",
	gapHostMemory: "memory_limiter measures the memory of the process that runs the debugger " +
		"(the CLI, a worker or the server), not the deployed Collector's, so whether it refuses data " +
		"here says nothing about production.",
	gapReceiverIntake: "Fixtures are injected where the receiver delivers data: its network intake, " +
		"decoding, authentication and rejections are not shown.",
	gapStandInProcessing: "The type is not in the distribution and a stand-in passes data through " +
		"unchanged: what the real processor does to the data is not shown.",
	gapStandInRouting: "The type is not in the distribution and a stand-in forwards data of its own " +
		"signal to every pipeline it feeds and emits nothing into other signals: the real connector's " +
		"routing is not shown.",
}

// markNotShown records on every component of g what its real counterpart
// does in a deployment that this run does not show, from its kind, type and
// execution mode.
func markNotShown(g *topology.Graph) {
	for i := range g.Components {
		c := &g.Components[i]
		c.NotShown = nil
		for _, code := range gapsOf(*c) {
			c.NotShown = append(c.NotShown, topology.Gap{Code: code, Detail: gapDetails[code]})
		}
	}
}

func gapsOf(c topology.Component) []string {
	switch c.Kind {
	case topology.KindReceiver:
		if c.Mode == topology.ModeSimulated || c.Mode == topology.ModeStandIn {
			return []string{gapReceiverIntake}
		}
	case topology.KindProcessor:
		switch {
		case c.Mode == topology.ModeStandIn:
			return []string{gapStandInProcessing}
		case c.Type == "memory_limiter":
			return []string{gapHostMemory}
		}
	case topology.KindExporter:
		if c.Mode == topology.ModeCaptured || c.Mode == topology.ModeStandIn {
			return []string{gapExportOutcome}
		}
	case topology.KindConnector:
		if c.Mode == topology.ModeStandIn {
			return []string{gapStandInRouting}
		}
	}
	return nil
}
