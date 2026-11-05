package engine

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/release"
)

// BundleSchema 6 records the target runtime of a run that executes out of
// process; bundles for the built-in runtime have none and are still
// written as schema 5. Schema 5 records source labels and saved revisions.
// Schema 4 records the run's limits. Schema 3 adds quick-run assertions.
// Schema 2 adds semantic build identity; schema 1 keeps its
// exact-executable compatibility rule. Schemas 1 to 3 remain readable,
// carry no limits and re-encode to their own bytes.
const BundleSchema = 6

// builtInBundleSchema is the schema of new bundles for the built-in runtime.
const builtInBundleSchema = 5
const MaxBundleBytes = 64 << 20

const bundleReproductionReason = "Configuration providers and declared inputs are captured; component runtime file/environment dependencies are not fully classified."

var ErrBundle = errors.New("invalid execution bundle")
var ErrBundleSchema = errors.New("unsupported execution bundle schema")
var ErrDistribution = errors.New("execution bundle requires a different debugger distribution")

// ErrRuntimeUnavailable marks a failure of an out-of-process runtime itself
// (its worker could not start or exited), not of the request.
var ErrRuntimeUnavailable = errors.New("runtime unavailable")

// BundleInfo is safe run metadata. The bundle itself contains private source and
// environment values. Its digest identifies the entire archive, not a secret.
type BundleInfo struct {
	Schema             int    `json:"schema"`
	Digest             string `json:"digest"`
	Bytes              int64  `json:"bytes"`
	Mode               string `json:"mode"`
	Reproducibility    string `json:"reproducibility"`
	ReproductionReason string `json:"reproduction_reason"`
	Environment        string `json:"environment,omitempty"`
	// Limits are the policy limits recorded at admission (schema 4); the
	// effective deadline also depends on the declared execution.timeout.
	Limits *ReportLimits `json:"limits,omitempty"`
	// Sources are the saved revisions sources were submitted by (schema 5).
	Sources *SourceRefs `json:"sources,omitempty"`
}

