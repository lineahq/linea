// Package manifest reads OpenTelemetry Collector Builder (OCB) manifests and
// generates the debugger's component registry from them.
// The built-in runtime's registry is generated from
// build/linea-runtime.yaml; runtimes built from a user's manifest are
// generated the same way. It imports nothing from this repository.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Manifest is the part of an OCB manifest the debugger reads. Fields that
// only steer OCB's own output are decoded so they can be reported, and
// unknown fields are accepted, as OCB itself accepts manifests from newer
// builders.
type Manifest struct {
	Dist       Dist     `yaml:"dist"`
	Receivers  []Module `yaml:"receivers"`
	Processors []Module `yaml:"processors"`
	Exporters  []Module `yaml:"exporters"`
	Connectors []Module `yaml:"connectors"`
	Extensions []Module `yaml:"extensions"`
	Providers  []Module `yaml:"providers"`
	Converters []Module `yaml:"converters"`
	Telemetry  *Module  `yaml:"telemetry"`
	Replaces   []string `yaml:"replaces"`
	Excludes   []string `yaml:"excludes"`
}

// Dist is the manifest's distribution section.
type Dist struct {
	Module           string `yaml:"module"`
	Name             string `yaml:"name"`
	Description      string `yaml:"description"`
	Version          string `yaml:"version"`
	OtelcolVersion   string `yaml:"otelcol_version"`
	OutputPath       string `yaml:"output_path"`
	DebugCompilation bool   `yaml:"debug_compilation"`
}

// Module is one manifest entry: a Go module with a version, optionally a
// package import path inside it, an import name and a local source path.
type Module struct {
	GoMod  string `yaml:"gomod"`
	Import string `yaml:"import"`
	Name   string `yaml:"name"`
	Path   string `yaml:"path"`
}

// Kind names a component list of the manifest.
type Kind string

// The component kinds, in the order the registry lists them.
const (
	Receivers  Kind = "receivers"
	Processors Kind = "processors"
	Exporters  Kind = "exporters"
	Connectors Kind = "connectors"
	Extensions Kind = "extensions"
)

// Kinds lists the component kinds in manifest order.
var Kinds = []Kind{Receivers, Processors, Exporters, Connectors, Extensions}

// Parse decodes an OCB manifest and checks that every component entry names
// a module and a version. A manifest without dist.otelcol_version gets the
// release its core components name.
func Parse(raw []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var errs []error
	for _, kind := range Kinds {
		for i, c := range m.Components(kind) {
			if err := c.validate(); err != nil {
				errs = append(errs, fmt.Errorf("manifest: %s[%d]: %w", kind, i, err))
			}
		}
	}
	if m.Telemetry != nil {
		if err := m.Telemetry.validate(); err != nil {
			errs = append(errs, fmt.Errorf("manifest: telemetry: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if m.Dist.OtelcolVersion == "" {
		v, err := m.deriveVersion()
		if err != nil {
			return nil, err
		}
		m.Dist.OtelcolVersion = v
	}
	if _, err := m.ParseReplaces(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Components returns the entries of one component list.
func (m *Manifest) Components(kind Kind) []Module {
	switch kind {
	case Receivers:
		return m.Receivers
	case Processors:
		return m.Processors
	case Exporters:
		return m.Exporters
	case Connectors:
		return m.Connectors
	case Extensions:
		return m.Extensions
	}
	return nil
}

// Modules lists every component module as "path version", in manifest
// order (receivers, processors, exporters, connectors, extensions).
func (m *Manifest) Modules() []string {
	var out []string
	for _, kind := range Kinds {
		for _, c := range m.Components(kind) {
			out = append(out, c.ModulePath()+" "+c.Version())
		}
	}
	return out
}

func (c Module) validate() error {
	mod, version, ok := strings.Cut(strings.TrimSpace(c.GoMod), " ")
	if !ok || mod == "" || strings.TrimSpace(version) == "" {
		return fmt.Errorf("gomod %q must be \"<module path> <version>\"", c.GoMod)
	}
	if c.Name != "" && !identifier.MatchString(c.Name) {
		return fmt.Errorf("name %q is not a Go identifier", c.Name)
	}
	return nil
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ModulePath is the module path of the gomod entry.
func (c Module) ModulePath() string {
	mod, _, _ := strings.Cut(strings.TrimSpace(c.GoMod), " ")
	return mod
}

// Version is the module version of the gomod entry.
func (c Module) Version() string {
	_, version, _ := strings.Cut(strings.TrimSpace(c.GoMod), " ")
	return strings.TrimSpace(version)
}

// ImportPath is the package to import: the explicit import, or the module
// path.
func (c Module) ImportPath() string {
	if c.Import != "" {
		return c.Import
	}
	return c.ModulePath()
}

// packageName is the import name OCB would use: the explicit name, or the
// last element of the import path, skipping a major-version suffix.
func (c Module) packageName() string {
	if c.Name != "" {
		return c.Name
	}
	p := c.ImportPath()
	base := path.Base(p)
	if majorSuffix.MatchString(base) {
		base = path.Base(path.Dir(p))
	}
	return sanitize(base)
}

var majorSuffix = regexp.MustCompile(`^v[0-9]+$`)

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "pkg" + out
	}
	return out
}
