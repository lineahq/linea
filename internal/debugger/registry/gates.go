package registry

import (
	"slices"

	"github.com/lineahq/linea/internal/debugger/release"
	"go.opentelemetry.io/collector/component"
)

// gateOwner is what links the package registering one feature gate, as
// internal/debugger/registry/gen found it in the worker's source (gateowners.go).
type gateOwner struct {
	// collector is true when the Collector's own packages link it.
	collector bool
	// packages are the component packages that link it.
	packages []string
}

// typeNames is a component's "<kind>/<type>" and, for a factory with a
// deprecated type, "<kind>/<deprecated type>".
func typeNames(kind string, f component.Factory) []string {
	names := []string{kind + "/" + f.Type().String()}
	if a, ok := f.(interface{ DeprecatedAlias() component.Type }); ok && a.DeprecatedAlias().String() != "" {
		names = append(names, kind+"/"+a.DeprecatedAlias().String())
	}
	return names
}

// gateScope returns a gate's scope and, for a component gate, the
// "<kind>/<type>" names of the components that link it, sorted. A gate the
// generator did not find has no scope.
func gateScope(id string, types map[string][]string) (string, []string) {
	o, ok := gateOwners[id]
	switch {
	case !ok:
		return "", nil
	case o.collector:
		return release.GateScopeCollector, nil
	case len(o.packages) == 0:
		return release.GateScopeDebugger, nil
	}
	var names []string
	for _, p := range o.packages {
		names = append(names, types[p]...)
	}
	slices.Sort(names)
	return release.GateScopeComponent, slices.Compact(names)
}
