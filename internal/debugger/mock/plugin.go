package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Plugin is a provider a component module supplies,
// compiled into the runtime from the module's package.
type Plugin struct {
	// Package is the import path of the package exporting it.
	Package  string
	Provider provider.Provider
}

// plugin runs a module's provider for the configured instances of its
// component types. Its components run unmodified: it wraps no factory.
type plugin struct {
	pkg   string
	p     provider.Provider
	cfg   *otelcol.Config
	spec  Spec
	comps []Component
}

func newPlugin(cfg *otelcol.Config, spec Spec, pl Plugin) *plugin {
	p := &plugin{pkg: pl.Package, p: pl.Provider, cfg: cfg, spec: spec}
	for _, t := range pl.Provider.Components {
		for _, id := range usedOfKind(cfg, topology.Kind(t.Kind)) {
			if id.Type().String() == t.Type {
				p.comps = append(p.comps, Component{Kind: topology.Kind(t.Kind), ID: id, Mode: topology.ModeDependencyMocked,
					Note:     fmt.Sprintf("the real %s runs, driven by the provider %s (%s)", t.Kind, pl.Provider.Name, pl.Package),
					Provider: pl.Provider.Name})
			}
		}
	}
	if len(p.comps) == 0 {
		return nil
	}
	return p
}

func (p *plugin) Name() string            { return p.p.Name }
func (p *plugin) Package() string         { return p.pkg }
func (p *plugin) Components() []Component { return p.comps }

func (p *plugin) Receiver(f receiver.Factory) (receiver.Factory, bool)    { return f, false }
func (p *plugin) Processor(f processor.Factory) (processor.Factory, bool) { return f, false }
func (p *plugin) Connector(f connector.Factory) (connector.Factory, bool) { return f, false }

func (p *plugin) Prepare(ctx context.Context, run provider.Run) error {
	if p.p.Prepare == nil {
		return nil
	}
	return p.p.Prepare(ctx, run)
}

// Gaps are the provider's Check findings.
func (p *plugin) Gaps() (out []Gap) {
	if p.p.Check == nil {
		return nil
	}
	r := &run{p: p, cfg: p.cfg, settings: p.spec.Providers[p.p.Name]}
	defer func() {
		if v := recover(); v != nil {
			out = append(out, Gap{Ref: topology.Ref(p.comps[0].Kind, p.comps[0].ID),
				Message: fmt.Sprintf("the provider %s failed to check the scenario: panic: %v", p.p.Name, v)})
		}
	}()
	for _, f := range p.p.Check(r) {
		g := Gap{Ref: f.Component, Message: f.Message, Hint: f.Hint}
		if f.Settings != nil {
			b, err := json.Marshal(f.Settings)
			if err != nil || !isObject(b) {
				out = append(out, Gap{Ref: f.Component,
					Message: fmt.Sprintf("the provider %s proposed settings that are not a JSON object; they are not offered", p.p.Name)})
			} else {
				g.Provider, g.Settings = p.p.Name, b
			}
		}
		out = append(out, g)
	}
	return out
}

func isObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// endpointPaths are the endpoint paths a provider serves itself.
func endpointPaths(p Provider) []string {
	pl, ok := p.(*plugin)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range pl.p.Endpoints {
		out = append(out, e.Path)
	}
	return out
}

// CheckSettings refuses scenario settings (mocks.providers) for a name no
// provider of the runtime has.
func CheckSettings(spec Spec, plugins []Plugin) error {
	var unknown []string
	for name := range spec.Providers {
		if !slices.ContainsFunc(plugins, func(pl Plugin) bool { return pl.Provider.Name == name }) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("mocks.providers names %s, which the runtime has no provider for (it has %s)",
		strings.Join(unknown, ", "), PluginNames(plugins))
}

// PluginNames lists the providers' names for messages.
func PluginNames(plugins []Plugin) string {
	if len(plugins) == 0 {
		return "none"
	}
	var names []string
	for _, pl := range plugins {
		names = append(names, pl.Provider.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// usedOfKind returns the IDs of a kind the configuration runs, sorted,
// once each: pipeline components, and the service's extensions.
func usedOfKind(cfg *otelcol.Config, kind topology.Kind) []component.ID {
	if cfg == nil {
		return nil
	}
	seen := map[component.ID]bool{}
	var out []component.ID
	add := func(id component.ID) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if kind == topology.KindExtension {
		for _, id := range cfg.Service.Extensions {
			add(id)
		}
	}
	for _, pl := range cfg.Service.Pipelines {
		switch kind {
		case topology.KindReceiver:
			for _, id := range pl.Receivers {
				if _, c := cfg.Connectors[id]; !c {
					add(id)
				}
			}
		case topology.KindProcessor:
			for _, id := range pl.Processors {
				add(id)
			}
		case topology.KindExporter:
			for _, id := range pl.Exporters {
				if _, c := cfg.Connectors[id]; !c {
					add(id)
				}
			}
		case topology.KindConnector:
			for _, id := range pl.Exporters {
				if _, c := cfg.Connectors[id]; c {
					add(id)
				}
			}
		}
	}
	sortIDs(out)
	return out
}