type distribution struct {
	Versions Versions `json:"versions"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	// Build is the semantic identity execution compatibility compares. Schema 1
	// bundles lack it and compare the executable digest instead.
	Build *buildIdentity `json:"build,omitempty"`
	// Executable is informational for new bundles: a rebuild of the same source
	// may replay its history.
	Executable   string                `json:"executable_digest,omitempty"`
	FeatureGates []release.FeatureGate `json:"feature_gates"`
}

// buildIdentity names what determines execution behavior: the main module, its
// VCS state when the toolchain stamped it, build settings and every linked
// module. Unstamped development builds (go run, go test) and dirty trees share
// an identity across source edits that keep the same dependencies.
type buildIdentity struct {
	Main         string   `json:"main"`
	Revision     string   `json:"vcs_revision,omitempty"`
	Modified     bool     `json:"vcs_modified,omitempty"`
	Settings     []string `json:"settings,omitempty"`
	Dependencies string   `json:"dependencies_digest"`
}

var currentBuild = sync.OnceValue(func() *buildIdentity {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	b := &buildIdentity{Main: info.Main.Path + "@" + info.Main.Version}
	for _, s := range info.Settings {
		switch {
		case s.Key == "vcs.revision":
			b.Revision = s.Value
		case s.Key == "vcs.modified":
			b.Modified = s.Value == "true"
		case strings.HasPrefix(s.Key, "vcs."):
			// vcs.time and vcs name follow from the revision.
		default:
			b.Settings = append(b.Settings, s.Key+"="+s.Value)
		}
	}
	slices.Sort(b.Settings)
	deps := make([]string, 0, len(info.Deps))
	for _, m := range info.Deps {
		if m.Replace != nil {
			m = m.Replace
		}
		deps = append(deps, m.Path+" "+m.Version+" "+m.Sum)
	}
	slices.Sort(deps)
	sum := sha256.Sum256([]byte(strings.Join(deps, "\n")))
	b.Dependencies = "sha256:" + hex.EncodeToString(sum[:])
	return b
})

// The executable digest is cached once per process, without caching a caller's
// cancellation as a permanent fault.
var executableDigest = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
})

// currentDistribution identifies this process's build. Without build info the
// executable digest is the identity and must be readable; otherwise it is
// recorded when readable.
func currentDistribution() (distribution, error) {
	d := distribution{Versions: CurrentVersions(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Build: currentBuild(), FeatureGates: changedFeatureGates()}
	digest, err := executableDigest()
	if err != nil && d.Build == nil {
		return d, err
	}
	d.Executable = digest
	return d, nil
}

// compatible compares semantic build identity, or the exact executable for
// bundles recorded without one.
func (d distribution) compatible() error {
	current, err := currentDistribution()
	if err != nil {
		return err
	}
	if d.Build != nil {
		d.Executable, current.Executable = "", ""
	} else if current.Executable == "" {
		return ErrDistribution
	} else {
		current.Build = nil
	}
	a, _ := json.Marshal(d)
	b, _ := json.Marshal(current)
	if !bytes.Equal(a, b) {
		return ErrDistribution
	}
	return nil
}

type bundleRequest struct {
	Config        string `json:"config_path,omitempty"`
	Fixture       string `json:"fixture_path,omitempty"`
	Scenario      string `json:"scenario_path,omitempty"`
	ScenarioDir   string `json:"scenario_dir,omitempty"`
	Receiver      string `json:"receiver,omitempty"`
	ClientAddress string `json:"client_address,omitempty"`
	Compare       bool   `json:"compare"`
	Normalize     bool   `json:"normalize"`
	// Limits is present from schema 4 on.
	Limits *ReportLimits `json:"limits,omitempty"`
	// Labels and Sources may be present from schema 5 on.
	ConfigLabel   string      `json:"config_label,omitempty"`
	FixtureLabel  string      `json:"fixture_label,omitempty"`
	ScenarioLabel string      `json:"scenario_label,omitempty"`
	Sources       *SourceRefs `json:"sources,omitempty"`
}

type bundleArchive struct {
	Schema             int               `json:"bundle_schema"`
	Mode               string            `json:"mode"`
	Reproducibility    string            `json:"reproducibility"`
	ReproductionReason string            `json:"reproduction_reason"`
	Distribution       distribution      `json:"distribution"`
	Request            bundleRequest     `json:"request"`
	Config             json.RawMessage   `json:"config_snapshot"`
	Fixture            []byte            `json:"fixture_base64,omitempty"`
	Scenario           []byte            `json:"scenario_base64,omitempty"`
	Setup              []byte            `json:"setup_base64,omitempty"`
	Assertions         []byte            `json:"assertions_base64,omitempty"`
	ScenarioBase       string            `json:"scenario_base,omitempty"`
	Files              map[string][]byte `json:"files"`
	FixtureFile        string            `json:"fixture_file,omitempty"`
	ScenarioFile       string            `json:"scenario_file,omitempty"`
	// Runtime is present from schema 6 on, and only there.
	Runtime *RuntimeTarget `json:"runtime,omitempty"`
}

// RuntimeTarget names the runtime a run executes on when it is not the
// built-in one: a debug Collector the debugger built, running out of process.
// Bundle schema 6 records it; a runtime refuses a bundle for
// another (ErrDistribution). A bundle without one is for the built-in
// runtime, identified by its distribution.
type RuntimeTarget struct {
	// ID is the runtime's identity as its worker announces it
	// ("sha256:<hex>").
	ID string `json:"id"`
	// Name is how the runtime was chosen, for messages; it is not compared.
	Name string `json:"name,omitempty"`
}

func (t *RuntimeTarget) describe() string {
	if t.Name != "" {
		return t.Name + " (" + t.ID + ")"
	}
	return t.ID
}

// Prepared reports whether the request has an engine-owned immutable capture.
// Custom scheduler test doubles can lack one; default production admission cannot.
func (r Request) Prepared() bool { return r.prepared != nil }

// EncodeBundle exports a private immutable input archive, excluding runtime ID,
// observer and logger. Equal captures/settings/builds produce equal bytes.
func EncodeBundle(req Request) ([]byte, *BundleInfo, error) {
	if req.prepared == nil || req.identity() != req.prepared.identity {
		return nil, nil, ErrBundle
	}
	p := req.prepared
	configRaw, err := json.Marshal(req.ConfigSnapshot)
	if err != nil {
		return nil, nil, err
	}
	mode := "scenario"
	if req.fixture() {
		mode = "quick"
	}
	schema := builtInBundleSchema
	switch {
	case req.Runtime != nil:
		if req.Runtime.ID == "" {
			return nil, nil, ErrBundle
		}
		schema = BundleSchema
	case p.schema != 0:
		schema = p.schema // a decoded bundle re-encodes to its own bytes
	}
	a := bundleArchive{Schema: schema, Mode: mode, Reproducibility: "unverified", ReproductionReason: bundleReproductionReason,
		Distribution: p.distribution, Request: bundleRequest{Config: req.Config, Fixture: req.Fixture, Scenario: req.Scenario,
			ScenarioDir: req.ScenarioDir, Receiver: req.Receiver, ClientAddress: req.ClientAddress, Compare: req.Compare, Normalize: req.Normalize},
		Config: configRaw, Fixture: p.fixture, Scenario: p.scenario, Setup: p.setup, Assertions: p.assertions,
		ScenarioBase: p.scenarioDir, Files: p.files, FixtureFile: p.fixtureFile, ScenarioFile: p.scenarioFile,
		Runtime: req.Runtime}
	if p.fixtureFile != "" {
		a.Fixture = nil
	}
	if p.scenarioFile != "" {
		a.Scenario = nil
	}
	if schema >= 4 {
		a.Request.Limits = req.Limits.record()
	}
	if schema >= 5 {
		a.Request.ConfigLabel, a.Request.FixtureLabel, a.Request.ScenarioLabel = req.ConfigLabel, req.FixtureLabel, req.ScenarioLabel
		a.Request.Sources = req.Sources
	} else if req.ConfigLabel != "" || req.FixtureLabel != "" || req.ScenarioLabel != "" || req.Sources != nil {
		return nil, nil, ErrBundle
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > MaxBundleBytes {
		return nil, nil, ErrInputLimit
	}
	return raw, bundleInfo(raw, a, req.ConfigSnapshot), nil
}

func bundleInfo(raw []byte, a bundleArchive, snapshot *config.Snapshot) *BundleInfo {
	sum := sha256.Sum256(raw)
	return &BundleInfo{a.Schema, "sha256:" + hex.EncodeToString(sum[:]), int64(len(raw)), a.Mode, a.Reproducibility, a.ReproductionReason, snapshot.EnvironmentMode(), a.Request.Limits, a.Request.Sources}
}

// InspectBundle validates the private envelope without checking this process's
// distribution or reading source paths. History remains readable after upgrades.
func InspectBundle(raw []byte) (*BundleInfo, error) {
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return nil, err
	}
	return bundleInfo(raw, a, snapshot), nil
}

// BundleParts contains private captured sources, never ordinary run metadata.
// Files are keyed by absolute path; HostEnvironment contains names only.
type BundleParts struct {
	Mode, Receiver, ClientAddress        string
	Fixture, Setup, Assertions, Scenario []byte
	ScenarioDir                          string
	Files                                map[string][]byte
	HostEnvironment                      []string
	// Config is the root configuration's original bytes: references stay
	// unexpanded, and provider-captured dependencies are not included.
	Config []byte
	// ConfigName, FixtureName and ScenarioName are each root source's
	// submitted path or label, empty for unnamed inline text.
	ConfigName, FixtureName, ScenarioName string
	// FixtureFile and ScenarioFile are the Files keys of root sources read
	// from a path, so callers can tell them from dependencies.
	FixtureFile, ScenarioFile string
	Sources                   *SourceRefs
	// ProductVersion, CollectorVersion and Build name the debugger build that
	// admitted the run; Build is one line (see buildIdentity.describe).
	ProductVersion, CollectorVersion, Build string
	// Reproducibility and ReproductionReason are the run's admission label.
	Reproducibility, ReproductionReason string
}

// InputsDigest identifies what the run injected and ran against: its mode,
// receiver and client address, fixture or scenario bytes, setup and the
// captured input and mock files ("sha256:<hex>"). The configuration and its
// provider-captured dependencies are not part of it, nor are a quick run's
// assertions, which only read the run. Two runs with equal digests received
// byte-identical inputs. A root source read from a path counts once, as its
// bytes: its Files entry, keyed by host path, is skipped, so the same text
// sent inline digests alike.
func (p *BundleParts) InputsDigest() string {
	h := sha256.New()
	// Each part is length-prefixed, so no two different inputs encode alike.
	part := func(b []byte) {
		fmt.Fprintf(h, "%d:", len(b))
		h.Write(b)
	}
	for _, s := range []string{p.Mode, p.Receiver, p.ClientAddress, p.ScenarioDir} {
		part([]byte(s))
	}
	part(p.Fixture)
	part(p.Setup)
	part(p.Scenario)
	names := slices.DeleteFunc(slices.Sorted(maps.Keys(p.Files)), func(name string) bool {
		return name == p.FixtureFile || name == p.ScenarioFile
	})
	fmt.Fprintf(h, "%d files;", len(names))
	for _, name := range names {
		part([]byte(name))
		part(p.Files[name])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// BundleRuntime returns the runtime a validated archive names, or nil for the
// built-in runtime.
func BundleRuntime(raw []byte) (*RuntimeTarget, error) {
	a, _, err := readBundle(raw)
	if err != nil || a.Runtime == nil {
		return nil, err
	}
	target := *a.Runtime
	return &target, nil
}

// ReadBundleParts validates and reads a private archive without checking this
// build's distribution or reading the host. Historical sources survive upgrades.
func ReadBundleParts(raw []byte) (*BundleParts, error) {
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return nil, err
	}
	r := requestFromBundle(a, snapshot)
	p := r.prepared
	return &BundleParts{Mode: a.Mode, Receiver: r.Receiver, ClientAddress: r.ClientAddress,
		Fixture: p.fixture, Setup: p.setup, Assertions: p.assertions, Scenario: p.scenario,
		ScenarioDir: p.scenarioDir, Files: p.files, HostEnvironment: snapshot.RequiredEnvironment(),
		Config: snapshot.SourceBytes(), ConfigName: cmp.Or(r.Config, r.ConfigLabel), FixtureName: cmp.Or(r.Fixture, r.FixtureLabel),
		ScenarioName: cmp.Or(r.Scenario, r.ScenarioLabel), FixtureFile: a.FixtureFile, ScenarioFile: a.ScenarioFile, Sources: r.Sources,
		ProductVersion: a.Distribution.Versions.Product, CollectorVersion: a.Distribution.Versions.Collector, Build: a.Distribution.describe(),
		Reproducibility: a.Reproducibility, ReproductionReason: a.ReproductionReason}, nil
}

// describe names the build in one line: the main module with its VCS revision
// when stamped, and the linked modules' digest; schema-1 bundles name the
// executable digest instead.
func (d distribution) describe() string {
	b := d.Build
	if b == nil {
		return "executable " + d.Executable
	}
	out := b.Main
	switch {
	case b.Revision == "":
		out += " (unstamped)"
	case b.Modified:
		out += " revision " + b.Revision + " (modified)"
	default:
		out += " revision " + b.Revision
	}
	return out + ", modules " + b.Dependencies
}

func readBundle(raw []byte) (bundleArchive, *config.Snapshot, error) {
	var a bundleArchive
	if len(raw) > MaxBundleBytes {
		return a, nil, ErrInputLimit
	}
	var version struct {
		Schema int `json:"bundle_schema"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return a, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	if version.Schema < 1 || version.Schema > BundleSchema {
		return a, nil, ErrBundleSchema
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	if d.Decode(new(any)) != io.EOF {
		return a, nil, ErrBundle
	}
	if a.Schema < 3 && len(a.Assertions) != 0 || a.Schema < 4 && a.Request.Limits != nil ||
		a.Schema < 5 && (a.Request.ConfigLabel != "" || a.Request.FixtureLabel != "" || a.Request.ScenarioLabel != "" || a.Request.Sources != nil) ||
		a.Schema < 6 && a.Runtime != nil {
		return a, nil, ErrBundleSchema
	}
	if a.Schema >= 6 && (a.Runtime == nil || a.Runtime.ID == "") {
		return a, nil, ErrBundle
	}
	if a.Schema >= 4 {
		if _, err := a.Request.Limits.decode(); err != nil {
			return a, nil, fmt.Errorf("%w: %w", ErrBundle, err)
		}
	}
	if a.Reproducibility != "unverified" || a.ReproductionReason != bundleReproductionReason || a.Files == nil || len(a.Files) > maxInputFiles ||
		(a.Mode != "quick" && a.Mode != "scenario") || a.Distribution.Versions.Product == "" ||
		a.Schema == 1 && (a.Distribution.Executable == "" || a.Distribution.Build != nil) ||
		a.Distribution.Build == nil && a.Distribution.Executable == "" ||
		a.Distribution.Build != nil && (a.Distribution.Build.Main == "" || a.Distribution.Build.Dependencies == "") {
		return a, nil, ErrBundle
	}
	if a.Mode == "quick" && (len(a.Scenario) != 0 || a.Request.Scenario != "" || a.Request.ScenarioDir != "" || a.ScenarioBase != "") ||
		a.Mode == "scenario" && (len(a.Fixture) != 0 || a.Request.Fixture != "" || len(a.Setup) != 0 || len(a.Assertions) != 0) {
		return a, nil, ErrBundle
	}
	if (a.Request.Fixture == "") != (a.FixtureFile == "") || (a.Request.Scenario == "") != (a.ScenarioFile == "") ||
		(a.FixtureFile != "" && len(a.Fixture) != 0) || (a.ScenarioFile != "" && len(a.Scenario) != 0) {
		return a, nil, ErrBundle
	}
	used := int64(len(a.Fixture)) + int64(len(a.Scenario)) + int64(len(a.Setup)) + int64(len(a.Assertions))
	if used > maxInputBytes {
		return a, nil, ErrInputLimit
	}
	for path, content := range a.Files {
		n := int64(len(path)) + int64(len(content))
		if path == "" || n > maxInputBytes-used {
			return a, nil, ErrInputLimit
		}
		used += n
	}
	for _, path := range []string{a.FixtureFile, a.ScenarioFile} {
		if path != "" {
			if _, ok := a.Files[path]; !ok {
				return a, nil, ErrBundle
			}
		}
	}
	snapshot, err := config.DecodeSnapshot(a.Config, config.CaptureLimits{})
	if err != nil {
		if errors.Is(err, config.ErrSnapshotSchema) {
			return a, nil, fmt.Errorf("%w: %w", ErrBundleSchema, err)
		}
		return a, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	if err := requestFromBundle(a, snapshot).Validate(); err != nil {
		return a, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	return a, snapshot, nil
}

// DecodeBundle prepares an explicit new execution from a captured archive. It
// requires a matching build identity (or, for schema 1, the exact executable),
// versions and gates, and performs no source host reads.
// Recovery does not call it or replay work. A caller may submit the returned
// request to receive a fresh run ID; portable cross-build replay is future work.
func DecodeBundle(ctx context.Context, raw []byte) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return Request{}, err
	}
	if a.Runtime != nil {
		return Request{}, fmt.Errorf("%w: the bundle is for runtime %s, not the built-in runtime", ErrDistribution, a.Runtime.describe())
	}
	if err := a.Distribution.compatible(); err != nil {
		return Request{}, err
	}
	return decodedRequest(ctx, a, snapshot, "")
}

// DecodeBundleForRuntime prepares an execution from an archive on the
// runtime whose identity is id: an out-of-process worker. The archive
// must name that runtime (schema 6). The admitting debugger's distribution
// is recorded, not compared: the worker is another build by design. It
// performs no host reads.
func DecodeBundleForRuntime(ctx context.Context, raw []byte, id string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return Request{}, err
	}
	switch {
	case a.Runtime == nil:
		return Request{}, fmt.Errorf("%w: the bundle is for the built-in runtime, not runtime %s", ErrDistribution, id)
	case a.Runtime.ID != id:
		return Request{}, fmt.Errorf("%w: the bundle is for runtime %s, not runtime %s", ErrDistribution, a.Runtime.describe(), id)
	}
	return decodedRequest(ctx, a, snapshot, id)
}

