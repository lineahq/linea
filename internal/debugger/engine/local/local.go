// Package local is the in-process Collector of this build: the generated
// registry's factories, the instrumented runtime that executes them, the
// debugger's mock providers and the optional analysis features of the
// manifest. A process that executes on the built-in runtime passes Collector
// to engine.Use; one that links this package links the Collector components.
// It does not import the engine, so the engine's tests can use it.
package local

import (
	"context"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/mock/builtin"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/runtime"
	"github.com/lineahq/linea/internal/debugger/topology"
	"go.opentelemetry.io/collector/otelcol"

	// The manifest's optional analysis features register in init.
	_ "github.com/lineahq/linea/internal/debugger/featureset"
)

// Collector is this build's in-process Collector; it satisfies
// engine.Collector.
var Collector collector

type collector struct{}

func (collector) Factories() (otelcol.Factories, error) { return registry.Factories() }

func (collector) FeatureGates() []release.Gate { return registry.FeatureGates() }

func (collector) ChangedFeatureGates() []release.FeatureGate {
	return registry.ChangedFeatureGates()
}

func (collector) Plugins() ([]mock.Plugin, error) { return runtime.Plugins() }

func (collector) Builtins() []mock.Builtin { return builtin.Providers }

func (collector) ExecutionModes(cfg *otelcol.Config, factories otelcol.Factories) topology.ModeFunc {
	return runtime.ExecutionModes(cfg, factories)
}

func (collector) DrivingProviders(cfg *otelcol.Config) map[string]string {
	return runtime.DrivingProviders(cfg)
}

func (collector) WallClockReads(cfg *otelcol.Config) []runresult.ClockRead {
	return runtime.WallClockReads(cfg)
}

func (collector) CheckReferencePrerequisites(cfg *otelcol.Config) error {
	return runtime.CheckReferencePrerequisites(cfg)
}

func (collector) Run(ctx context.Context, opts runresult.Options) (*runresult.Result, error) {
	return runtime.Run(ctx, opts)
}

func (collector) RunReference(ctx context.Context, opts runresult.Options) (*runresult.Result, error) {
	return runtime.RunReference(ctx, opts)
}
