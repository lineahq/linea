// Package scenario loads debugging scenarios: versioned YAML files, kept
// separate from Collector configurations, that declare fixture inputs, mocks,
// environment values, execution limits and assertions.
package scenario

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/provenance"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Version is the only scenario schema version understood by this build.
const Version = 1

// FormatOTLPJSON is the only supported fixture format.
const FormatOTLPJSON = "otlp-json"

// Scenario is a parsed scenario file. Fields not listed here are rejected
// rather than silently ignored.
type Scenario struct {
	Version     int               `yaml:"version"`
	Name        string            `yaml:"name"`
	Inputs      []Input           `yaml:"inputs"`
	Mocks       Mocks             `yaml:"mocks"`
	Environment map[string]string `yaml:"environment"`
	Execution   Execution         `yaml:"execution"`
	Assertions  []Assertion       `yaml:"assertions"`
	// Private declared lineage, carried by input bundles and scenario export.
	// It neither authorizes capture nor changes how any input is replayed.
	CaptureOrigins []provenance.CaptureDerivation `yaml:"capture_origins,omitempty"`

	dir          string
	readFileFunc func(string) ([]byte, error)
	resolveFunc  func(dir, name string) (string, error)
}

// WithFileReader returns a copy whose dependencies are read through reader.
// Paths passed to reader are absolute and keep the scenario directory semantics.
// The engine uses this seam to capture once, then refuse uncaptured host reads.
func (s *Scenario) WithFileReader(reader func(string) ([]byte, error)) *Scenario {
	copy := *s
	copy.readFileFunc = reader
	return &copy
}

// WithPathResolver returns a copy that makes the paths it passes to its file
// reader with resolve instead of this host's path rules: a scenario captured
// on another operating system keeps that system's paths (a Windows host's
// bundle in a Linux worker). resolve returns ErrRelativeWithoutDir for a
// relative name when dir is empty.
func (s *Scenario) WithPathResolver(resolve func(dir, name string) (string, error)) *Scenario {
	copy := *s
	copy.resolveFunc = resolve
	return &copy
}

// ErrRelativeWithoutDir is a relative file name in a scenario without a
// directory.
var ErrRelativeWithoutDir = errors.New("relative file name without a scenario directory")

// Input is one fixture: a file, relative to the scenario file, or inline
// OTLP JSON in Data (a YAML mapping, or a string holding the JSON). Exactly
// one of File and Data is set.
type Input struct {
	Receiver string    `yaml:"receiver"`
	Signal   string    `yaml:"signal"`
	File     string    `yaml:"file"`
	Data     yaml.Node `yaml:"data"`
	Format   string    `yaml:"format"`
	// ClientAddress is the sender IP address (optionally with a port) the
	// receiver reports for this input, as for a connection from a pod.
	ClientAddress string `yaml:"client_address"`
	// After is how long the run waits, after the previous input (or the
	// Collector's start), before injecting this one: wall-clock time, in
	// which batch timeouts and decision timers fire.
	After Duration `yaml:"after"`
}

func (in Input) inline() bool { return !in.Data.IsZero() }

// Mocks declares the run's mock environment. Components with
// an external dependency always run against a mock; this section supplies
// its fixtures.
type Mocks struct {
	// Files form the filesystem that filelog receivers read.
	Files []FileMock `yaml:"files"`
	// Kubernetes holds the objects that k8sattributes processors read from
	// the mocked Kubernetes API.
	Kubernetes KubernetesMock `yaml:"kubernetes"`
	// Exporters documents exporter handling. Only capture exists: exporters
	// that could reach the network always record locally.
	Exporters struct {
		Mode string `yaml:"mode"`
	} `yaml:"exporters"`
	// Providers are the settings of component modules' debugger providers
	// by provider name, handed to each as JSON.
	Providers map[string]any `yaml:"providers"`
}

