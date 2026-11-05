// Package topology describes a resolved Collector configuration as a static
// graph: pipelines, the components they use, and how each component executes
// in the debugger. It needs no telemetry.
package topology

import (
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pipeline"
)

// Mode is a component execution mode.
type Mode string

const (
	ModeReal             Mode = "real"
	ModeDependencyMocked Mode = "dependency_mocked"
	ModeSimulated        Mode = "simulated"
	ModeCaptured         Mode = "captured"
	ModeUnsupported      Mode = "unsupported"
	// ModeStandIn: the type is not in the distribution; a stand-in runs in
	// its place.
	ModeStandIn Mode = "stand_in"
)

// Kind is the role of a component.
type Kind string

const (
	KindReceiver  Kind = "receiver"
	KindProcessor Kind = "processor"
	KindExporter  Kind = "exporter"
	KindConnector Kind = "connector"
	KindExtension Kind = "extension"
)

// ModeFunc decides how a component executes. Note explains modes other than
// real.
type ModeFunc func(kind Kind, id component.ID) (mode Mode, note string)

// Component is one configured component in use by the service.
type Component struct {
	// Ref is "<kind>/<id>", unique across kinds (a connector and an exporter
	// may share an ID).
	Ref  string `json:"ref"`
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
	Type string `json:"type"`
	Mode Mode   `json:"mode"`
	Note string `json:"note,omitempty"`
	// Pipelines lists the pipelines using the component. A processor used
	// in several pipelines runs as one instance per pipeline.
	Pipelines []string `json:"pipelines,omitempty"`
	// StandInValues names the references ("scheme:target") whose stand-in
	// strings the component's settings use in place of their values.
	StandInValues []string `json:"stand_in_values,omitempty"`
	// Provider names the module provider that drives the component in a
	// run: its scenario settings are mocks.providers.<Provider>.
	Provider string `json:"provider,omitempty"`
}

// Shared reports whether several pipelines use the component.
func (c Component) Shared() bool { return len(c.Pipelines) > 1 }

// Pipeline is one service pipeline with its components in order.
type Pipeline struct {
	ID         string   `json:"id"`
	Signal     string   `json:"signal"`
	Receivers  []string `json:"receivers"`
	Processors []string `json:"processors"`
	Exporters  []string `json:"exporters"`
}

