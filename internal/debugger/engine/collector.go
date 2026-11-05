package engine

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/topology"
	"go.opentelemetry.io/collector/otelcol"
)

// Collector is the Collector a process runs in process: the factories of the
// registry it links and the runtime that executes a configuration with them.
// The engine prepares, captures and encodes requests without one; executing,
// inspecting or previewing on the built-in runtime needs one. engine/local
// provides this build's, and a process that links none, such as the team
// server, sends that work to workers.
type Collector interface {
	// Factories are the registry's component factories.
	Factories() (otelcol.Factories, error)
	// FeatureGates are every gate the registry's components register.
	FeatureGates() []release.Gate
	// ChangedFeatureGates are the registry's gates not at their default.
	ChangedFeatureGates() []release.FeatureGate
	// Plugins are the module providers compiled in.
	Plugins() ([]mock.Plugin, error)
	// Builtins are the debugger's own mock providers.
	Builtins() []mock.Builtin
	// ExecutionModes says how the runtime executes each component.
	ExecutionModes(cfg *otelcol.Config, factories otelcol.Factories) topology.ModeFunc
	// DrivingProviders names the module provider driving each component.
	DrivingProviders(cfg *otelcol.Config) map[string]string
	// WallClockReads lists the statements that read the wall clock.
	WallClockReads(cfg *otelcol.Config) []runresult.ClockRead
	// CheckReferencePrerequisites refuses a configuration whose reference
	// run cannot be built.
	CheckReferencePrerequisites(cfg *otelcol.Config) error
	// Run executes the configuration on the inputs.
	Run(ctx context.Context, opts runresult.Options) (*runresult.Result, error)
	// RunReference executes the configuration without instrumentation.
	RunReference(ctx context.Context, opts runresult.Options) (*runresult.Result, error)
}

// ErrNoCollector marks work on the built-in runtime in a process that runs
// no Collector in process.
var ErrNoCollector = errors.New("this process runs no Collector in process; select a runtime")

var inProcess atomic.Pointer[Collector]

// Use sets the process's in-process Collector. A process calls it once,
// before preparing or executing requests.
func Use(c Collector) {
	inProcess.Store(&c)
}

// InProcess is the process's in-process Collector, or ErrNoCollector.
func InProcess() (Collector, error) { return collector() }

// collector is the process's in-process Collector, or ErrNoCollector.
func collector() (Collector, error) {
	if c := inProcess.Load(); c != nil && *c != nil {
		return *c, nil
	}
	return nil, ErrNoCollector
}

// changedFeatureGates are the in-process Collector's changed gates, none
// without one.
func changedFeatureGates() []release.FeatureGate {
	if c, err := collector(); err == nil {
		return c.ChangedFeatureGates()
	}
	return nil
}

// inProcessFactories is the in-process Collector with its factories.
func inProcessFactories() (Collector, otelcol.Factories, error) {
	c, err := collector()
	if err != nil {
		return nil, otelcol.Factories{}, err
	}
	f, err := c.Factories()
	return c, f, err
}
