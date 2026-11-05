// Package mock substitutes the external dependencies of Collector components
// for a run, as a scenario declares them. The Collector YAML is
// never changed: a provider wraps the real component's factory and points
// the component at a controlled stand-in for its dependency, such as an
// isolated directory instead of the host filesystem. The component itself
// still executes (mode dependency_mocked).
package mock

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Spec is the mock environment of a run, decoded from a scenario's mocks
// section.
type Spec struct {
	// Files form the filesystem that filelog receivers read.
	Files []File
	// Objects are the Kubernetes objects the mocked API server serves to
	// k8sattributes processors, as decoded manifests (apiVersion, kind,
	// metadata, ...).
	Objects []map[string]any
	// Drain, when positive, is how long a run waits after the last
	// telemetry a mocked receiver emitted before it shuts down. Providers
	// otherwise derive it from the component configuration.
	Drain time.Duration
	// Clock is where simulated clocks start, for components that act on a
	// timer (spanmetrics). Zero means the time the component is created.
	Clock time.Time
	// Providers are the settings of module providers by name
	// (mocks.providers), as JSON, handed to them through provider.Run.
	Providers map[string]json.RawMessage
}

// File is one file of the mocked filesystem.
type File struct {
	// Path is where the file lives in the deployment, for example
	// /var/log/pods/shop_checkout-1_uid/app/0.log. Include patterns are
	// matched against it.
	Path string
	// Existing is the content present before the Collector starts. Whether
	// it is read depends on the receiver's start_at.
	Existing []byte
	// Content is written while the Collector runs, after the receiver's
	// first poll, as a running application would. It is always read.
	Content []byte
	// Modified, when set, is the file's modification time after each write.
	Modified time.Time
}

// Host is how a provider hands the run what its components do. The runtime
// implements it.
type Host interface {
	// Received records telemetry that a receiver running against a mocked
	// dependency emitted, before it enters the pipeline. The receiver passes
	// the returned context on with the telemetry, which links the pipeline's
	// observations to the recording.
	Received(ctx context.Context, id component.ID, p telemetry.Payload) context.Context
	// Register makes a receiver's next consumer available, so scenario
	// inputs can still be injected where the receiver delivers data.
	Register(id component.ID, signal pipeline.Signal, next any)
}

// Component is a configured component whose dependency a provider
// substitutes.
type Component struct {
	Kind topology.Kind
	ID   component.ID
	Mode topology.Mode
	Note string
	// Provider names the module provider driving the component; empty for
	// the built-in mocks.
	Provider string
}

// Declaration records what a provider mocked for one component in a run.
// It is part of the run result.
type Declaration struct {
	Provider string        `json:"provider"`
	Ref      string        `json:"component"`
	Mode     topology.Mode `json:"mode"`
	// Supplied lists the fixtures the component used, for example the file
	// paths a filelog receiver matched.
	Supplied []string `json:"supplied,omitempty"`
	// Limitations says where the run differs from a real deployment.
	Limitations []string `json:"limitations,omitempty"`
	// Warnings report fixtures that had no effect, such as a file no
	// include pattern matches.
	Warnings []string `json:"warnings,omitempty"`
}

// Provider substitutes one kind of external dependency, or
// drives a component module's own components. A provider is
// built for one run and runs on the provider SDK's lifecycle (Session):
// Prepare before the Collector starts, then the hooks it registers on its
// provider.Run.
type Provider interface {
	// Name identifies the provider in declarations ("filelog").
	Name() string
	// Components lists the components the provider handles.
	Components() []Component
	// Receiver returns the factory to run for f's type, wrapping f, and
	// true when the provider handles that type. Only the debugger's own
	// providers wrap factories.
	Receiver(f receiver.Factory) (receiver.Factory, bool)
	// Processor is Receiver for processor factories.
	Processor(f processor.Factory) (processor.Factory, bool)
	// Connector is Receiver for connector factories.
	Connector(f connector.Factory) (connector.Factory, bool)
	// Prepare is called once before the Collector starts; it registers the
	// provider's hooks on run.
	Prepare(ctx context.Context, run provider.Run) error
	// Gaps reports, without running anything, components the spec gives
	// nothing (or nothing usable) to read.
	Gaps() []Gap
}

