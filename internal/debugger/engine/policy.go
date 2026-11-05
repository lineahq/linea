package engine

import (
	"fmt"

	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Policy is the trusted policy of a composition, applied to every operation
// that reads its sources: which ${env:NAME} values may be bound and which host
// files a request may name. One struct, so later policy adds a field rather
// than a parameter.
type Policy struct {
	Environment config.EnvironmentPolicy
	Paths       hostfs.Policy
}

// Validate checks both policies.
func (p Policy) Validate() error {
	if err := p.Environment.Validate(); err != nil {
		return err
	}
	return p.Paths.Validate()
}

// Clone returns a copy that shares no slices with p.
func (p Policy) Clone() Policy {
	return Policy{Environment: p.Environment.Clone(), Paths: p.Paths.Clone()}
}

// unrestricted is the path policy of the low-level engine entry points:
// host paths are read as before, relative ones from the working directory.
var unrestricted = hostfs.Policy{Unrestricted: true}

// captureOptions is the config capture under p with the default limits. The
// scenario's mocks.files answer ${file:} references by path.
func (p Policy) captureOptions(files []mock.File) config.CaptureOptions {
	opts := config.CaptureOptions{Environment: p.Environment, Paths: p.Paths}
	for _, f := range files {
		if opts.Files == nil {
			opts.Files = map[string][]byte{}
		}
		opts.Files[f.Path] = append(append([]byte(nil), f.Existing...), f.Content...)
	}
	return opts
}

// graph is the topology of a configuration resolved with factories, with
// its execution modes, per component the references it uses as stand-ins
// and the module provider driving it.
func graph(c Collector, resolved *config.Resolved, factories otelcol.Factories) topology.Graph {
	g := topology.Build(resolved.Config, c.ExecutionModes(resolved.Config, factories))
	byRef := map[string][]string{}
	for _, r := range resolved.References {
		if r.Source != config.SourceStandIn {
			continue
		}
		for _, c := range r.Components {
			byRef[c] = append(byRef[c], r.Scheme+":"+r.Target)
		}
	}
	g.MarkStandInValues(byRef)
	g.MarkProviders(c.DrivingProviders(resolved.Config))
	return g
}

// loadScenarioWith loads a scenario for inspection or preview, reading the
// scenario file and the files it names through the path policy. When the
// policy reads every path directly, it is loadScenario.
func loadScenarioWith(path, text, dir string, paths hostfs.Policy) (*scenario.Scenario, error) {
	if paths.Direct() {
		return loadScenario(path, text, dir)
	}
	read := func(name string) ([]byte, error) { return paths.ReadFile(name, maxInputBytes) }
	if path != "" {
		return scenario.LoadWith(path, read)
	}
	if dir != "" {
		var err error
		if dir, err = paths.Abs(dir); err != nil {
			return nil, err
		}
	}
	s, err := scenario.Parse([]byte(text), dir)
	if err != nil {
		return nil, fmt.Errorf("%w: scenario_yaml: %v", ErrInvalidRequest, err)
	}
	return s.WithFileReader(read), nil
}
