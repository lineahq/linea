package registry

import (
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/release"
	"go.opentelemetry.io/collector/featuregate"
)

// FeatureGates lists every gate of the global registry, sorted by ID: what
// an upgrade check compares between two runtimes.
func FeatureGates() []release.Gate {
	gates := []release.Gate{}
	types := componentTypes()
	featuregate.GlobalRegistry().VisitAll(func(g *featuregate.Gate) {
		scope, components := gateScope(g.ID(), types)
		gates = append(gates, release.Gate{ID: g.ID(), Stage: g.Stage().String(), Enabled: g.IsEnabled(),
			FromVersion: g.FromVersion(), ToVersion: g.ToVersion(), Description: g.Description(),
			Scope: scope, Components: components})
	})
	slices.SortFunc(gates, func(a, b release.Gate) int { return strings.Compare(a.ID, b.ID) })
	return gates
}

// ChangedFeatureGates lists the gates of the global registry that are not in
// their stage default (alpha and deprecated gates are off by default, beta
// and stable gates on), sorted by ID. Gates in their default state follow
// from release.CollectorVersion and the module list.
func ChangedFeatureGates() []release.FeatureGate {
	gates := []release.FeatureGate{}
	featuregate.GlobalRegistry().VisitAll(func(g *featuregate.Gate) {
		def := g.Stage() == featuregate.StageBeta || g.Stage() == featuregate.StageStable
		if g.IsEnabled() != def {
			gates = append(gates, release.FeatureGate{ID: g.ID(), Stage: g.Stage().String(), Enabled: g.IsEnabled()})
		}
	})
	slices.SortFunc(gates, func(a, b release.FeatureGate) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return gates
}