// Edge is a data path between two components of one pipeline. Refs name
// Component.Ref values.
type Edge struct {
	Pipeline string `json:"pipeline"`
	Signal   string `json:"signal"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// Graph is the static topology of a configuration. Configured components
// that no pipeline uses are not built by the Collector and are listed as
// unused.
type Graph struct {
	Pipelines  []Pipeline  `json:"pipelines"`
	Components []Component `json:"components"`
	Edges      []Edge      `json:"edges"`
	Unused     []string    `json:"unused,omitempty"`
}

// Component returns the component with the given ref.
func (g Graph) Component(ref string) (Component, bool) {
	for _, c := range g.Components {
		if c.Ref == ref {
			return c, true
		}
	}
	return Component{}, false
}

// MarkStandInValues records, per component ref, the references whose
// stand-in strings its settings use. Refs the graph lacks are ignored.
func (g *Graph) MarkStandInValues(byRef map[string][]string) {
	for i := range g.Components {
		if refs := byRef[g.Components[i].Ref]; len(refs) > 0 {
			g.Components[i].StandInValues = slices.Sorted(slices.Values(refs))
		}
	}
}

// MarkProviders records, per component ref, the module provider driving
// it. Refs the graph lacks are ignored.
func (g *Graph) MarkProviders(byRef map[string]string) {
	for i := range g.Components {
		g.Components[i].Provider = byRef[g.Components[i].Ref]
	}
}

// Upstream lists the refs of the components data passes before it reaches
// ref in the pipeline: the pipeline's receivers and the processors before
// ref, and for a connector receiver the components upstream of it in every
// pipeline that exports to it. A processor's own ref is included, an
// exporter's is not.
func (g Graph) Upstream(pipelineID, ref string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(pipelineID, ref string)
	walk = func(pipelineID, ref string) {
		key := pipelineID + " " + ref
		if seen[key] {
			return
		}
		seen[key] = true
		for _, p := range g.Pipelines {
			if p.ID != pipelineID {
				continue
			}
			add := func(r string) {
				if !slices.Contains(out, r) {
					out = append(out, r)
				}
			}
			for _, r := range p.Receivers {
				add(r)
				if strings.HasPrefix(r, string(KindConnector)+"/") {
					for _, q := range g.Pipelines {
						if slices.Contains(q.Exporters, r) {
							walk(q.ID, r)
						}
					}
				}
			}
			for _, r := range p.Processors {
				add(r)
				if r == ref {
					break
				}
			}
		}
	}
	walk(pipelineID, ref)
	return out
}

// Ref formats a component reference.
func Ref(kind Kind, id component.ID) string { return string(kind) + "/" + id.String() }

// Build derives the graph from cfg. Pipelines, components and edges are
// sorted, so equal configurations give equal graphs.
func Build(cfg *otelcol.Config, mode ModeFunc) Graph {
	g := Graph{Pipelines: []Pipeline{}, Components: []Component{}, Edges: []Edge{}}
	components := map[string]*Component{}
	use := func(kind Kind, id component.ID, p pipeline.ID) string {
		ref := Ref(kind, id)
		c, ok := components[ref]
		if !ok {
			m, note := mode(kind, id)
			c = &Component{Ref: ref, Kind: kind, ID: id.String(), Type: id.Type().String(), Mode: m, Note: note}
			components[ref] = c
		}
		if p.String() != "" && !slices.Contains(c.Pipelines, p.String()) {
			c.Pipelines = append(c.Pipelines, p.String())
		}
		return ref
	}

	connectors := cfg.Connectors
	pipelineIDs := slices.SortedFunc(maps.Keys(cfg.Service.Pipelines), func(a, b pipeline.ID) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, pid := range pipelineIDs {
		p := cfg.Service.Pipelines[pid]
		out := Pipeline{ID: pid.String(), Signal: pid.Signal().String(), Receivers: []string{}, Processors: []string{}, Exporters: []string{}}
		var first []string
		for _, id := range p.Receivers {
			kind := KindReceiver
			if _, ok := connectors[id]; ok {
				kind = KindConnector
			}
			ref := use(kind, id, pid)
			out.Receivers = append(out.Receivers, ref)
			first = append(first, ref)
		}
		prev := first
		for _, id := range p.Processors {
			ref := use(KindProcessor, id, pid)
			out.Processors = append(out.Processors, ref)
			for _, from := range prev {
				g.Edges = append(g.Edges, Edge{Pipeline: out.ID, Signal: out.Signal, From: from, To: ref})
			}
			prev = []string{ref}
		}
		for _, id := range p.Exporters {
			kind := KindExporter
			if _, ok := connectors[id]; ok {
				kind = KindConnector
			}
			ref := use(kind, id, pid)
			out.Exporters = append(out.Exporters, ref)
			for _, from := range prev {
				g.Edges = append(g.Edges, Edge{Pipeline: out.ID, Signal: out.Signal, From: from, To: ref})
			}
		}
		g.Pipelines = append(g.Pipelines, out)
	}
	for _, id := range cfg.Service.Extensions {
		use(KindExtension, id, pipeline.ID{})
	}

	for _, ref := range slices.Sorted(maps.Keys(components)) {
		g.Components = append(g.Components, *components[ref])
	}
	g.Unused = unused(cfg, components)
	return g
}

func unused(cfg *otelcol.Config, used map[string]*Component) []string {
	var out []string
	add := func(kind Kind, ids []component.ID) {
		for _, id := range ids {
			if _, ok := used[Ref(kind, id)]; !ok {
				out = append(out, Ref(kind, id))
			}
		}
	}
	add(KindReceiver, slices.Collect(maps.Keys(cfg.Receivers)))
	add(KindProcessor, slices.Collect(maps.Keys(cfg.Processors)))
	add(KindExporter, slices.Collect(maps.Keys(cfg.Exporters)))
	add(KindConnector, slices.Collect(maps.Keys(cfg.Connectors)))
	add(KindExtension, slices.Collect(maps.Keys(cfg.Extensions)))
	slices.Sort(out)
	return out
}
