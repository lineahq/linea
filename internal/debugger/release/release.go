// Package release names what one Linea build is: the product version every
// binary of the build reports, the Collector release line and modules of its
// built-in runtime (collector.go, generated from the OCB manifest), and the
// feature gate types runtimes report. It imports nothing that links a
// Collector component, so the team server knows the built-in release without
// linking it; package registry holds the factories themselves.
package release

import "strings"

// VersionVariable is the variable a build sets to the release version
// without its "v", with -ldflags "-X <VersionVariable>=0.1.0". Release builds
// set it from the git tag, and runtime builds to the building server's
// ProductVersion, which workers and builders must match.
const VersionVariable = "github.com/lineahq/linea/internal/debugger/release.version"

// DevVersion is ProductVersion of a build that carries no version.
const DevVersion = "0.0.0-dev"

var version string

// ProductVersion is the debugger version: the one the build set, else
// DevVersion. Every binary of one build (CLI, server, worker, builder) must
// report the same one, so it never comes from per-binary build information.
var ProductVersion = productVersion(version)

// UnverifiedVariable is the variable linea build sets to "true", with
// -ldflags "-X <UnverifiedVariable>=true", in a CLI whose in-process
// Collector is a release outside the support table. Runtime builds mark
// their workers with the worker's own variable instead.
const UnverifiedVariable = "github.com/lineahq/linea/internal/debugger/release.unverified"

var unverified string

// Verified reports whether this binary's in-process Collector is a release
// of the support table: false only in a CLI linea build made for a newer
// release.
var Verified = unverified != "true"

func productVersion(set string) string {
	if set == "" {
		return DevVersion
	}
	return strings.TrimPrefix(set, "v")
}

// FeatureGate is a Collector feature gate whose state differs from the
// default of its stage.
type FeatureGate struct {
	ID      string `json:"id"`
	Stage   string `json:"stage"`
	Enabled bool   `json:"enabled"`
}

// Gate scopes: what links the code that registers a feature gate.
const (
	// GateScopeCollector is a gate the Collector itself links (its service,
	// pipelines or telemetry): it concerns every configuration.
	GateScopeCollector = "collector"
	// GateScopeComponent is a gate only components link; Components names
	// them.
	GateScopeComponent = "component"
	// GateScopeDebugger is a gate only the debugger's own code links, such
	// as the filelog receiver behind file mocks in a runtime without it: no
	// configuration on the runtime can use it.
	GateScopeDebugger = "debugger"
)

// Gate is a registered Collector feature gate as a runtime's registry holds
// it: its stage, whether it is enabled, and the releases it was added in and
// is removed in, as the gate declares them.
type Gate struct {
	ID          string `json:"id"`
	Stage       string `json:"stage"`
	Enabled     bool   `json:"enabled"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Description string `json:"description,omitempty"`
	// Scope says what links the code registering the gate: the Collector
	// (GateScopeCollector), only components (GateScopeComponent) or only
	// the debugger (GateScopeDebugger); empty when the build did not find
	// where it is registered, or for a worker from before scopes.
	Scope string `json:"scope,omitempty"`
	// Components are the "<kind>/<type>" names, deprecated types included,
	// of the components linking a component gate.
	Components []string `json:"components,omitempty"`
}