func decodedRequest(ctx context.Context, a bundleArchive, snapshot *config.Snapshot, runtime string) (Request, error) {
	req := requestFromBundle(a, snapshot)
	req.prepared.runtime = runtime
	if _, err := Prepare(ctx, req); err != nil {
		return Request{}, err
	}
	if _, err := loadInputs(req, req.prepared.read); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	return req, nil
}

func requestFromBundle(a bundleArchive, snapshot *config.Snapshot) Request {
	r := a.Request
	fixture, scenario := a.Fixture, a.Scenario
	if a.FixtureFile != "" {
		fixture = a.Files[a.FixtureFile]
	}
	if a.ScenarioFile != "" {
		scenario = a.Files[a.ScenarioFile]
	}
	limits, _ := r.Limits.decode() // validated by readBundle
	req := Request{Config: r.Config, Fixture: r.Fixture, Scenario: r.Scenario, ScenarioDir: r.ScenarioDir,
		Receiver: r.Receiver, ClientAddress: r.ClientAddress, Compare: r.Compare, Normalize: r.Normalize, ConfigSnapshot: snapshot, Limits: limits,
		ConfigLabel: r.ConfigLabel, FixtureLabel: r.FixtureLabel, ScenarioLabel: r.ScenarioLabel, Sources: r.Sources, Runtime: a.Runtime}
	if req.Config == "" {
		req.ConfigYAML = string(snapshot.SourceBytes())
	}
	if a.Mode == "quick" && req.Fixture == "" {
		req.FixtureJSON = string(fixture)
	}
	if a.Mode == "scenario" && req.Scenario == "" {
		req.ScenarioYAML = string(scenario)
	}
	req.SetupYAML = string(a.Setup)
	req.AssertionsYAML = string(a.Assertions)
	req.prepared = &preparation{files: a.Files, fixture: fixture, scenario: scenario,
		setup: a.Setup, assertions: a.Assertions, scenarioDir: a.ScenarioBase, distribution: a.Distribution, schema: a.Schema, fixtureFile: a.FixtureFile, scenarioFile: a.ScenarioFile}
	req.prepared.identity = req.identity()
	return req
}

// decode reads limits recorded in a schema-4 bundle. They must be present,
// canonical (re-encoding gives the same bytes) and without clamps.
func (r *ReportLimits) decode() (Limits, error) {
	if r == nil {
		return Limits{}, errors.New("limits are missing")
	}
	l := Limits{EvidenceBytes: r.EvidenceBytes, Events: r.Events}
	if r.Execution != "" {
		d, err := time.ParseDuration(r.Execution)
		if err != nil || d <= 0 || d.String() != r.Execution {
			return Limits{}, fmt.Errorf("invalid execution limit %q", r.Execution)
		}
		l.Execution = d
	}
	if l.EvidenceBytes < 0 || l.Events < 0 || len(r.Clamped) != 0 {
		return Limits{}, errors.New("invalid limits")
	}
	return l, nil
}
