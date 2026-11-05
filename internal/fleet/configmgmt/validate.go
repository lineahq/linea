// Package configmgmt owns optional Collector configuration management:
// version validation, the layered plan an agent should run, and the decision
// whether to offer it. Reporting never calls into this package; only the
// reconcile path does, and only while config management is enabled.
package configmgmt

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Bounds keep one RemoteConfig within the agent message limit (1 MiB).
const (
	MaxBodyBytes   = 128 * 1024
	MaxLayers      = 8
	MaxAssignments = 1000
	MaxLayer       = 99
	maxDepth       = 32
	maxNodes       = 20000
)

// ValidationError names where a version body is rejected. It never carries
// the offending value, so it is safe to return and log.
type ValidationError struct {
	Path   string
	Reason string
	Err    error
}

func (e *ValidationError) Unwrap() error { return e.Err }

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Reason
	}
	return e.Path + ": " + e.Reason
}

var sections = map[string]string{"receivers": "receiver", "processors": "processor", "exporters": "exporter", "connectors": "connector", "extensions": "extension"}

var componentType = regexp.MustCompile(`^[a-zA-Z][0-9a-zA-Z_]{0,62}$`)

// Validate checks a version body before it is stored: bounded YAML whose
// top-level keys are Collector sections, well-formed component IDs, no inline
// secrets, and component types within allowed when it is non-empty. Whether
// the configuration works is decided by the supervisor on the host.
func Validate(body []byte, allowed []string) error {
	if len(body) > MaxBodyBytes {
		return &ValidationError{Reason: fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes)}
	}
	if !utf8.Valid(body) {
		return &ValidationError{Reason: "body is not UTF-8"}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return &ValidationError{Reason: "body is not valid YAML"}
	}
	if len(doc.Content) == 0 {
		return &ValidationError{Reason: "body is empty; use {} for an empty layer"}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return &ValidationError{Reason: "body must be a mapping"}
	}
	nodes := 0
	if err := bounded(root, 0, &nodes); err != nil {
		return err
	}
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i].Value, root.Content[i+1]
		kind, isSection := sections[key]
		switch {
		case isSection:
			if err := checkSection(key, kind, value, allowed); err != nil {
				return err
			}
		case key == "service":
			if value.Kind != yaml.MappingNode && !null(value) {
				return &ValidationError{Path: key, Reason: "must be a mapping"}
			}
		default:
			return &ValidationError{Path: key, Reason: "unknown top-level key"}
		}
	}
	return secrets(root, "", "")
}

// bounded refuses aliases and deep or very large trees before any walk.
func bounded(n *yaml.Node, depth int, count *int) error {
	*count++
	if depth > maxDepth || *count > maxNodes {
		return &ValidationError{Reason: "body is nested too deeply or too large"}
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return &ValidationError{Reason: "YAML anchors and aliases are not supported"}
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Kind != yaml.ScalarNode {
				return &ValidationError{Reason: "mapping keys must be scalars"}
			}
		}
	}
	for _, c := range n.Content {
		if err := bounded(c, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func null(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.Tag == "!!null" }

func checkSection(key, kind string, n *yaml.Node, allowed []string) error {
	if null(n) {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return &ValidationError{Path: key, Reason: "must be a mapping"}
	}
	for i := 0; i < len(n.Content); i += 2 {
		id := n.Content[i].Value
		typ, name, named := strings.Cut(id, "/")
		if !componentType.MatchString(typ) || named && !validName(name) {
			return &ValidationError{Path: key, Reason: "invalid component ID"}
		}
		if len(allowed) > 0 && !slices.Contains(allowed, kind+":"+typ) {
			return &ValidationError{Path: key + "." + id, Reason: "component type is not allowed by policy"}
		}
	}
	return nil
}

func validName(name string) bool {
	if name == "" || len(name) > 1024 {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || r == '/' {
			return false
		}
	}
	return true
}

// ValidPolicy reports whether every allowlist entry is kind:type.
func ValidPolicy(allowed []string) bool {
	for _, a := range allowed {
		kind, typ, ok := strings.Cut(a, ":")
		known := false
		for _, k := range sections {
			known = known || k == kind
		}
		if !ok || !known || !componentType.MatchString(typ) {
			return false
		}
	}
	return true
}

var (
	secretKeys = map[string]bool{
		"password": true, "passwd": true, "token": true, "bearer_token": true, "access_token": true, "auth_token": true,
		"api_key": true, "apikey": true, "secret": true, "client_secret": true, "private_key": true, "key_pem": true,
		"authorization": true, "access_key": true, "secret_key": true,
	}
	secretSuffixes = []string{"_password", "_token", "_secret", "_api_key", "_apikey"}
	reference      = regexp.MustCompile(`\$\{(env|file):[^}]+\}`)
	scheme         = regexp.MustCompile(`(?i)^(bearer|basic)\s+`)
	userinfo       = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://([^/@\s]*)@`)
)

// ErrInlineSecret is wrapped by secret rejections.
var ErrInlineSecret = errors.New("inline secret")

func secretKey(key string) bool {
	key = strings.ToLower(key)
	if secretKeys[key] {
		return true
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(key, s) {
			return true
		}
	}
	return false
}

// referenced reports whether a value holds nothing but ${env:…} or ${file:…}
// references, optionally after an HTTP auth scheme.
func referenced(v string) bool {
	rest := strings.TrimSpace(scheme.ReplaceAllString(strings.TrimSpace(v), ""))
	return rest == "" || strings.TrimSpace(reference.ReplaceAllString(rest, "")) == ""
}

// password reports whether URL userinfo carries a literal password once
// ${env:…} and ${file:…} references are removed.
func password(info string) bool {
	_, pass, ok := strings.Cut(reference.ReplaceAllString(info, ""), ":")
	return ok && pass != ""
}

// secrets rejects literal values under secret-like keys, any literal header
// value, and passwords in URL userinfo. It is a guard against mistakes, not a
// scanner.
func secrets(n *yaml.Node, path, parent string) error {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i].Value, n.Content[i+1]
			p := key
			if path != "" {
				p = path + "." + key
			}
			if value.Kind == yaml.ScalarNode && !null(value) {
				if (secretKey(key) || strings.EqualFold(parent, "headers")) && !referenced(value.Value) {
					return &ValidationError{Path: p, Reason: "literal secret; use ${env:NAME} or ${file:/path}", Err: ErrInlineSecret}
				}
			}
			if err := secrets(value, p, key); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			if err := secrets(c, fmt.Sprintf("%s[%d]", path, i), parent); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if m := userinfo.FindStringSubmatch(n.Value); m != nil && password(m[1]) {
			return &ValidationError{Path: path, Reason: "password in URL; use ${env:NAME}", Err: ErrInlineSecret}
		}
	}
	return nil
}
