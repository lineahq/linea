package manifest

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The parts of an OCB manifest the debugger's builder honors. Every
// other dist field and top-level section only steers OCB's own output and is
// ignored with a warning.
var (
	honoredDist = []string{"name", "description", "version", "otelcol_version"}
	honoredTop  = []string{"dist", "receivers", "processors", "exporters", "connectors", "extensions",
		"providers", "converters", "telemetry", "replaces", "excludes"}
)

// Warnings lists the manifest fields the builder ignores, in manifest order.
// The manifest itself stays unchanged and usable with the user's own OCB.
func Warnings(raw []byte) []string {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	top := doc.Content[0].Content
	for i := 0; i+1 < len(top); i += 2 {
		key := top[i].Value
		if !slices.Contains(honoredTop, key) {
			out = append(out, fmt.Sprintf("%s only steers OCB's own build and is ignored", key))
			continue
		}
		if key != "dist" || top[i+1].Kind != yaml.MappingNode {
			continue
		}
		dist := top[i+1].Content
		for j := 0; j+1 < len(dist); j += 2 {
			if k := dist[j].Value; !slices.Contains(honoredDist, k) {
				out = append(out, fmt.Sprintf("dist.%s only steers OCB's own build and is ignored", k))
			}
		}
	}
	return out
}

// Upstream reports whether a module belongs to the Collector core or
// contrib repositories, whose versions a release fixes.
func Upstream(module string) bool {
	return module == "go.opentelemetry.io/collector" ||
		strings.HasPrefix(module, "go.opentelemetry.io/collector/") ||
		strings.HasPrefix(module, "github.com/open-telemetry/opentelemetry-collector-contrib/")
}

// deriveVersion is the release line the manifest's core modules name, for a
// manifest without dist.otelcol_version (upstream's own manifests have
// none): the version of its v0 core component modules, which must agree.
func (m *Manifest) deriveVersion() (string, error) {
	seen := map[string]bool{}
	for _, kind := range Kinds {
		for _, c := range m.Components(kind) {
			if p := c.ModulePath(); Upstream(p) && !strings.HasPrefix(p, "github.com/") && strings.HasPrefix(c.Version(), "v0.") {
				seen[strings.TrimPrefix(c.Version(), "v")] = true
			}
		}
	}
	switch len(seen) {
	case 1:
		for v := range seen {
			return v, nil
		}
	case 0:
		return "", fmt.Errorf("manifest: dist.otelcol_version is missing and no core component names the Collector release")
	}
	versions := make([]string, 0, len(seen))
	for v := range seen {
		versions = append(versions, v)
	}
	slices.Sort(versions)
	return "", fmt.Errorf("manifest: dist.otelcol_version is missing and the core components name several releases (%s)", strings.Join(versions, ", "))
}

// CollectorVersion is the manifest's Collector release, without a "v".
func (m *Manifest) CollectorVersion() string {
	return strings.TrimPrefix(m.Dist.OtelcolVersion, "v")
}

// Replace is one entry of the manifest's replaces: "old [version] => new
// [version]", where a new path without a version is a local directory.
type Replace struct {
	Old, OldVersion string
	New, NewVersion string
}

// Local reports whether the replacement is a local directory.
func (r Replace) Local() bool { return r.NewVersion == "" }

// ParseReplaces decodes the manifest's replaces.
func (m *Manifest) ParseReplaces() ([]Replace, error) {
	out := make([]Replace, 0, len(m.Replaces))
	for _, s := range m.Replaces {
		left, right, ok := strings.Cut(s, "=>")
		lf, rf := strings.Fields(left), strings.Fields(right)
		if !ok || len(lf) < 1 || len(lf) > 2 || len(rf) < 1 || len(rf) > 2 {
			return nil, fmt.Errorf("manifest: replaces: %q must be \"<module> [version] => <module> <version>\" or \"<module> [version] => <directory>\"", s)
		}
		r := Replace{Old: lf[0], New: rf[0]}
		if len(lf) == 2 {
			r.OldVersion = lf[1]
		}
		if len(rf) == 2 {
			r.NewVersion = rf[1]
		}
		out = append(out, r)
	}
	return out, nil
}

// Entries lists every module entry the build compiles: the components,
// then the providers, converters and telemetry, each with where it is.
func (m *Manifest) Entries() []Entry {
	var out []Entry
	add := func(where string, list []Module) {
		for i, c := range list {
			out = append(out, Entry{Where: fmt.Sprintf("%s[%d]", where, i), Module: c})
		}
	}
	for _, kind := range Kinds {
		add(string(kind), m.Components(kind))
	}
	add("providers", m.Providers)
	add("converters", m.Converters)
	if m.Telemetry != nil {
		out = append(out, Entry{Where: "telemetry", Module: *m.Telemetry})
	}
	return out
}

// Entry is a module entry and its place in the manifest.
type Entry struct {
	Where string
	Module
}
