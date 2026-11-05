package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/confmap"
)

var ErrEnvironmentBinding = errors.New("environment variable requires an explicit binding")
var ErrEnvironmentPolicy = errors.New("invalid environment capture policy")

// EnvironmentPolicy is trusted application policy, never a transport request.
// The zero value reads only declared values and URI defaults. Host names are an
// explicit import allowlist; LegacyHost preserves existing local adapter behavior.
// This controls confmap env retrievals, not component runtime environment reads.
type EnvironmentPolicy struct {
	Host       []string `json:"host,omitempty"`
	LegacyHost bool     `json:"legacy_host,omitempty"`
}

var environmentName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (p EnvironmentPolicy) Clone() EnvironmentPolicy {
	p.Host = slices.Clone(p.Host)
	slices.Sort(p.Host)
	p.Host = slices.Compact(p.Host)
	return p
}

func (p EnvironmentPolicy) Validate() error {
	if len(p.Host) > 128 || p.LegacyHost && len(p.Host) != 0 {
		return ErrEnvironmentPolicy
	}
	for _, name := range p.Host {
		if len(name) > 256 || !environmentName.MatchString(name) {
			return ErrEnvironmentPolicy
		}
	}
	return nil
}

func (p EnvironmentPolicy) allows(name string) bool {
	return p.LegacyHost || slices.Contains(p.Host, name)
}

func (p EnvironmentPolicy) Mode() string {
	if p.LegacyHost {
		return "legacy_host"
	}
	if len(p.Host) != 0 {
		return "allowlisted_host"
	}
	return "declared_only"
}

// boundEnvironment captures a single retrieval without ever changing process
// environment. Preserve v1.68.0 envprovider YAML/default/empty/unset semantics;
// explicit scenario/setup empty strings retain their existing override behavior.
func boundEnvironment(ctx context.Context, uri string, declared map[string]string, policy EnvironmentPolicy) (*confmap.Retrieved, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(uri, "env:") {
		return nil, "", ErrEnvironmentBinding
	}
	name, fallback, hasDefault := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
	if !environmentName.MatchString(name) {
		return nil, "", ErrEnvironmentBinding
	}
	value, exists := declared[name]
	origin := "declared"
	emptyString := exists && value == ""
	if !exists {
		if policy.allows(name) {
			value, exists = os.LookupEnv(name)
			origin = "host"
		}
		if !exists {
			if hasDefault {
				value, origin, emptyString = fallback, "default", fallback == ""
			} else if policy.allows(name) {
				origin = "host_unset"
			} else {
				return nil, "", fmt.Errorf("%w: %s; declare it in environment or authorize its host import", ErrEnvironmentBinding, name)
			}
		}
	}
	if emptyString {
		r, err := confmap.NewRetrieved("")
		return r, origin, err
	}
	r, err := confmap.NewRetrievedFromYAML([]byte(value))
	return r, origin, err
}

// CheckEnvironment enforces an admitting service's policy on an existing
// capture, including a decoded/prepared request. It never rereads the host.
// Schema-1 env origins are unknown and require explicit host authorization.
func (s *Snapshot) CheckEnvironment(policy EnvironmentPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	for uri, value := range s.data.Values {
		if !strings.HasPrefix(uri, "env:") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
		// Stand-ins read nothing from the host.
		if value.Binding == "declared" || value.Binding == "default" || value.Binding == SourceStandIn {
			continue
		}
		if !policy.allows(name) {
			return fmt.Errorf("%w: %s; captured host or legacy binding is not authorized", ErrEnvironmentBinding, name)
		}
	}
	return nil
}

// EnvironmentMode reports policy without publishing names, URIs or values.
// Legacy snapshots omit it so historical run metadata remains byte compatible.
func (s *Snapshot) EnvironmentMode() string {
	if s.data.Environment == nil {
		return ""
	}
	return s.data.Environment.Mode()
}

// EnvironmentValues returns the environment values the configuration read,
// declared or from the host, for redaction. Defaults written in the
// configuration and stand-ins are not environment values.
func (s *Snapshot) EnvironmentValues() []string {
	var values []string
	for uri, value := range s.data.Values {
		if !strings.HasPrefix(uri, "env:") || value.Binding == "default" || value.Binding == SourceStandIn || len(value.Content) == 0 {
			continue
		}
		values = append(values, string(value.Content))
	}
	return values
}