// Lifecycle is how the debugger's own providers (package builtin) run:
// Start before the Collector, Ready once it runs, Drive after the inputs,
// Declarations once it shut down, Close at the end.
type Lifecycle struct {
	Start        func(context.Context) error
	Ready        func(context.Context) error
	Drive        func(context.Context) error
	Declarations func() []Declaration
	Close        func() error
}

// PrepareBuiltin registers a built-in provider's lifecycle on run. Its
// declarations become the run's evidence through run, as a module's
// provider reports it. Close runs even when Start failed.
func PrepareBuiltin(ctx context.Context, run provider.Run, l Lifecycle) error {
	run.OnClose(func() error {
		for _, d := range l.Declarations() {
			for _, s := range d.Supplied {
				run.Supplied(d.Ref, s)
			}
			for _, s := range d.Limitations {
				run.Limitation(d.Ref, s)
			}
			for _, s := range d.Warnings {
				run.Warning(d.Ref, s)
			}
		}
		return l.Close()
	})
	if err := l.Start(ctx); err != nil {
		return err
	}
	run.OnReady(l.Ready)
	run.OnInputsDone(l.Drive)
	return nil
}

// Gap is a mocked component whose dependency the scenario does not supply
// usably. A run still executes, against an empty or
// unmatched dependency, and its declarations warn.
type Gap struct {
	Ref     string
	Message string
	Hint    string
	// Provider and Settings are a module provider's proposed settings for
	// mocks.providers.<Provider> (a JSON object), when its finding has them.
	Provider string
	Settings json.RawMessage
}

// Builtin builds one of the debugger's own providers for a configuration,
// or returns nil when the configuration uses none of its components. The
// providers live in package builtin, which links the components they
// substitute; only the in-process executor passes them.
type Builtin func(cfg *otelcol.Config, spec Spec, host Host) Provider

// Gaps checks the spec against the configuration's mocked components before
// a run.
func Gaps(cfg *otelcol.Config, spec Spec, builtins []Builtin, plugins []Plugin) []Gap {
	var out []Gap
	for _, p := range Build(cfg, spec, nil, builtins, plugins) {
		out = append(out, p.Gaps()...)
	}
	return out
}

// Build returns the providers a configuration needs: the built-in ones in
// order, then the plugins whose component types it uses, in the plugins'
// order. host may be nil when only Components is used (to derive execution
// modes).
func Build(cfg *otelcol.Config, spec Spec, host Host, builtins []Builtin, plugins []Plugin) []Provider {
	var out []Provider
	for _, b := range builtins {
		if p := b(cfg, spec, host); p != nil {
			out = append(out, p)
		}
	}
	for _, pl := range plugins {
		if p := newPlugin(cfg, spec, pl); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// Modes indexes the components that providers handle by topology ref.
func Modes(providers []Provider) map[string]Component {
	out := map[string]Component{}
	for _, p := range providers {
		for _, c := range p.Components() {
			out[topology.Ref(c.Kind, c.ID)] = c
		}
	}
	return out
}

// UsedReceivers returns the receiver IDs of the given types that a pipeline
// uses, sorted, once each.
func UsedReceivers(cfg *otelcol.Config, types ...component.Type) []component.ID {
	seen := map[component.ID]bool{}
	var out []component.ID
	for _, p := range cfg.Service.Pipelines {
		for _, id := range p.Receivers {
			if slices.Contains(types, id.Type()) && !seen[id] {
				if _, connector := cfg.Connectors[id]; connector {
					continue
				}
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sortIDs(out)
	return out
}

// UsedProcessors returns the processor IDs of the given types that a
// pipeline uses, sorted, once each.
func UsedProcessors(cfg *otelcol.Config, types ...component.Type) []component.ID {
	seen := map[component.ID]bool{}
	var out []component.ID
	for _, p := range cfg.Service.Pipelines {
		for _, id := range p.Processors {
			if slices.Contains(types, id.Type()) && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sortIDs(out)
	return out
}

// aliasProvider is implemented by factories registered under a deprecated
// type alias.
type aliasProvider interface{ DeprecatedAlias() component.Type }

func sortIDs(ids []component.ID) {
	slices.SortFunc(ids, func(a, b component.ID) int { return strings.Compare(a.String(), b.String()) })
}
