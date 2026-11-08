package scenario

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Defaults holds what every input of the scenario shares unless an input
// declares its own.
type Defaults struct {
	// Headers are sent with every input; an input's own headers replace a
	// default of the same name.
	Headers Headers `yaml:"headers"`
}

// Headers are the request headers an input arrives with (HTTP headers, or
// gRPC metadata), as a client sends them. A value is one string or a list
// of strings for a repeated header. Names are matched case-insensitively,
// as the Collector does. A receiver hands them to the pipeline only with
// include_metadata: true.
type Headers map[string]HeaderValues

// HeaderValues are the values of one header.
type HeaderValues []string

// UnmarshalYAML accepts a string or a list of strings.
func (v *HeaderValues) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*v = HeaderValues{s}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return fmt.Errorf("a header value is a string or a list of strings")
	}
	*v = list
	return nil
}

func (h Headers) validate() error {
	seen := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(h)) {
		if !validHeaderName(name) {
			return fmt.Errorf("header %q is not a valid header name", name)
		}
		low := strings.ToLower(name)
		if prev, ok := seen[low]; ok {
			return fmt.Errorf("headers %q and %q are the same header", prev, name)
		}
		seen[low] = name
		if len(h[name]) == 0 {
			return fmt.Errorf("header %q has no value", name)
		}
	}
	return nil
}

// validHeaderName reports whether s is an HTTP field name (RFC 9110 token)
// that is also usable as gRPC metadata: not a pseudo-header and not binary.
func validHeaderName(s string) bool {
	if s == "" || strings.HasSuffix(strings.ToLower(s), "-bin") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// mergeHeaders returns the defaults overridden by an input's own headers,
// with lowercased names, or nil when there are none.
func mergeHeaders(defaults, own Headers) map[string][]string {
	if len(defaults) == 0 && len(own) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, h := range []Headers{defaults, own} {
		for name, v := range h {
			out[strings.ToLower(name)] = slices.Clone(v)
		}
	}
	return out
}
