package engine

import (
	"slices"

	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Runtime kinds of a report.
const (
	// RuntimeBuiltIn is the debugger's own registry, in process or in a
	// worker of the same build.
	RuntimeBuiltIn = "built_in"
	// RuntimeContainer is a built runtime's worker in a container.
	RuntimeContainer = "container"
	// RuntimeRemote is the runtime of a worker that connected over the
	// network: its isolation is its operator's, not the
	// local container's.
	RuntimeRemote = "remote"
	// RuntimeImage is a runtime image's binary read from a registry and run
	// as a child process of a direct command: Name is the image
	// by digest. Its isolation is the host's, not a container's.
	RuntimeImage = "image"
)

// ReportRuntime is the runtime a run executed on (report schema 0.9).
// A built runtime names its image, the digests of its manifest and module
// set, and whether its Collector version is in the support table.
type ReportRuntime struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	// Endpoint is where a remote runtime's worker was reached. Only reports
	// of debuggers that dialed workers (report schema 0.11) have it; a
	// connected worker's runtime leaves it empty.
	Endpoint         string `json:"endpoint,omitempty"`
	ImageID          string `json:"image_id,omitempty"`
	CollectorVersion string `json:"collector_version"`
	ManifestDigest   string `json:"manifest_digest,omitempty"`
	ModuleSet        string `json:"module_set,omitempty"`
	Verified         bool   `json:"verified"`
	// CustomModules are the modules built from supplied sources.
	CustomModules []ReportCustomModule  `json:"custom_modules,omitempty"`
	FeatureGates  []release.FeatureGate `json:"feature_gates"`
	// MissingFeatures are the optional analysis features this runtime
	// lacks because its manifest does not have their component
	// (report schema 0.10): features.OTTLReplay and the like.
	MissingFeatures []string `json:"missing_features,omitempty"`
	// Providers are the debugger providers of component modules compiled
	// into the runtime (report schema 0.13).
	Providers []ReportProvider `json:"providers,omitempty"`
}

// ReportProvider is a component module's debugger provider: its name, the
// package exporting it and the component types it acts for
// ("extension/livecapture").
type ReportProvider struct {
	Name       string   `json:"name"`
	Package    string   `json:"package"`
	Components []string `json:"components,omitempty"`
}

// Providers describes the providers of this process's registry.
func Providers(plugins []mock.Plugin) []ReportProvider {
	var out []ReportProvider
	for _, pl := range plugins {
		rp := ReportProvider{Name: pl.Provider.Name, Package: pl.Package}
		for _, t := range pl.Provider.Components {
			rp.Components = append(rp.Components, t.Kind+"/"+t.Type)
		}
		out = append(out, rp)
	}
	return out
}

// ReportCustomModule is a module built from a supplied source: a directory
// or a git repository with its ref, and the commit it resolved to.
type ReportCustomModule struct {
	Module string `json:"module"`
	Source string `json:"source"`
	Commit string `json:"commit,omitempty"`
}

// BuiltInRuntime describes the runtime of the in-process Collector; without
// one it names the release only.
func BuiltInRuntime() *ReportRuntime {
	gates := changedFeatureGates()
	if gates == nil {
		gates = []release.FeatureGate{}
	}
	var plugins []mock.Plugin
	if c, err := collector(); err == nil {
		plugins, _ = c.Plugins()
	}
	return &ReportRuntime{Kind: RuntimeBuiltIn, CollectorVersion: release.CollectorVersion, Verified: release.Verified, FeatureGates: gates,
		MissingFeatures: MissingFeatures(), Providers: Providers(plugins)}
}

// MissingFeatures names the optional features this process lacks,
// or nil.
func MissingFeatures() []string {
	var out []string
	for _, n := range features.Missing() {
		out = append(out, string(n))
	}
	return out
}

// readableReportSchemas are the stored report schemas this build reads: a
// 0.8 report is a 0.9 report without its runtime, a 0.9 report a 0.10
// report without missing features, a 0.10 report a 0.11 report without a
// remote runtime, a 0.11 report a 0.12 report without components' own logs
// and metrics, a 0.12 report a 0.13 report without providers, and
// a 0.14 report a 0.15 report without batching and grouping.
var readableReportSchemas = []string{"0.8", "0.9", "0.10", "0.11", "0.12", "0.13", "0.14", ReportSchemaVersion}

// ReadableReportSchema reports whether a stored report of schema v decodes
// into this build's Report.
func ReadableReportSchema(v string) bool { return slices.Contains(readableReportSchemas, v) }
