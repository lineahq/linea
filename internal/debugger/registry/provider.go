package registry

import "github.com/lineahq/linea/collector/provider"

// Provider is a component module's debugger provider compiled into the
// registry: the package exporting it and its
// DebuggerProvider function.
type Provider struct {
	Package string
	New     func() provider.Provider
}
