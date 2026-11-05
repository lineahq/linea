// Package builtin holds the debugger's own mock providers: the filelog
// filesystem, the Kubernetes API server for k8sattributes and the simulated
// clock for spanmetrics. It links those components, so only the in-process
// executor imports it; package mock keeps the types a run result carries.
package builtin

import (
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/mock"
)

// Providers are the built-in providers in the order a run prepares them.
var Providers = []mock.Builtin{
	func(cfg *otelcol.Config, spec mock.Spec, host mock.Host) mock.Provider {
		if p := newFilelog(cfg, spec, host); p != nil {
			return p
		}
		return nil
	},
	func(cfg *otelcol.Config, spec mock.Spec, _ mock.Host) mock.Provider {
		if p := newKubernetes(cfg, spec); p != nil {
			return p
		}
		return nil
	},
	func(cfg *otelcol.Config, spec mock.Spec, _ mock.Host) mock.Provider {
		if p := newClock(cfg, spec); p != nil {
			return p
		}
		return nil
	},
}