// KubernetesMock declares Kubernetes objects: inline manifests, and files
// holding manifests (relative to the scenario), such as the output of
// kubectl get -o yaml. Multi-document files and List objects are accepted.
type KubernetesMock struct {
	Objects []map[string]any `yaml:"objects"`
	Files   []string         `yaml:"files"`
}

// FileMock is one file a filelog receiver can read. Path is its path in the
// deployment. Content (text) or File (raw bytes, relative to the scenario)
// is written while the Collector runs; Existing is present before it starts,
// so start_at decides whether it is read.
type FileMock struct {
	Path     string    `yaml:"path"`
	Content  *string   `yaml:"content"`
	File     string    `yaml:"file"`
	Existing string    `yaml:"existing"`
	Modified time.Time `yaml:"modified"`
}

// Execution holds run limits and timing.
type Execution struct {
	Timeout Duration `yaml:"timeout"`
	// Settle is how long the run waits after the last input before the
	// Collector shuts down. Without it the run derives the wait its
	// components need (none, unless a shutdown would lose their output).
	Settle Duration `yaml:"settle"`
	// Timestamps is TimestampsRecorded (the default: input timestamps are
	// used as written) or TimestampsRebase: every input timestamp is
	// shifted so that the latest one falls at the run's start.
	Timestamps string `yaml:"timestamps"`
	// DrainTimeout is how long the run waits after the last record a mocked
	// receiver emitted before it shuts down.
	DrainTimeout Duration `yaml:"drain_timeout"`
}

// Values of execution.timestamps.
const (
	TimestampsRecorded = "recorded"
	TimestampsRebase   = "rebase"
)

// Duration is a time.Duration written as a Go duration string ("10s").
type Duration time.Duration

// UnmarshalYAML decodes a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v <= 0 {
		return fmt.Errorf("duration must be positive, got %s", s)
	}
	*d = Duration(v)
	return nil
}

// Fixture is a loaded scenario input.
type Fixture struct {
	// Receiver is the configured receiver ID, or empty to infer it.
	Receiver      string
	Payload       telemetry.Payload
	ClientAddress string
	After         time.Duration
}

// Load reads and validates a scenario file. Fixture files are not read
// until Fixtures is called.
func Load(path string) (*Scenario, error) {
	return LoadWith(path, nil)
}

// LoadWith is Load reading the scenario file, and later the files it names,
// through read (see WithFileReader). A nil read reads the host directly.
func LoadWith(path string, read func(string) ([]byte, error)) (*Scenario, error) {
	var raw []byte
	var err error
	if read != nil {
		raw, err = read(path)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(raw, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", path, err)
	}
	if read != nil {
		s = s.WithFileReader(read)
	}
	return s, nil
}

// Parse validates scenario YAML text. dir resolves relative input files;
// when it is empty (a scenario that is not a file, such as one from the web
// editor) inputs must be inline or use absolute paths.
func Parse(raw []byte, dir string) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty scenario")
		}
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	s.dir = dir
	return &s, nil
}

// ParseSetup validates the setup of a quick run: scenario YAML holding only
// mocks, environment and execution. The quick run's single fixture is its
// input; assertions use a separate source, so inputs and assertions are rejected.
// version may be omitted. Mocked content must be inline: a setup has no
// directory to resolve files against.
func ParseSetup(raw []byte) (*Scenario, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s Scenario
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	switch {
	case s.Version != 0 && s.Version != Version:
		return nil, fmt.Errorf("unsupported version %d (want %d)", s.Version, Version)
	case len(s.Inputs) > 0:
		return nil, errors.New("inputs: a quick run's input is its fixture; use a scenario for several inputs")
	case len(s.Assertions) > 0:
		return nil, errors.New("assertions: a quick run has none; use a scenario to assert")
	}
	if err := s.validateMocks(); err != nil {
		return nil, err
	}
	if err := s.validateExecution(); err != nil {
		return nil, err
	}
	if err := provenance.ValidateCaptures(s.CaptureOrigins); err != nil {
		return nil, err
	}
	return &s, nil
}

