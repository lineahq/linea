package mcp

import "github.com/lineahq/linea/internal/debugger/platform"

// serverInstructions is fixed text: it never carries configuration,
// paths or other request content.
const serverInstructions = `This server debugs OpenTelemetry Collector configurations by running them in an isolated, instrumented Collector.

Send a configuration as inline text or a saved revision; the server reads no host file. a scenario or setup_yaml adds inputs, environment values and mocks.

The server never refuses a configuration because it cannot reach what it references. A ${env:...} or ${file:...} reference it cannot read resolves to a listed stand-in string; supply the real value in a scenario's environment: or mocks.files. A component type the debugger does not include runs as a stand-in (mode stand_in): processors pass data through, connectors forward same-signal data, receivers and exporters do nothing. Results name stand-in components and references in their structured content, and the text line counts them.

An assertion on data that passed a stand-in is inconclusive. An inconclusive assertion is never a pass, and an OTTL preview is never a test: verify a change with a quick or scenario run that has assertions.`

// standIns names what ran in place of the real thing in a
// structured result; the text line only counts them.
type standIns struct {
	// StandInComponents are the component references (kind/id) that ran as
	// stand-ins because their type is not in the distribution.
	StandInComponents []string `json:"stand_in_components,omitempty"`
	// References are the configuration's ${scheme:target} references with
	// where each value came from; source stand_in marks an unavailable one.
	References []platform.Reference `json:"references,omitempty"`
}

func standInsOf(g *platform.Graph, refs []platform.Reference) standIns {
	out := standIns{References: refs}
	if g != nil {
		for _, c := range g.Components {
			if c.Mode == platform.ModeStandIn {
				out.StandInComponents = append(out.StandInComponents, c.Ref)
			}
		}
	}
	return out
}

// facts counts the stand-ins for the text line.
func (s standIns) facts() string {
	text := ""
	if n := len(s.StandInComponents); n > 0 {
		text += "; " + plural(n, "stand-in component")
	}
	n := 0
	for _, r := range s.References {
		if r.Source == platform.ReferenceStandIn {
			n++
		}
	}
	if n > 0 {
		text += "; " + plural(n, "stand-in reference")
	}
	return text
}
