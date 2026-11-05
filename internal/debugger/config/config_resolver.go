// Package config resolves an unchanged user Collector configuration through
// the Collector's own confmap resolver and validation.
package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/standin"
)

// Resolved is a validated Collector configuration and its provider inputs.
// Only a non-nil Snapshot pins those inputs; live URIs/Env can change on reread.
type Resolved struct {
	URIs     []string
	Env      map[string]string
	Config   *otelcol.Config
	Snapshot *Snapshot
	// References are the configuration's references and where each value
	// came from; set for captured and snapshot resolutions.
	References []Reference
	// Warnings are findings that do not stop resolution: one per component
	// that runs as a stand-in.
	Warnings []Diagnostic
}

// ProviderSettings preserves the retrieval source used for validation, including
// when reference execution layers its private yaml overrides onto the root URI.
func (r *Resolved) ProviderSettings(uris []string) otelcol.ConfigProviderSettings {
	if r.Snapshot != nil {
		return r.Snapshot.ProviderSettings(uris)
	}
	return ProviderSettings(uris, r.Env)
}

// ProviderSettings returns the confmap providers shipped by the OCB manifest
// for the given URIs. env supplies scenario values for ${env:NAME}
// references; it may be nil.
func ProviderSettings(uris []string, env map[string]string) otelcol.ConfigProviderSettings {
	return otelcol.ConfigProviderSettings{
		ResolverSettings: confmap.ResolverSettings{
			URIs: uris,
			ProviderFactories: []confmap.ProviderFactory{
				scenarioEnvFactory(env),
				fileprovider.NewFactory(),
				yamlprovider.NewFactory(),
			},
		},
	}
}

// FileURI returns the file provider URI for a config path.
func FileURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return "file:" + abs, nil
}

// Source is a Collector configuration given either by file path or as YAML
// text (for example from an editor). Exactly one field is set.
type Source struct {
	Path string
	YAML string
}

// URI returns the confmap provider URI for the source: the file provider for
// a path, the upstream yaml provider for text. Both are shipped by the OCB
// manifest, so the Collector resolves either exactly as it would at startup.
func (s Source) URI() (string, error) {
	switch {
	case s.Path != "" && s.YAML != "":
		return "", errors.New("config: give a path or YAML text, not both")
	case s.Path != "":
		return FileURI(s.Path)
	case strings.TrimSpace(s.YAML) != "":
		return "yaml:" + s.YAML, nil
	}
	return "", errors.New("config: a path or YAML text is required")
}

// Bytes returns the configuration as given: the file content or the text.
func (s Source) Bytes() ([]byte, error) {
	if s.Path != "" {
		return os.ReadFile(s.Path)
	}
	return []byte(s.YAML), nil
}

// BytesWith is Bytes reading a path through the path policy.
func (s Source) BytesWith(paths hostfs.Policy) ([]byte, error) {
	if s.Path != "" && !paths.Direct() {
		return paths.ReadFile(s.Path, 0)
	}
	return s.Bytes()
}

// Resolve loads the config at path, unmarshals it with the given factories and
// validates it with the same confmap.Validate call the Collector uses at
// startup. The file is only read.
func Resolve(ctx context.Context, path string, factories otelcol.Factories, env map[string]string) (*Resolved, error) {
	return ResolveSource(ctx, Source{Path: path}, factories, env)
}

// ResolveSource is Resolve for a path or YAML text.
func ResolveSource(ctx context.Context, src Source, factories otelcol.Factories, env map[string]string) (*Resolved, error) {
	uri, err := src.URI()
	if err != nil {
		return nil, err
	}
	return ResolveURIs(ctx, []string{uri}, factories, env)
}

// ResolveURIs is Resolve for an explicit list of provider URIs. Component
// types missing from factories are reported together as a DiagnosticsError
// before the Collector unmarshals the configuration.
func ResolveURIs(ctx context.Context, uris []string, factories otelcol.Factories, env map[string]string) (*Resolved, error) {
	resolved, err := resolveSettings(ctx, ProviderSettings(uris, env), factories)
	if err == nil {
		resolved.Env = env
	}
	return resolved, err
}

func resolveSettings(ctx context.Context, settings otelcol.ConfigProviderSettings, factories otelcol.Factories) (*Resolved, error) {
	raw, err := resolveRawSettings(ctx, settings)
	if err != nil {
		return nil, err
	}
	warnings, ids := checkComponents(raw, factories)
	factories = standin.Add(factories, standin.Find(factories, ids))

	provider, err := otelcol.NewConfigProvider(settings)
	if err != nil {
		return nil, fmt.Errorf("create config provider: %w", err)
	}
	defer provider.Shutdown(ctx) //nolint:errcheck

	cfg, err := provider.Get(ctx, factories)
	if err != nil {
		return nil, fmt.Errorf("resolve config: %w", err)
	}
	if err := confmap.Validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return &Resolved{URIs: append([]string(nil), settings.ResolverSettings.URIs...), Config: cfg, Warnings: warnings}, nil
}

// Effective returns the resolved configuration as the Collector interprets
// it, including defaults, with sensitive (configopaque) values redacted the
// way "otelcol print-config" does.
func Effective(cfg *otelcol.Config) (map[string]any, error) {
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return nil, fmt.Errorf("marshal effective config: %w", err)
	}
	return conf.ToStringMap(), nil
}
