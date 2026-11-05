package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/confmap"
)

// CodeReferenceStandIn marks a configuration reference that resolved to a
// stand-in string because its value was unavailable.
const CodeReferenceStandIn = "reference_stand_in"

// Reference sources: where a reference's value came from.
const (
	SourceScenario = "scenario"
	SourceHost     = "host"
	SourceDefault  = "default"
	SourceStandIn  = "stand_in"
)

// Reasons a reference resolved to a stand-in.
const (
	ReasonMissing           = "missing"
	ReasonUnreadable        = "unreadable"
	ReasonOutsideRoots      = "outside_roots"
	ReasonUnbound           = "unbound"
	ReasonUnsupportedScheme = "unsupported_scheme"
)

// Reference is one ${scheme:...} reference of a configuration and where its
// value came from. It never carries the value.
type Reference struct {
	Scheme string `json:"scheme"`
	// Target is the path, variable name or opaque URI part the reference
	// names.
	Target string `json:"target"`
	Source string `json:"source"`
	// Reason says why a stand-in was used.
	Reason string `json:"reason,omitempty"`
	// StandIn is the string that replaced an unavailable value.
	StandIn string `json:"stand_in,omitempty"`
	// Components are the configured components (kind/id, or "service")
	// whose settings contain the reference in the configuration itself;
	// empty for references nested in referenced content.
	Components []string `json:"components,omitempty"`
}

// standIn is the deterministic stand-in for an unavailable reference. It is
// derived from the URI because the Collector resolves references in map
// order, so a counter would differ between captures.
func standIn(uri string) string {
	sum := sha256.Sum256([]byte(uri))
	return "linea-stand-in-" + hex.EncodeToString(sum[:4])
}

func standInValue(uri, reason string) capturedValue {
	return capturedValue{Content: []byte(standIn(uri)), Binding: SourceStandIn, Reason: reason}
}

// fileReason classifies a failed ${file:} read.
func fileReason(err error) string {
	switch {
	case Refused(err):
		return ReasonOutsideRoots
	case errors.Is(err, fs.ErrNotExist):
		return ReasonMissing
	}
	return ReasonUnreadable
}

var unsupportedScheme = regexp.MustCompile(`scheme "([A-Za-z][A-Za-z0-9+.-]+)" is not supported for uri`)

// References lists the configuration's references (never the root),
// sorted by scheme and target.
func (s *Snapshot) References() []Reference {
	root := ""
	if len(s.data.URIs) == 1 {
		root = s.data.URIs[0]
	}
	where := referenceLocations(s.SourceBytes())
	var out []Reference
	for uri, value := range s.data.Values {
		if uri == root {
			continue
		}
		scheme, target, _ := strings.Cut(uri, ":")
		ref := Reference{Scheme: scheme, Target: target, Source: valueSource(scheme, value.Binding), Reason: value.Reason}
		if scheme == "env" {
			ref.Target, _, _ = strings.Cut(target, ":-")
		}
		if ref.Source == SourceStandIn {
			ref.StandIn = string(value.Content)
		}
		ref.Components = where("${" + uri + "}")
		out = append(out, ref)
	}
	slices.SortFunc(out, func(a, b Reference) int {
		return strings.Compare(a.Scheme+":"+a.Target, b.Scheme+":"+b.Target)
	})
	return out
}

func valueSource(scheme, binding string) string {
	switch binding {
	case SourceStandIn:
		return SourceStandIn
	case "declared", SourceScenario:
		return SourceScenario
	case "default":
		return SourceDefault
	}
	return SourceHost
}