// ParseAssertions validates a quick run's separate assertions source: only
// assertions and an optional version are accepted, with at least one assertion.
func ParseAssertions(raw []byte) ([]Assertion, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var s struct {
		Version    int         `yaml:"version"`
		Assertions []Assertion `yaml:"assertions"`
	}
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if s.Version != 0 && s.Version != Version {
		return nil, fmt.Errorf("unsupported version %d (want %d)", s.Version, Version)
	}
	if len(s.Assertions) == 0 {
		return nil, errors.New("assertions: at least one assertion is required")
	}
	for i := range s.Assertions {
		if err := s.Assertions[i].validate(); err != nil {
			return nil, fmt.Errorf("assertions[%d]: %w", i, err)
		}
	}
	return s.Assertions, nil
}

func (s *Scenario) validate() error {
	if err := provenance.ValidateCaptures(s.CaptureOrigins); err != nil {
		return err
	}
	if s.Version != Version {
		return fmt.Errorf("unsupported version %d (want %d)", s.Version, Version)
	}
	if len(s.Inputs) == 0 && len(s.Mocks.Files) == 0 {
		return errors.New("at least one input or mocked file (mocks.files) is required")
	}
	if err := s.validateMocks(); err != nil {
		return err
	}
	if err := s.validateExecution(); err != nil {
		return err
	}
	for i := range s.Assertions {
		if err := s.Assertions[i].validate(); err != nil {
			return fmt.Errorf("assertions[%d]: %w", i, err)
		}
	}
	for i, in := range s.Inputs {
		if (in.File == "") == !in.inline() {
			return fmt.Errorf("inputs[%d]: exactly one of file or data is required", i)
		}
		if in.Format != "" && in.Format != FormatOTLPJSON {
			return fmt.Errorf("inputs[%d]: unsupported format %q (want %s)", i, in.Format, FormatOTLPJSON)
		}
		switch in.Signal {
		case "", "traces", "metrics", "logs":
		default:
			return fmt.Errorf("inputs[%d]: unknown signal %q", i, in.Signal)
		}
		if a := in.ClientAddress; a != "" && !ValidClientAddress(a) {
			return fmt.Errorf("inputs[%d]: client_address %q is not an IP address (optionally with a port)", i, a)
		}
	}
	return nil
}

func (s *Scenario) validateExecution() error {
	switch t := s.Execution.Timestamps; t {
	case "", TimestampsRecorded, TimestampsRebase:
		return nil
	default:
		return fmt.Errorf("execution.timestamps: unsupported %q (want %s or %s)", t, TimestampsRecorded, TimestampsRebase)
	}
}

func (s *Scenario) validateMocks() error {
	if m := s.Mocks.Exporters.Mode; m != "" && m != "capture" {
		return fmt.Errorf("mocks.exporters.mode: unsupported %q (only capture)", m)
	}
	for i, f := range s.Mocks.Files {
		if f.Path == "" {
			return fmt.Errorf("mocks.files[%d]: path is required", i)
		}
		if f.Content != nil && f.File != "" {
			return fmt.Errorf("mocks.files[%d]: at most one of content or file", i)
		}
	}
	return nil
}

// Timeout returns the execution timeout, or 0 when none is declared.
func (s *Scenario) Timeout() time.Duration { return time.Duration(s.Execution.Timeout) }

// Settle returns the declared wait after the last input, or 0.
func (s *Scenario) Settle() time.Duration { return time.Duration(s.Execution.Settle) }

// Rebase reports whether input timestamps are shifted to the run's start.
func (s *Scenario) Rebase() bool { return s.Execution.Timestamps == TimestampsRebase }

// ValidClientAddress reports whether a is an IP address, optionally with a
// port.
func ValidClientAddress(a string) bool {
	if h, _, err := net.SplitHostPort(a); err == nil {
		a = h
	}
	return net.ParseIP(a) != nil
}

