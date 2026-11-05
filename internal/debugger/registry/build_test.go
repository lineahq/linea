package registry

import (
	"testing"

	"github.com/lineahq/linea/internal/debugger/release"
	"go.opentelemetry.io/collector/featuregate"
)

func TestChangedFeatureGatesListsNonDefaultGates(t *testing.T) {
	const id = "otel.debugger.test.alpha"
	g := featuregate.GlobalRegistry().MustRegister(id, featuregate.StageAlpha)
	if gates := ChangedFeatureGates(); containsGate(gates, id) {
		t.Fatalf("default-off alpha gate reported: %v", gates)
	}
	if err := featuregate.GlobalRegistry().Set(g.ID(), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = featuregate.GlobalRegistry().Set(g.ID(), false) })
	if gates := ChangedFeatureGates(); !containsGate(gates, id) {
		t.Fatalf("enabled alpha gate missing: %v", gates)
	}
}

func containsGate(gates []release.FeatureGate, id string) bool {
	for _, g := range gates {
		if g.ID == id && g.Enabled && g.Stage == "Alpha" {
			return true
		}
	}
	return false
}
