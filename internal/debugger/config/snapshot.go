package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/yamlprovider"
	"go.opentelemetry.io/collector/otelcol"

	"github.com/lineahq/linea/internal/debugger/hostfs"
)

// snapshotSchema 4 adds provider overrides (WithOverrides); a snapshot
// without them is still written as schema 3. Schema 3 adds scenario files
// and stand-ins; 1 to 3 stay readable.
const snapshotSchema = 4

// capturedSchema is the schema of new captures, which have no overrides.
const capturedSchema = 3

// ErrOverride rejects a provider override that is not a yaml: URI.
var ErrOverride = errors.New("invalid configuration override")

var ErrCaptureLimit = errors.New("configuration capture limit exceeded")
var ErrUncaptured = errors.New("configuration dependency was not captured")
var ErrSnapshot = errors.New("invalid configuration snapshot")
var ErrSnapshotSchema = errors.New("unsupported configuration snapshot schema")

// CaptureLimits bound retrieval bytes and unique file/env URIs. These are initial
// materialization limits, independent of Collector capture and query budgets.
type CaptureLimits struct {
	Bytes        int64
	Dependencies int
}

func (l CaptureLimits) defaults() CaptureLimits {
	if l.Bytes <= 0 {
		l.Bytes = 16 << 20
	}
	if l.Dependencies <= 0 {
		l.Dependencies = 128
	}
	return l
}

type capturedValue struct {
	Content []byte `json:"content_base64"`
	Binding string `json:"binding,omitempty"`
	// Empty override/default strings differ from nil YAML values upstream.
	EmptyString bool `json:"empty_string,omitempty"`
	// Reason says why a stand_in binding replaced the value (schema 3).
	Reason string `json:"reason,omitempty"`
}

type snapshotData struct {
	Schema      int                      `json:"schema"`
	URIs        []string                 `json:"uris"`
	Values      map[string]capturedValue `json:"values"`
	Environment *EnvironmentPolicy       `json:"environment_policy,omitempty"`
}

// Snapshot seals exact provider responses, not the redacted effective config.
// It is immutable after creation/decoding. Its JSON contains private source and
// environment values and must not be included in ordinary run metadata.
type Snapshot struct{ data snapshotData }

// RequiredEnvironment returns names whose captured binding requires host
// authorization, including legacy snapshots with unknown origins. It returns
// no values, defaults or provider URIs and never consults the host.
func (s *Snapshot) RequiredEnvironment() []string {
	names := []string{}
	for uri, value := range s.data.Values {
		if !strings.HasPrefix(uri, "env:") || value.Binding == "declared" || value.Binding == "default" || value.Binding == SourceStandIn {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
		names = append(names, name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func (s *Snapshot) MarshalJSON() ([]byte, error) { return json.Marshal(s.data) }

// DecodeSnapshot verifies a private serialized snapshot. Returning a new object
// prevents clients replacing the contents of a snapshot already used by a run.
func DecodeSnapshot(raw []byte, limits CaptureLimits) (*Snapshot, error) {
	limits = limits.defaults()
	// Generous encoded bound covers base64, escaped URI keys and the root URI
	// repeated in URIs/Values. Divide first to avoid overflow in custom limits.
	// The decoded byte/count budgets below remain exact.
	if int64(len(raw))/16-4096 > limits.Bytes {
		return nil, ErrCaptureLimit
	}
	var version struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSnapshot, err)
	}
	if version.Schema < 1 || version.Schema > snapshotSchema {
		return nil, fmt.Errorf("%w: %w: %d", ErrSnapshot, ErrSnapshotSchema, version.Schema)
	}
	var data snapshotData
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&data); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSnapshot, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, ErrSnapshot
	}
	if len(data.URIs) == 0 || data.Values == nil || len(data.URIs) > 1 && data.Schema < 4 || len(data.URIs) == 1 && data.Schema == 4 {
		return nil, ErrSnapshot
	}
	for _, uri := range data.URIs[1:] {
		if !strings.HasPrefix(uri, "yaml:") {
			return nil, ErrSnapshot
		}
	}
	if data.Schema == 1 && data.Environment != nil || data.Schema > 1 && data.Environment == nil {
		return nil, ErrSnapshot
	}
	if data.Environment != nil {
		if err := data.Environment.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSnapshot, err)
		}
	}
	if len(data.Values) > limits.Dependencies {
		return nil, ErrCaptureLimit
	}
	var used int64
	for uri, value := range data.Values {
		if !validValue(data, uri, value) {
			return nil, ErrSnapshot
		}
		n := int64(len(uri)) + int64(len(value.Content))
		if n > limits.Bytes-used {
			return nil, ErrCaptureLimit
		}
		used += n
	}
	root := data.URIs[0]
	if !strings.HasPrefix(root, "yaml:") && !strings.HasPrefix(root, "file:") {
		return nil, ErrSnapshot
	}
	if strings.HasPrefix(root, "yaml:") && int64(len(root)) > limits.Bytes-used {
		return nil, ErrCaptureLimit
	}
	for _, uri := range data.URIs[1:] {
		if int64(len(uri)) > limits.Bytes-used {
			return nil, ErrCaptureLimit
		}
		used += int64(len(uri))
	}
	if strings.HasPrefix(root, "file:") {
		if _, ok := data.Values[root]; !ok {
			return nil, ErrSnapshot
		}
	}
	return &Snapshot{data: data}, nil
}

