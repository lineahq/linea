// Package catalog writes OCB manifests from a release's component catalog:
// the "all core and contrib" and "built-in set" presets, the
// components a configuration uses, and the picker's own selection. Every
// path ends in an ordinary manifest the user can edit, save or build with
// their own OCB. It is pure: it reads only the catalog and its inputs.
package catalog

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

// The presets.
const (
	// PresetAll is every core and contrib component the debugger supports
	// at the release.
	PresetAll = "all"
	// PresetBuiltIn is the built-in runtime's manifest, unchanged.
	PresetBuiltIn = "built-in"
)

// Request selects the components of a manifest: a preset, or the
// components a configuration uses together with picked ones.
type Request struct {
	Preset string
	// Config is a Collector configuration whose component types are
	// selected.
	Config []byte
	// Components are picked as "<kind>/<type>", such as
	// "processors/transform".
	Components []string
	// Custom are components built from supplied module sources; their
	// entries follow the catalog's (AddCustom).
	Custom []Custom
}

// Result is a written manifest.
type Result struct {
	Manifest []byte
	// Unknown lists "<kind>/<type>" the request named that neither the
	// catalog nor a custom component's type covers: custom components the
	// user adds with their own module.
	Unknown []string
	// Warnings name selected components the debugger does not support, with
	// the reason.
	Warnings []string
}

// Compose writes a manifest for the release of cat. builtIn is the
// built-in runtime's manifest, the PresetBuiltIn result at its release.
func Compose(cat *versions.Catalog, builtIn []byte, req Request) (*Result, error) {
	res, err := compose(cat, builtIn, req)
	if err != nil {
		return nil, err
	}
	if res.Manifest, err = AddCustom(res.Manifest, req.Custom); err != nil {
		return nil, err
	}
	return res, nil
}

func compose(cat *versions.Catalog, builtIn []byte, req Request) (*Result, error) {
	if req.Preset != "" && (len(req.Config) > 0 || len(req.Components) > 0) {
		return nil, fmt.Errorf("a preset cannot be combined with a configuration or components")
	}
	switch req.Preset {
	case "":
	case PresetBuiltIn:
		m, err := manifest.Parse(builtIn)
		if err != nil {
			return nil, err
		}
		if m.CollectorVersion() != cat.CollectorVersion {
			return nil, fmt.Errorf("the built-in set is the Collector %s manifest; for %s, start from it and move it to that release", m.CollectorVersion(), cat.CollectorVersion)
		}
		return &Result{Manifest: bytes.Clone(builtIn), Unknown: []string{}, Warnings: []string{}}, nil
	case PresetAll:
		var picked []versions.Component
		for _, c := range cat.Components {
			if c.Unsupported == "" {
				picked = append(picked, c)
			}
		}
		return write(cat, picked, "every core and contrib component the debugger supports", nil, nil)
	default:
		return nil, fmt.Errorf("unknown preset %q: use %q or %q", req.Preset, PresetAll, PresetBuiltIn)
	}

	wanted := slices.Clone(req.Components)
	if len(req.Config) > 0 {
		used, err := ConfigComponents(req.Config)
		if err != nil {
			return nil, err
		}
		wanted = append(wanted, used...)
	}
	if len(wanted) == 0 && len(req.Custom) == 0 {
		return nil, fmt.Errorf("select a preset, a configuration or components")
	}
	selected := map[string]bool{}
	var unknown, warnings []string
	for _, w := range wanted {
		kind, typ, ok := strings.Cut(w, "/")
		if !ok || !slices.Contains(manifest.Kinds, manifest.Kind(kind)) || typ == "" {
			return nil, fmt.Errorf("component %q must be \"<kind>/<type>\" with a kind of %s", w, kindList())
		}
		i := slices.IndexFunc(cat.Components, func(c versions.Component) bool { return c.Kind == kind && c.Type == typ })
		if i < 0 {
			// A deprecated type still names its component.
			i = slices.IndexFunc(cat.Components, func(c versions.Component) bool { return c.Kind == kind && c.DeprecatedType == typ })
		}
		if i < 0 {
			// A custom component of this type comes from a source.
			if slices.ContainsFunc(req.Custom, func(c Custom) bool { return string(c.Kind) == kind && c.Type == typ }) {
				continue
			}
			if !slices.Contains(unknown, w) {
				unknown = append(unknown, w)
			}
			continue
		}
		c := cat.Components[i]
		key := c.Kind + "/" + c.Type
		if selected[key] {
			continue
		}
		if c.Type != typ {
			warnings = append(warnings, fmt.Sprintf("%s is the deprecated name of %s; the manifest includes %s", w, key, key))
		}
		if c.Unsupported != "" {
			warnings = append(warnings, fmt.Sprintf("%s: %s", key, c.Unsupported))
		}
		selected[key] = true
	}
	var picked []versions.Component
	for _, c := range cat.Components {
		if selected[c.Kind+"/"+c.Type] {
			picked = append(picked, c)
		}
	}
	return write(cat, picked, "the selected components", unknown, warnings)
}

func kindList() string {
	names := make([]string, len(manifest.Kinds))
	for i, k := range manifest.Kinds {
		names[i] = string(k)
	}
	return strings.Join(names, ", ")
}

// ConfigComponents lists the component types a Collector configuration
// defines, as "<kind>/<type>" in configuration order: the Collector needs a
// factory for each, used in a pipeline or not.
func ConfigComponents(config []byte) ([]string, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(config)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration: not a YAML mapping")
	}
	var out []string
	top := doc.Content[0].Content
	for i := 0; i+1 < len(top); i += 2 {
		kind := top[i].Value
		if !slices.Contains(manifest.Kinds, manifest.Kind(kind)) || top[i+1].Kind != yaml.MappingNode {
			continue
		}
		ids := top[i+1].Content
		for j := 0; j+1 < len(ids); j += 2 {
			typ, _, _ := strings.Cut(ids[j].Value, "/")
			if w := kind + "/" + typ; typ != "" && !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	return out, nil
}

// write renders a manifest: the release's components, providers and
// replaces, in catalog order.
func write(cat *versions.Catalog, picked []versions.Component, what string, unknown, warnings []string) (*Result, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by linea from the Collector %s catalog: %s.\n", cat.CollectorVersion, what)
	fmt.Fprintf(&b, "dist:\n  name: linea-runtime\n  otelcol_version: %s\n", cat.CollectorVersion)
	for _, kind := range manifest.Kinds {
		first := true
		for _, c := range picked {
			if c.Kind != string(kind) {
				continue
			}
			if first {
				fmt.Fprintf(&b, "\n%s:\n", kind)
				first = false
			}
			fmt.Fprintf(&b, "  - gomod: %s\n", c.GoMod)
			if c.Import != "" {
				fmt.Fprintf(&b, "    import: %s\n", c.Import)
			}
		}
	}
	if len(cat.Providers) > 0 {
		b.WriteString("\nproviders:\n")
		for _, p := range cat.Providers {
			fmt.Fprintf(&b, "  - gomod: %s\n", p)
		}
	}
	if len(cat.Replaces) > 0 {
		b.WriteString("\nreplaces:\n")
		for _, r := range cat.Replaces {
			fmt.Fprintf(&b, "  - %s\n", r)
		}
	}
	raw := []byte(b.String())
	if _, err := manifest.Parse(raw); err != nil {
		return nil, fmt.Errorf("the written manifest does not parse: %w", err)
	}
	if unknown == nil {
		unknown = []string{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	return &Result{Manifest: raw, Unknown: unknown, Warnings: warnings}, nil
}
