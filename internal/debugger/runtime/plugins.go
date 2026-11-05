package runtime

import (
	"fmt"

	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/mock/builtin"
	"github.com/lineahq/linea/internal/debugger/registry"
)

// providersOf lists the debugger providers compiled into the registry;
// tests replace it.
var providersOf = registry.Providers

// DrivingProviders maps the ref of each component of cfg that a module
// provider drives to the provider's name.
func DrivingProviders(cfg *otelcol.Config) map[string]string {
	plugins, _ := Plugins()
	out := map[string]string{}
	for ref, c := range mock.Modes(mock.Build(cfg, mock.Spec{}, nil, builtin.Providers, plugins)) {
		if c.Provider != "" {
			out[ref] = c.Provider
		}
	}
	return out
}

// Plugins are the registry's providers, each called once. A provider
// without a name, or with a name another has, is an error naming its
// package.
func Plugins() ([]mock.Plugin, error) {
	var out []mock.Plugin
	seen := map[string]string{}
	for _, r := range providersOf() {
		p := r.New()
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("the debugger provider of %s has no name", r.Package)
		case seen[p.Name] != "":
			return nil, fmt.Errorf("the debugger providers of %s and %s are both named %q", seen[p.Name], r.Package, p.Name)
		}
		seen[p.Name] = r.Package
		out = append(out, mock.Plugin{Package: r.Package, Provider: p})
	}
	return out, nil
}