// MockSpec reads the mocked files and Kubernetes objects in declaration
// order.
func (s *Scenario) MockSpec() (mock.Spec, error) {
	spec := mock.Spec{Drain: time.Duration(s.Execution.DrainTimeout)}
	spec.Objects = append(spec.Objects, s.Mocks.Kubernetes.Objects...)
	for i, name := range s.Mocks.Kubernetes.Files {
		raw, err := s.readFile(name)
		if err != nil {
			return mock.Spec{}, fmt.Errorf("mocks.kubernetes.files[%d]: %w", i, err)
		}
		objects, err := decodeManifests(raw)
		if err != nil {
			return mock.Spec{}, fmt.Errorf("mocks.kubernetes.files[%d] %s: %w", i, name, err)
		}
		spec.Objects = append(spec.Objects, objects...)
	}
	for i, f := range s.Mocks.Files {
		m := mock.File{Path: f.Path, Existing: []byte(f.Existing), Modified: f.Modified}
		switch {
		case f.Content != nil:
			m.Content = []byte(*f.Content)
		case f.File != "":
			raw, err := s.readFile(f.File)
			if err != nil {
				return mock.Spec{}, fmt.Errorf("mocks.files[%d]: %w", i, err)
			}
			m.Content = raw
		}
		spec.Files = append(spec.Files, m)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Mocks.Providers)) {
		raw, err := json.Marshal(s.Mocks.Providers[name])
		if err != nil {
			return mock.Spec{}, fmt.Errorf("mocks.providers.%s: %w", name, err)
		}
		if spec.Providers == nil {
			spec.Providers = map[string]json.RawMessage{}
		}
		spec.Providers[name] = raw
	}
	return spec, nil
}

// Fixtures reads the input files in declaration order and checks each
// declared signal against the file content.
func (s *Scenario) Fixtures() ([]Fixture, error) {
	out := make([]Fixture, len(s.Inputs))
	for i, in := range s.Inputs {
		name := in.File
		if in.inline() {
			name = "data"
		}
		raw, err := s.read(in)
		if err != nil {
			return nil, fmt.Errorf("inputs[%d]: %w", i, err)
		}
		p, err := telemetry.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("inputs[%d] %s: %w", i, name, err)
		}
		if in.Signal != "" && in.Signal != p.Signal.String() {
			return nil, fmt.Errorf("inputs[%d] %s: declared signal %s but it contains %s", i, name, in.Signal, p.Signal)
		}
		out[i] = Fixture{Receiver: in.Receiver, Payload: p, ClientAddress: in.ClientAddress, After: time.Duration(in.After)}
	}
	return out, nil
}

// read returns an input's OTLP JSON.
func (s *Scenario) read(in Input) ([]byte, error) {
	if in.inline() {
		if in.Data.Kind == yaml.ScalarNode {
			return []byte(in.Data.Value), nil
		}
		var v any
		if err := in.Data.Decode(&v); err != nil {
			return nil, err
		}
		return json.Marshal(v)
	}
	return s.readFile(in.File)
}

// readFile reads a file named by the scenario, relative to its directory.
func (s *Scenario) readFile(name string) ([]byte, error) {
	if s.readFileFunc != nil && s.resolveFunc != nil {
		abs, err := s.resolveFunc(s.dir, name)
		if errors.Is(err, ErrRelativeWithoutDir) {
			return nil, fmt.Errorf("file %s is relative, but the scenario has no directory; inline it or use an absolute path", name)
		}
		if err != nil {
			return nil, err
		}
		return s.readFileFunc(abs)
	}
	path := name
	if !filepath.IsAbs(path) {
		if s.dir == "" {
			return nil, fmt.Errorf("file %s is relative, but the scenario has no directory; inline it or use an absolute path", name)
		}
		path = filepath.Join(s.dir, path)
	}
	if s.readFileFunc != nil {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		return s.readFileFunc(abs)
	}
	return os.ReadFile(path)
}

// decodeManifests decodes the YAML documents of a manifest file; empty
// documents are skipped.
func decodeManifests(raw []byte) ([]map[string]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []map[string]any
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if len(doc) > 0 {
			out = append(out, doc)
		}
	}
}