// validValue checks one captured value against its schema: schema 1 has no
// bindings, schema 2 binds env values, schema 3 adds scenario files and
// stand-ins, the only values other schemes can have.
func validValue(data snapshotData, uri string, value capturedValue) bool {
	scheme, _, ok := strings.Cut(uri, ":")
	if !ok || !providerScheme.MatchString(scheme) || value.EmptyString && len(value.Content) != 0 {
		return false
	}
	if value.Binding == SourceStandIn {
		if data.Schema < 3 || value.EmptyString || string(value.Content) != standIn(uri) {
			return false
		}
		switch scheme {
		case "file":
			return value.Reason == ReasonMissing || value.Reason == ReasonUnreadable || value.Reason == ReasonOutsideRoots
		case "env":
			name, _, _ := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
			return environmentName.MatchString(name) && value.Reason == ReasonUnbound
		case "yaml":
			return false
		}
		return value.Reason == ReasonUnsupportedScheme
	}
	if value.Reason != "" {
		return false
	}
	switch scheme {
	case "file":
		return value.Binding == "" || data.Schema >= 3 && value.Binding == SourceScenario
	case "env":
		if data.Schema == 1 {
			return value.Binding == ""
		}
		name, _, hasDefault := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
		return environmentName.MatchString(name) && (value.Binding == "declared" || value.Binding == "default" || value.Binding == "host" || value.Binding == "host_unset") &&
			(value.Binding != "default" || hasDefault) &&
			(value.Binding != "host" && value.Binding != "host_unset" || data.Environment.allows(name))
	}
	return false
}

var providerScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]+$`)

// SourceBytes returns a fresh copy of the original root config, without rereading
// the host path. The root URI remains logical source identity, never a temp path.
func (s *Snapshot) SourceBytes() []byte {
	if len(s.data.URIs) == 0 {
		return nil
	}
	uri := s.data.URIs[0]
	if strings.HasPrefix(uri, "yaml:") {
		return []byte(strings.TrimPrefix(uri, "yaml:"))
	}
	return bytes.Clone(s.data.Values[uri].Content)
}

// WithOverrides returns a copy of the snapshot whose resolution layers the
// given yaml: URIs after the root, in order, as confmap merges later URIs
// over earlier ones (a list value replaces the list). The root and every
// captured value stay as they are: the user's configuration is unchanged,
// and an override that references a file or environment value not captured
// fails on retrieval. No overrides returns s.
func (s *Snapshot) WithOverrides(overrides []string) (*Snapshot, error) {
	if len(overrides) == 0 {
		return s, nil
	}
	for _, uri := range overrides {
		if !strings.HasPrefix(uri, "yaml:") || len(uri) == len("yaml:") {
			return nil, fmt.Errorf("%w: %q is not a yaml: URI", ErrOverride, uri)
		}
	}
	data := s.data
	data.Schema = snapshotSchema
	data.URIs = append(slices.Clone(s.data.URIs), overrides...)
	return &Snapshot{data: data}, nil
}

// Overrides are the yaml: URIs layered after the root (schema 4), nil for a
// captured configuration.
func (s *Snapshot) Overrides() []string {
	if len(s.data.URIs) < 2 {
		return nil
	}
	return slices.Clone(s.data.URIs[1:])
}

func (s *Snapshot) Resolve(ctx context.Context, factories otelcol.Factories) (*Resolved, error) {
	resolved, err := resolveSettings(ctx, s.ProviderSettings(s.data.URIs), factories)
	if err != nil {
		return nil, withStandInHint(err, s.References())
	}
	resolved.Snapshot, resolved.References = s, s.References()
	return resolved, nil
}

// ProviderSettings replays file/env retrievals from the sealed map. yaml URIs
// are intrinsically immutable, including reference-run isolation overrides;
// any new file/env reference inside one still fails closed on retrieval.
func (s *Snapshot) ProviderSettings(uris []string) otelcol.ConfigProviderSettings {
	settings := ProviderSettings(append([]string(nil), uris...), nil)
	settings.ResolverSettings.ProviderFactories = []confmap.ProviderFactory{
		frozenFactory("file", s.data.Values), frozenFactory("env", s.data.Values), yamlprovider.NewFactory(),
	}
	// Schemes without a provider replay their captured stand-ins.
	var extra []string
	for uri := range s.data.Values {
		if scheme, _, _ := strings.Cut(uri, ":"); scheme != "file" && scheme != "env" && scheme != "yaml" {
			extra = append(extra, scheme)
		}
	}
	slices.Sort(extra)
	for _, scheme := range slices.Compact(extra) {
		settings.ResolverSettings.ProviderFactories = append(settings.ResolverSettings.ProviderFactories, frozenFactory(scheme, s.data.Values))
	}
	return settings
}

func retrieved(v capturedValue) (*confmap.Retrieved, error) {
	if v.EmptyString {
		return confmap.NewRetrieved("")
	}
	return confmap.NewRetrievedFromYAML(v.Content)
}

func frozenFactory(scheme string, values map[string]capturedValue) confmap.ProviderFactory {
	return confmap.NewProviderFactory(func(confmap.ProviderSettings) confmap.Provider {
		return &frozenProvider{scheme: scheme, values: values}
	})
}

type frozenProvider struct {
	scheme string
	values map[string]capturedValue
}

func (p *frozenProvider) Scheme() string               { return p.scheme }
func (*frozenProvider) Shutdown(context.Context) error { return nil }
func (p *frozenProvider) Retrieve(ctx context.Context, uri string, _ confmap.WatcherFunc) (*confmap.Retrieved, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, ok := p.values[uri]
	if !ok {
		return nil, fmt.Errorf("%w: %s provider", ErrUncaptured, p.scheme)
	}
	return retrieved(v)
}

type capture struct {
	mu          sync.Mutex
	limits      CaptureLimits
	used        int64
	values      map[string]capturedValue
	environment *EnvironmentPolicy
	declared    map[string]string
	paths       hostfs.Policy
	// refusal is the first path-policy refusal: the Collector may wrap a
	// provider error without %w, so it is returned instead of the
	// resolution error.
	refusal error
	// root is the configuration's own URI, told apart from the references
	// it contains in refusals.
	root string
	// files are the scenario's mocks.files contents by cleaned path; they
	// answer ${file:} references before the host.
	files map[string][]byte
}

// CaptureOptions is trusted capture policy: limits, environment bindings and
// the host paths ${file:...} (and a configuration path) may read.
type CaptureOptions struct {
	Limits      CaptureLimits
	Environment EnvironmentPolicy
	Paths       hostfs.Policy
	// Files are the scenario's mocks.files contents by path. A ${file:}
	// reference naming one of them reads it instead of the host.
	Files map[string][]byte
}

// Refused reports whether err is a path-policy refusal (hostfs).
func Refused(err error) bool {
	return errors.Is(err, hostfs.ErrOutsideRoots) || errors.Is(err, hostfs.ErrNoRoots)
}

// CaptureSource uses Collector expansion and validation to discover the actual
// dependency graph. Each exact file/env URI is retrieved once across both the
// diagnostic and typed validation passes. Host env fallback is captured here for
// legacy compatibility, never consulted during frozen execution.
func CaptureSource(ctx context.Context, src Source, factories otelcol.Factories, env map[string]string, limits CaptureLimits) (*Resolved, error) {
	return CaptureSourceWithEnvironment(ctx, src, factories, env, limits, EnvironmentPolicy{LegacyHost: true})
}

// CaptureSourceWithEnvironment applies explicit trusted env-provider policy.
// The older CaptureSource entry point preserves local compatibility.
func CaptureSourceWithEnvironment(ctx context.Context, src Source, factories otelcol.Factories, env map[string]string, limits CaptureLimits, policy EnvironmentPolicy) (*Resolved, error) {
	return CaptureSourceWith(ctx, src, factories, env, CaptureOptions{Limits: limits, Environment: policy, Paths: hostfs.Policy{Unrestricted: true}})
}

// CaptureSourceWith captures under explicit options. Every host file it reads
// (the configuration path and ${file:...} URIs, nested ones included) opens
// through opts.Paths; a refusal is returned as the error, never as a
// configuration diagnostic.
func CaptureSourceWith(ctx context.Context, src Source, factories otelcol.Factories, env map[string]string, opts CaptureOptions) (*Resolved, error) {
	resolved, snapshot, err := captureWith(ctx, src, env, opts, func(ctx context.Context, settings otelcol.ConfigProviderSettings) (*Resolved, error) {
		return resolveSettings(ctx, settings, factories)
	})
	if err != nil {
		return nil, err
	}
	resolved.Snapshot, resolved.References = snapshot, snapshot.References()
	return resolved, nil
}

// CaptureSnapshot performs the host reads of CaptureSourceWith without
// component factories: it resolves only the raw configuration, so every
// ${file:}/${env:}/scheme reference is retrieved and sealed, and returns the
// snapshot. Snapshot.Resolve completes it with any registry, reading nothing
// from the host. A configuration naming unknown component types still captures.
func CaptureSnapshot(ctx context.Context, src Source, env map[string]string, opts CaptureOptions) (*Snapshot, error) {
	_, snapshot, err := captureWith(ctx, src, env, opts, func(ctx context.Context, settings otelcol.ConfigProviderSettings) (*Resolved, error) {
		_, err := resolveRawSettings(ctx, settings)
		return &Resolved{}, err
	})
	return snapshot, err
}

// captureWith is the one capture implementation; resolve is the step that
// drives retrieval (raw only, or typed with factories).
func captureWith(ctx context.Context, src Source, env map[string]string, opts CaptureOptions, resolve func(context.Context, otelcol.ConfigProviderSettings) (*Resolved, error)) (*Resolved, *Snapshot, error) {
	policy, limits := opts.Environment, opts.Limits
	if err := policy.Validate(); err != nil {
		return nil, nil, err
	}
	if err := opts.Paths.Validate(); err != nil {
		return nil, nil, err
	}
	policy = policy.Clone()
	limits = limits.defaults()
	if src.Path != "" && src.YAML == "" {
		// FileURI makes the path absolute against the working directory.
		if _, err := opts.Paths.Abs(src.Path); err != nil {
			return nil, nil, err
		}
	}
	uri, err := src.URI()
	if err != nil {
		return nil, nil, err
	}
	c := &capture{limits: limits, values: map[string]capturedValue{}, environment: &policy, declared: maps.Clone(env), paths: opts.Paths.Clone(), root: uri, files: map[string][]byte{}}
	for path, content := range opts.Files {
		c.files[filepath.Clean(path)] = bytes.Clone(content)
	}
	if strings.HasPrefix(uri, "yaml:") {
		c.used = int64(len(uri))
	}
	if c.used > limits.Bytes {
		return nil, nil, ErrCaptureLimit
	}
	// Copy overrides so later caller mutation cannot change capture midway.
	settings := ProviderSettings([]string{uri}, maps.Clone(env))
	for i, factory := range settings.ResolverSettings.ProviderFactories {
		probe := factory.Create(confmap.ProviderSettings{})
		scheme := probe.Scheme()
		_ = probe.Shutdown(ctx)
		if scheme == "yaml" {
			continue
		}
		settings.ResolverSettings.ProviderFactories[i] = confmap.NewProviderFactory(func(ps confmap.ProviderSettings) confmap.Provider {
			return &captureProvider{Provider: factory.Create(ps), capture: c}
		})
	}
	// A scheme without a provider stands in (unsupported_scheme): the
	// resolver knows its providers up front, so each one found is added
	// and resolution repeats, rereading nothing already captured.
	var resolved *Resolved
	for added := map[string]bool{}; ; {
		resolved, err = resolve(ctx, settings)
		m := unsupportedScheme.FindStringSubmatch(fmt.Sprint(err))
		if err == nil || m == nil || added[m[1]] || len(added) >= 16 {
			break
		}
		added[m[1]] = true
		settings.ResolverSettings.ProviderFactories = append(settings.ResolverSettings.ProviderFactories, standInFactory(m[1], c))
	}
	c.mu.Lock()
	refusal := c.refusal
	c.mu.Unlock()
	if refusal != nil {
		return nil, nil, refusal
	}
	snapshot := &Snapshot{data: snapshotData{Schema: capturedSchema, URIs: []string{uri}, Values: c.values, Environment: &policy}}
	if err != nil {
		return nil, nil, withStandInHint(err, snapshot.References())
	}
	return resolved, snapshot, nil
}

// standInFactory serves a scheme the distribution has no provider for: every
// URI stands in, and nothing is fetched.
func standInFactory(scheme string, c *capture) confmap.ProviderFactory {
	return confmap.NewProviderFactory(func(confmap.ProviderSettings) confmap.Provider {
		return &standInProvider{scheme: scheme, capture: c}
	})
}

type standInProvider struct {
	scheme  string
	capture *capture
}

func (p *standInProvider) Scheme() string               { return p.scheme }
func (*standInProvider) Shutdown(context.Context) error { return nil }
func (p *standInProvider) Retrieve(ctx context.Context, uri string, _ confmap.WatcherFunc) (*confmap.Retrieved, error) {
	c := p.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value, ok := c.values[uri]; ok {
		return retrieved(value)
	}
	return c.store(uri, standInValue(uri, ReasonUnsupportedScheme))
}

// store records a captured value within the limits. The caller holds mu.
func (c *capture) store(uri string, value capturedValue) (*confmap.Retrieved, error) {
	if len(c.values) >= c.limits.Dependencies || int64(len(uri))+int64(len(value.Content)) > c.limits.Bytes-c.used {
		return nil, ErrCaptureLimit
	}
	c.used += int64(len(uri)) + int64(len(value.Content))
	c.values[uri] = value
	return retrieved(value)
}

type captureProvider struct {
	confmap.Provider
	capture *capture
}

func (p *captureProvider) Retrieve(ctx context.Context, uri string, _ confmap.WatcherFunc) (*confmap.Retrieved, error) {
	c := p.capture
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if value, ok := c.values[uri]; ok {
		return retrieved(value)
	}
	if len(c.values) >= c.limits.Dependencies || int64(len(uri)) > c.limits.Bytes-c.used {
		return nil, ErrCaptureLimit
	}
	remaining := c.limits.Bytes - c.used - int64(len(uri))
	var value capturedValue
	if p.Scheme() == "file" {
		path := filepath.Clean(strings.TrimPrefix(uri, "file:"))
		if content, ok := c.files[path]; ok && uri != c.root {
			return c.store(uri, capturedValue{Content: bytes.Clone(content), Binding: SourceScenario})
		}
		// The policy checks the name and the opened handle: a regular file
		// under an allowed root (or any regular file, unrestricted).
		f, err := c.paths.Open(path)
		if err != nil && uri != c.root {
			// A reference the host cannot supply stands in.
			return c.store(uri, standInValue(uri, fileReason(err)))
		}
		if err != nil {
			if Refused(err) && c.refusal == nil {
				c.refusal = err
			}
			return nil, fmt.Errorf("capture file provider: %w", err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() > remaining {
			return nil, ErrCaptureLimit
		}
		value.Content, err = io.ReadAll(io.LimitReader(f, remaining+1))
		if err != nil {
			return nil, err
		}
	} else {
		var r *confmap.Retrieved
		var err error
		if p.Scheme() == "env" && c.environment != nil {
			r, value.Binding, err = boundEnvironment(ctx, uri, c.declared, *c.environment)
			name, _, _ := strings.Cut(strings.TrimPrefix(uri, "env:"), ":-")
			if errors.Is(err, ErrEnvironmentBinding) && environmentName.MatchString(name) {
				return c.store(uri, standInValue(uri, ReasonUnbound))
			}
		} else {
			r, err = p.Provider.Retrieve(ctx, uri, nil)
		}
		if err != nil {
			return nil, err
		}
		defer r.Close(ctx)
		text, err := r.AsString()
		if err != nil {
			return nil, err
		}
		value.Content = []byte(text)
		raw, err := r.AsRaw()
		if err != nil {
			return nil, err
		}
		str, isString := raw.(string)
		value.EmptyString = isString && str == "" && text == ""
	}
	if int64(len(value.Content)) > remaining {
		return nil, ErrCaptureLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.used += int64(len(uri)) + int64(len(value.Content))
	c.values[uri] = value
	return retrieved(value)
}
