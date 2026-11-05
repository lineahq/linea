package runtime

import (
	"strings"
	"sync"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// endpointNote says how a component with simulated endpoints runs.
const endpointNote = "its client endpoints are loopback OTLP sinks: "

// endpointCapture records what a run's simulated endpoints receive: an
// endpoint.received event per export, and the telemetry while the
// evidence budget keeps it, as exporter capture does.
type endpointCapture struct {
	rec    *instrument.Recorder
	budget *instrument.Budget

	mu       sync.Mutex
	received map[mock.EndpointRef][]captured
}

func newEndpointCapture(rec *instrument.Recorder, budget *instrument.Budget) *endpointCapture {
	return &endpointCapture{rec: rec, budget: budget, received: map[mock.EndpointRef][]captured{}}
}

func (c *endpointCapture) EndpointReceived(e mock.EndpointRef, p telemetry.Payload) {
	c.rec.RecordTelemetry(instrument.Event{
		Kind:          instrument.KindEndpointReceived,
		Component:     e.ID.String(),
		ComponentKind: string(e.Kind),
		Endpoint:      e.Path,
		Mode:          instrument.ModeCaptured,
		Source:        instrument.SourceEndpointSink,
	}, p)
	entry := keep(c.budget, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.received[e] = append(c.received[e], entry)
}

// outputs are the endpoint outputs by component, endpoint and signal, and
// the names ("<component>:<endpoint>/<signal>") of those whose data lacks
// telemetry the budget did not keep.
func (c *endpointCapture) outputs() ([]runresult.EndpointOutput, []string, error) {
	c.mu.Lock()
	byName := make(map[string][]captured, len(c.received))
	refs := map[string]mock.EndpointRef{}
	for ref, batches := range c.received {
		name := ref.Ref() + ":" + ref.Path
		byName[name] = append([]captured(nil), batches...)
		refs[name] = ref
	}
	c.mu.Unlock()
	outs, incomplete, err := buildOutputs(byName)
	if err != nil {
		return nil, nil, err
	}
	out := make([]runresult.EndpointOutput, len(outs))
	for i, o := range outs {
		ref := refs[o.Exporter]
		out[i] = runresult.EndpointOutput{Component: ref.Ref(), Endpoint: ref.Path, Signal: o.Signal, Batches: o.Batches, Counts: o.Counts, Data: o.Data}
	}
	return out, incomplete, nil
}

// customModules are the module maps of the kinds whose custom components
// run for real with their client endpoints simulated: receivers
// and exporters are always substituted.
func customModules(factories otelcol.Factories, kind topology.Kind) map[component.Type]string {
	switch kind {
	case topology.KindProcessor:
		return factories.ProcessorModules
	case topology.KindConnector:
		return factories.ConnectorModules
	case topology.KindExtension:
		return factories.ExtensionModules
	}
	return nil
}

// simulatedClients are the client endpoints of a custom component that a
// run simulates, by their paths in its configuration.
func simulatedClients(cfg *otelcol.Config, factories otelcol.Factories, kind topology.Kind, id component.ID) []string {
	if !isCustom(customModules(factories, kind), id.Type()) {
		return nil
	}
	var c component.Config
	switch kind {
	case topology.KindProcessor:
		c = cfg.Processors[id]
	case topology.KindConnector:
		c = cfg.Connectors[id]
	case topology.KindExtension:
		c = cfg.Extensions[id]
	}
	var paths []string
	for _, cl := range mock.Clients(c) {
		paths = append(paths, cl.Path)
	}
	return paths
}

// withEndpointNote adds the simulated endpoints of a custom component to
// its execution note and makes it dependency_mocked.
func withEndpointNote(mode topology.Mode, note string, paths []string) (topology.Mode, string) {
	if len(paths) == 0 {
		return mode, note
	}
	if note != "" {
		note += "; "
	}
	return topology.ModeDependencyMocked, note + endpointNote + strings.Join(paths, ", ")
}
