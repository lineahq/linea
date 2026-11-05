// Package provider is the debugger's provider SDK: what a
// component module implements so that the OpenTelemetry Collector Pipeline
// Debugger can drive its components during a run and say what it did.
//
// A package of the module exports
//
//	func DebuggerProvider() provider.Provider
//
// and a runtime build that has the module's source finds it, compiles it
// into the runtime's worker and names it in the runtime and its reports. A
// run whose configuration uses one of the provider's component types calls
// Prepare once, before the Collector starts; the provider then serves the
// component's client endpoints, acts on the run's timeline through hooks and
// reports evidence. The component itself runs unmodified (mode
// dependency_mocked).
//
// This module imports only the standard library, so it never moves a
// Collector release's dependencies.
//
// Compatibility: within a minor version, changes are additions only: new
// fields of Provider, new methods of Run and Static, which only the
// debugger implements. A provider written against version v0.N builds with
// every debugger embedding v0.N or later. Hook order and the evidence
// methods are part of the contract; Logf output is not.
package provider

import (
	"context"
	"net"
)

// Version is this SDK's version. A runtime build refuses a module that
// requires a later one than the debugger embeds.
const Version = "0.1.0"

// Provider is what a component module hands the debugger.
type Provider struct {
	// Name identifies the provider in runtimes, reports and the scenario's
	// mocks.providers settings. It is required and unique in a runtime.
	Name string
	// Components are the component types the provider acts for. Prepare is
	// called only in runs whose configuration uses one of them. Processors,
	// connectors and extensions run for real in a run; receivers and
	// exporters are replaced, so a provider has nothing to drive there.
	Components []Type
	// Endpoints are the client endpoints of those components that the
	// provider serves itself, instead of the debugger's loopback OTLP sink.
	Endpoints []Endpoint
	// Prepare is called once per run, before the Collector starts. It reads
	// the provider's settings, takes its listeners and registers hooks.
	// An error fails the run, naming the provider.
	Prepare func(ctx context.Context, run Run) error
	// Check, optional, reports at inspection time, without running
	// anything, what the scenario's settings lack for the components.
	Check func(run Static) []Finding
}

// Type is a component type.
type Type struct {
	// Kind is "receiver", "processor", "exporter", "connector" or
	// "extension".
	Kind string
	// Type is the component type, as configurations name it.
	Type string
}

// Endpoint names a client endpoint in a component's configuration by its
// path: the configuration keys leading to it, joined by "::" ("control",
// "routes[0]::client"). The path is either a configgrpc or confighttp
// client configuration, whose endpoint is pointed at the provider's
// listener with TLS, auth and proxies off, or a string field holding an
// address ("coordinator::url"), whose host and port are replaced by the
// listener's.
type Endpoint struct {
	Path string
}

// Run is one run, as a provider sees it. Only the debugger implements it,
// so methods may be added in minor versions.
type Run interface {
	// Components are the configured instances of the provider's types that
	// the run executes.
	Components() []Component
	// Settings decodes the scenario's mocks.providers.<Name>, as JSON, into
	// target. Absent settings leave target unchanged.
	Settings(target any) error
	// Listener is the loopback listener a component's endpoint (one of
	// Provider.Endpoints) points at during the run. The provider serves
	// its protocol on it and closes it, or the run closes it at its end.
	Listener(component, path string) (net.Listener, error)
	// Record hands the run telemetry a served endpoint received: an OTLP
	// protobuf export request (ExportTraceServiceRequest,
	// ExportMetricsServiceRequest or ExportLogsServiceRequest) of signal
	// "traces", "metrics" or "logs". It becomes an endpoint output, which
	// scenario assertions can check.
	Record(component, path, signal string, otlp []byte) error

	// OnReady runs once the Collector is running, before the first input.
	// It may wait, within ctx, until the component is ready.
	OnReady(func(ctx context.Context) error)
	// OnInput runs before each input is injected, after the input's
	// declared wait.
	OnInput(func(ctx context.Context, in Input) error)
	// OnInputsDone runs after the last input, before the run settles and
	// the Collector shuts down.
	OnInputsDone(func(ctx context.Context) error)
	// OnClose runs after the Collector shut down, also when the run failed.
	// Evidence reported until it returns is part of the run.
	OnClose(func() error)

	// Supplied records what the provider gave a component
	// ("session s1 at traces/after_filter").
	Supplied(component, what string)
	// Limitation records where the run differs from a deployment.
	Limitation(component, what string)
	// Warning records a setting that had no effect or a problem the run
	// survived.
	Warning(component, what string)
	// Logf writes to the run's log, never to its report.
	Logf(format string, args ...any)
}

// Component is a configured component of one of the provider's types.
type Component struct {
	// ID is "<kind>/<id>", as reports name it: "extension/livecapture".
	ID string
	// Config is the component's effective configuration, defaults
	// included, by its configuration keys. Secrets are redacted.
	Config map[string]any
}

// Input is a scenario input about to be injected.
type Input struct {
	// Index counts inputs from 0.
	Index int
	// Receiver is the receiver it is injected into ("otlp").
	Receiver string
	// Signal is "traces", "metrics" or "logs".
	Signal string
}

// Static is what Check sees: the components and the settings, with nothing
// running.
type Static interface {
	Components() []Component
	Settings(target any) error
	// Pipelines are the configuration's pipelines, ordered by ID.
	Pipelines() []Pipeline
}

// Pipeline is a pipeline of the configuration.
type Pipeline struct {
	// ID is the pipeline's ID, as the configuration names it
	// ("traces", "logs/raw").
	ID string
	// Signal is "traces", "metrics" or "logs".
	Signal string
	// Processors are the pipeline's processors in order, with their
	// configuration.
	Processors []Component
}

// Finding is something Check reports for a component.
type Finding struct {
	Component string
	Message   string
	Hint      string
	// Settings, optional, are settings that would resolve the finding,
	// which the debugger offers to add under mocks.providers.<Name> in one
	// step: a value that encoding/json marshals into an object. Keys the
	// scenario already sets are kept.
	Settings any
}
