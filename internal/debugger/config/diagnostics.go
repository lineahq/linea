package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/otelcol"
)

// Severity of a Diagnostic.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Diagnostic codes.
const (
	CodeUnsupportedComponent = "unsupported_component"
	CodeInvalidConfiguration = "invalid_configuration"
)

// Diagnostic is one machine-readable finding about a configuration.
type Diagnostic struct {
	Severity  Severity `json:"severity"`
	Code      string   `json:"code"`
	Component string   `json:"component,omitempty"`
	Message   string   `json:"message"`
	Hint      string   `json:"hint,omitempty"`
	// ProviderSettings are settings a module provider proposes to resolve
	// the diagnostic.
	ProviderSettings *ProposedSettings `json:"provider_settings,omitempty"`
}

// ProposedSettings are a module provider's proposed settings for the
// scenario's mocks.providers.<Provider>, a JSON object.
type ProposedSettings struct {
	Provider string          `json:"provider"`
	Settings json.RawMessage `json:"settings"`
}

// DiagnosticsError is returned when a configuration cannot be used. It keeps
// the individual diagnostics for structured reporting.
type DiagnosticsError struct {
	Diagnostics []Diagnostic
}

func (e *DiagnosticsError) Error() string {
	msgs := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		msgs[i] = d.Message
		if d.Hint != "" {
			msgs[i] += " (" + d.Hint + ")"
		}
	}
	return strings.Join(msgs, "; ")
}

// AsDiagnostics converts err into diagnostics: the structured ones of a
// DiagnosticsError, or a single invalid_configuration diagnostic.
func AsDiagnostics(err error) []Diagnostic {
	var de *DiagnosticsError
	if errors.As(err, &de) {
		return de.Diagnostics
	}
	return []Diagnostic{{Severity: SeverityError, Code: CodeInvalidConfiguration, Message: err.Error()}}
}

// sections maps top-level config keys to the factories that serve them.
func sections(f otelcol.Factories) []struct {
	key   string
	kind  string
	types []component.Type
} {
	return []struct {
		key   string
		kind  string
		types []component.Type
	}{
		{"receivers", "receiver", slices.Collect(maps.Keys(f.Receivers))},
		{"processors", "processor", slices.Collect(maps.Keys(f.Processors))},
		{"exporters", "exporter", slices.Collect(maps.Keys(f.Exporters))},
		{"connectors", "connector", slices.Collect(maps.Keys(f.Connectors))},
		{"extensions", "extension", slices.Collect(maps.Keys(f.Extensions))},
	}
}

// checkComponents warns about every configured component whose type the
// distribution does not contain, with the supported alternatives, and
// returns the configured IDs per section. Such a component runs as a
// stand-in; a mistyped name is caught by the same warning.
func checkComponents(conf *confmap.Conf, f otelcol.Factories) ([]Diagnostic, map[string][]component.ID) {
	var out []Diagnostic
	ids := map[string][]component.ID{}
	for _, s := range sections(f) {
		sub, err := conf.Sub(s.key)
		if err != nil {
			continue // a malformed section is reported by the Collector itself
		}
		for _, key := range slices.Sorted(maps.Keys(sub.ToStringMap())) {
			var id component.ID
			if err := id.UnmarshalText([]byte(key)); err != nil {
				continue // likewise reported by the Collector
			}
			ids[s.key] = append(ids[s.key], id)
			if slices.Contains(s.types, id.Type()) {
				continue
			}
			names := make([]string, len(s.types))
			for i, t := range s.types {
				names[i] = t.String()
			}
			slices.Sort(names)
			hint := "the distribution has no " + s.key
			if len(names) > 0 {
				hint = "supported " + s.key + ": " + strings.Join(names, ", ")
			}
			if s.kind == "processor" || s.kind == "connector" {
				hint += "; assertions on data that passes this stand-in are inconclusive"
			}
			out = append(out, Diagnostic{
				Severity:  SeverityWarning,
				Code:      CodeUnsupportedComponent,
				Component: s.kind + "/" + id.String(),
				Message:   fmt.Sprintf("%s type %q is not part of the debugger distribution; it runs as a stand-in that %s", s.kind, id.Type(), standInBehavior[s.kind]),
				Hint:      hint,
			})
		}
	}
	return out, ids
}

// standInBehavior says what each kind of stand-in does.
var standInBehavior = map[string]string{
	"receiver":  "receives the scenario's inputs",
	"processor": "passes data through unchanged",
	"exporter":  "records data like a captured exporter",
	"connector": "forwards data of its own signal and emits nothing into other signals",
	"extension": "does nothing, like every extension in a run",
}

func resolveRawSettings(ctx context.Context, settings otelcol.ConfigProviderSettings) (*confmap.Conf, error) {
	resolver, err := confmap.NewResolver(settings.ResolverSettings)
	if err != nil {
		return nil, fmt.Errorf("create config resolver: %w", err)
	}
	defer resolver.Shutdown(ctx) //nolint:errcheck
	conf, err := resolver.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve config: %w", err)
	}
	return conf, nil
}