// referenceLocations returns a lookup of the components whose settings, in
// the root configuration text, contain a reference token.
func referenceLocations(source []byte) func(token string) []string {
	type place struct {
		name    string
		strings []string
	}
	var places []place
	if r, err := confmap.NewRetrievedFromYAML(source); err == nil {
		if raw, err := r.AsRaw(); err == nil {
			top, _ := raw.(map[string]any)
			for _, s := range []struct{ key, kind string }{
				{"receivers", "receiver"}, {"processors", "processor"}, {"exporters", "exporter"},
				{"connectors", "connector"}, {"extensions", "extension"},
			} {
				section, _ := top[s.key].(map[string]any)
				for _, id := range slices.Sorted(maps.Keys(section)) {
					places = append(places, place{s.kind + "/" + id, leaves(section[id], nil)})
				}
			}
			if service, ok := top["service"]; ok {
				places = append(places, place{"service", leaves(service, nil)})
			}
		}
	}
	return func(token string) []string {
		var names []string
		for _, p := range places {
			if slices.ContainsFunc(p.strings, func(s string) bool { return strings.Contains(s, token) }) {
				names = append(names, p.name)
			}
		}
		return names
	}
}

func leaves(v any, out []string) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case map[string]any:
		for k, x := range v {
			out = leaves(x, append(out, k))
		}
	case []any:
		for _, x := range v {
			out = leaves(x, out)
		}
	}
	return out
}

// StandInDiagnostics warns once per reference that resolved to a stand-in.
func StandInDiagnostics(refs []Reference) []Diagnostic {
	var out []Diagnostic
	for _, r := range refs {
		if r.Source != SourceStandIn {
			continue
		}
		d := Diagnostic{
			Severity: SeverityWarning,
			Code:     CodeReferenceStandIn,
			Message:  fmt.Sprintf("${%s:%s} is unavailable (%s); the configuration uses the stand-in string %q in its place", r.Scheme, r.Target, r.Reason, r.StandIn),
			Hint:     standInRemedy(r),
		}
		if len(r.Components) > 0 {
			d.Component = r.Components[0]
		}
		out = append(out, d)
	}
	return out
}

func standInRemedy(r Reference) string {
	switch r.Reason {
	case ReasonMissing:
		return "the file does not exist on this host; add it to the scenario's mocks.files with path " + r.Target + " to supply its content"
	case ReasonUnreadable:
		return "the file could not be read; add it to the scenario's mocks.files with path " + r.Target + " to supply its content"
	case ReasonOutsideRoots:
		// A team server reads no host file; its requests carry the content.
		return "the server reads no host file; add it to the scenario's mocks.files with path " + r.Target + " to supply its content"
	case ReasonUnbound:
		// A team server reads no host environment variable; a local command
		// reads the ones it imports.
		return "declare " + r.Target + " under the scenario's environment (a local command can instead import it from the host with --import-env " + r.Target + ")"
	case ReasonUnsupportedScheme:
		return "the debugger distribution has no " + r.Scheme + " provider; nothing is fetched"
	}
	return ""
}

// withStandInHint turns a resolution error into a diagnostic whose hint names
// the stand-ins that may have caused it: those whose string the error quotes,
// or all of them. Errors without stand-ins are returned unchanged.
func withStandInHint(err error, refs []Reference) error {
	var de *DiagnosticsError
	if err == nil || errors.As(err, &de) {
		return err
	}
	var all, quoted []string
	for _, r := range refs {
		if r.Source != SourceStandIn {
			continue
		}
		name := fmt.Sprintf("${%s:%s} (%s)", r.Scheme, r.Target, r.Reason)
		all = append(all, name)
		if strings.Contains(err.Error(), r.StandIn) {
			quoted = append(quoted, name)
		}
	}
	if len(all) == 0 {
		return err
	}
	if len(quoted) > 0 {
		all = quoted
	}
	return &DiagnosticsError{Diagnostics: []Diagnostic{{
		Severity: SeverityError,
		Code:     CodeInvalidConfiguration,
		Message:  err.Error(),
		Hint:     "unavailable references were replaced by stand-in strings, which this setting cannot use: " + strings.Join(all, ", ") + "; supply their values in the scenario (environment, mocks.files)",
	}}}
}
