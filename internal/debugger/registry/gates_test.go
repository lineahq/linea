package registry

import (
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/release"
)

// TestEveryGateHasAScope keeps gateowners.go current: every gate the
// registry holds was found in the worker's source and has a scope.
func TestEveryGateHasAScope(t *testing.T) {
	for _, g := range FeatureGates() {
		if strings.HasPrefix(g.ID, "otel.debugger.test.") {
			continue // Registered by this package's tests.
		}
		if g.Scope == "" {
			t.Errorf("gate %s has no scope: run go generate ./internal/debugger/registry", g.ID)
		}
		if (g.Scope == release.GateScopeComponent) != (len(g.Components) > 0) {
			t.Errorf("gate %s: scope %s with components %v", g.ID, g.Scope, g.Components)
		}
	}
}

func TestGatesBelongToTheComponentsLinkingThem(t *testing.T) {
	byID := map[string]release.Gate{}
	for _, g := range FeatureGates() {
		byID[g.ID] = g
	}
	if g := byID["filelog.mtimeSortType"]; g.Scope != release.GateScopeComponent || !slices.Contains(g.Components, "receivers/filelog") {
		t.Errorf("filelog.mtimeSortType = %+v", g)
	}
	if g := byID["service.profilesSupport"]; g.Scope != release.GateScopeCollector {
		t.Errorf("service.profilesSupport = %+v", g)
	}
	// otlp_grpc keeps its deprecated type, which configurations still use.
	if g := byID["pkg.exporterhelper.queueBatchEnabled"]; !slices.Contains(g.Components, "exporters/otlp") ||
		!slices.Contains(g.Components, "exporters/otlp_grpc") {
		t.Errorf("pkg.exporterhelper.queueBatchEnabled = %+v", g)
	}
}
