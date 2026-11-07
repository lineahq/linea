package engine

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/topology"
)

const (
	// CodeScenarioIgnored marks an inspection that ignored invalid scenario
	// text.
	CodeScenarioIgnored = "scenario_ignored"
	// CodeMockFixtureMissing marks a mocked component the scenario gives
	// nothing (usable) to read.
	CodeMockFixtureMissing = "mock_fixture_missing"
	// CodeWallClock marks an expression that reads the wall clock while the
	// inputs keep their recorded timestamps.
	CodeWallClock = "wall_clock"
)

// InspectionSchemaVersion versions the Inspection JSON shape. 0.2 added
// runtime, 0.3 recommendations.
const InspectionSchemaVersion = "0.3"

// InspectRequest selects a configuration to validate, by path (Config) or as
// text (ConfigYAML). A scenario, by path or as text, or the setup of a
// quick run optionally supplies ${env:NAME} values and mocks.
type InspectRequest struct {
	Config       string `json:"config,omitempty"`
	ConfigYAML   string `json:"config_yaml,omitempty"`
	Scenario     string `json:"scenario,omitempty"`
	ScenarioYAML string `json:"scenario_yaml,omitempty"`
	ScenarioDir  string `json:"scenario_dir,omitempty"`
	// SetupYAML is a quick run's setup (see Request.SetupYAML).
	SetupYAML string `json:"setup_yaml,omitempty"`
	// ConfigLabel names inline configuration text (see Request.ConfigLabel).
	ConfigLabel string `json:"config_label,omitempty"`
}

// Source identifies an input by content. Path is empty for inputs given as
// text.
type Source struct {
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256"`
}

// Inspection is the static analysis of a configuration.
// An invalid configuration is reported through Valid and Diagnostics rather
// than as an error.
type Inspection struct {
	Schema      string              `json:"inspection_schema"`
	Config      Source              `json:"config"`
	Valid       bool                `json:"valid"`
	Diagnostics []config.Diagnostic `json:"diagnostics"`
	Topology    *topology.Graph     `json:"topology,omitempty"`
	// References are the configuration's ${scheme:...} references and where
	// each value came from, stand-ins included.
	References []config.Reference `json:"references,omitempty"`
	// Effective is the resolved configuration with defaults applied and
	// sensitive values redacted. The source file is never modified.
	Effective map[string]any `json:"effective,omitempty"`
	// Runtime is the runtime that answered. The platform
	// sets it; a worker leaves it empty.
	Runtime *ReportRuntime `json:"runtime,omitempty"`
	// Recommendations are the ones the configuration alone supports
	// (confidence static); a run adds those its evidence supports.
	Recommendations []recommend.Recommendation `json:"recommendations,omitempty"`
}

// Inspect validates the configuration with the debugger's Collector registry
// and describes its topology. Errors are returned only when the request
// itself cannot be served (for example an unreadable file). ${env:NAME}
// falls back to the host environment (legacy local behavior).
func Inspect(ctx context.Context, req InspectRequest) (*Inspection, error) {
	return InspectWithEnvironment(ctx, req, config.EnvironmentPolicy{LegacyHost: true})
}

// InspectWithEnvironment inspects under trusted environment policy, as runs
// prepare: an ${env:NAME} the policy does not bind is a diagnostic. Host paths
// are unrestricted.
func InspectWithEnvironment(ctx context.Context, req InspectRequest, policy config.EnvironmentPolicy) (*Inspection, error) {
	return InspectWithPolicy(ctx, req, Policy{Environment: policy, Paths: unrestricted})
}

// InspectWithPolicy inspects under the composition's policy. Every host file
// the request names opens through the path policy; a refusal is an error,
// never a diagnostic, because it is not a configuration problem.
func InspectWithPolicy(ctx context.Context, req InspectRequest, pol Policy) (*Inspection, error) {
	in, final, err := CaptureInspection(ctx, req, pol)
	if err != nil || final != nil {
		return final, err
	}
	return CompleteInspection(ctx, in)
}

// CaptureInspection performs every host read. When the capture alone decides
// the outcome (request error or refused path: an error; a source that does not
// resolve: a final Inspection with diagnostics) it returns that; otherwise it
// returns an input for CompleteInspection. It needs no component factories.
func CaptureInspection(ctx context.Context, req InspectRequest, pol Policy) (*InspectionInput, *Inspection, error) {
	if err := pol.Validate(); err != nil {
		return nil, nil, err
	}
	if (req.Config == "") == (req.ConfigYAML == "") {
		return nil, nil, fmt.Errorf("%w: exactly one of config or config_yaml is required", ErrInvalidRequest)
	}
	if nonEmpty(req.Scenario, req.ScenarioYAML, req.SetupYAML) > 1 {
		return nil, nil, fmt.Errorf("%w: give at most one of scenario, scenario_yaml or setup_yaml", ErrInvalidRequest)
	}
	if req.ConfigLabel != "" && req.ConfigYAML == "" {
		return nil, nil, fmt.Errorf("%w: a config label applies to inline config text only", ErrInvalidRequest)
	}
	cfg := config.Source{Path: req.Config, YAML: req.ConfigYAML}
	src, err := sourceOf(cfg, pol.Paths)
	if err != nil {
		return nil, nil, err
	}
	src.Path = cmp.Or(src.Path, req.ConfigLabel)
	out := &Inspection{Schema: InspectionSchemaVersion, Config: src, Diagnostics: []config.Diagnostic{}}
	var env map[string]string
	var mocks mock.Spec
	rebase := false
	if req.Scenario != "" || req.ScenarioYAML != "" || req.SetupYAML != "" {
		var s *scenario.Scenario
		if req.SetupYAML != "" {
			s, err = scenario.ParseSetup([]byte(req.SetupYAML))
		} else {
			s, err = loadScenarioWith(req.Scenario, req.ScenarioYAML, req.ScenarioDir, pol.Paths)
		}
		switch {
		case err != nil && (req.Scenario != "" || config.Refused(err)):
			return nil, nil, err
		case err != nil:
			// Scenario text comes from an editor and may be mid-edit; the
			// configuration is still validated, without its environment.
			out.Diagnostics = append(out.Diagnostics, config.Diagnostic{
				Severity: config.SeverityWarning, Code: CodeScenarioIgnored,
				Message: fmt.Sprintf("%s not applied: %v", setupNoun(req), err),
				Hint:    "its ${env:NAME} values and mocks are unavailable until it is valid",
			})
		default:
			env = s.Environment
			rebase = s.Rebase()
			if mocks, err = s.MockSpec(); config.Refused(err) {
				return nil, nil, err
			} else if err != nil {
				out.Diagnostics = append(out.Diagnostics, config.Diagnostic{
					Severity: config.SeverityWarning, Code: CodeMockFixtureMissing,
					Message: fmt.Sprintf("%s mocks not applied: %v", setupNoun(req), err),
				})
			}
		}
	}

	snapshot, err := config.CaptureSnapshot(ctx, cfg, env, pol.captureOptions(mocks.Files))
	if config.Refused(err) {
		return nil, nil, err
	}
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, config.AsDiagnostics(err)...)
		return nil, out, nil
	}
	return &InspectionInput{source: src, snapshot: snapshot, diagnostics: out.Diagnostics, mocks: mocks, rebase: rebase}, nil, nil
}

// CompleteInspection resolves the captured configuration with this process's
// registry and finishes the inspection. It reads no host file.
func CompleteInspection(ctx context.Context, in *InspectionInput) (*Inspection, error) {
	if in == nil || in.snapshot == nil {
		return nil, fmt.Errorf("%w: inspection input is required", ErrInvalidRequest)
	}
	out := &Inspection{Schema: InspectionSchemaVersion, Config: in.source, Diagnostics: append([]config.Diagnostic{}, in.diagnostics...)}
	mocks, rebase := in.mocks, in.rebase
	c, factories, err := inProcessFactories()
	if err != nil {
		return nil, err
	}
	resolved, err := in.snapshot.Resolve(ctx, factories)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, config.AsDiagnostics(err)...)
		return out, nil
	}
	out.Valid = true
	out.Diagnostics = append(out.Diagnostics, resolved.Warnings...)
	out.Diagnostics = append(out.Diagnostics, config.StandInDiagnostics(resolved.References)...)
	g := graph(c, resolved, factories)
	out.Topology = &g
	out.Recommendations = recommend.Static(&g, contextLosses(resolved, &g))
	out.References = resolved.References
	// Without a scenario the gaps say what a run's scenario has to supply.
	plugins, err := c.Plugins()
	if err != nil {
		return nil, err
	}
	if err := mock.CheckSettings(mocks, plugins); err != nil {
		out.Diagnostics = append(out.Diagnostics, config.Diagnostic{
			Severity: config.SeverityWarning, Code: CodeMockFixtureMissing, Message: err.Error(),
			Hint: "name a provider of the runtime under mocks.providers, or choose a runtime built with the module that has it",
		})
	}
	for _, gap := range mock.Gaps(resolved.Config, mocks, c.Builtins(), plugins) {
		d := config.Diagnostic{
			Severity: config.SeverityWarning, Code: CodeMockFixtureMissing,
			Component: gap.Ref, Message: gap.Message, Hint: gap.Hint,
		}
		if gap.Provider != "" {
			d.ProviderSettings = &config.ProposedSettings{Provider: gap.Provider, Settings: gap.Settings}
		}
		out.Diagnostics = append(out.Diagnostics, d)
	}
	if !rebase {
		for _, r := range c.WallClockReads(resolved.Config) {
			out.Diagnostics = append(out.Diagnostics, config.Diagnostic{
				Severity: config.SeverityWarning, Code: CodeWallClock, Component: r.Component,
				Message: fmt.Sprintf("`%s` reads the wall clock (Now()): its outcome depends on how old the input timestamps are when the run happens", r.Expression),
				Hint:    "declare execution.timestamps: rebase to shift the input timestamps to the run's start",
			})
		}
	}
	if out.Effective, err = config.Effective(resolved.Config); err != nil {
		return nil, err
	}
	return out, nil
}

func setupNoun(req InspectRequest) string {
	if req.SetupYAML != "" {
		return "quick run"
	}
	return "scenario"
}

func sourceOf(src config.Source, paths hostfs.Policy) (Source, error) {
	raw, err := src.BytesWith(paths)
	if err != nil {
		return Source{}, err
	}
	return sourceFromBytes(src.Path, raw), nil
}

// ReadConfigurationSource returns original source bytes through the same host
// path policy as inspection. It does not resolve providers or rewrite YAML.
func ReadConfigurationSource(path, text string, pol Policy) ([]byte, error) {
	if err := pol.Validate(); err != nil {
		return nil, err
	}
	if (path == "") == (text == "") {
		return nil, fmt.Errorf("%w: exactly one of config or config_yaml is required", ErrInvalidRequest)
	}
	return (config.Source{Path: path, YAML: text}).BytesWith(pol.Paths)
}

// ReadScenarioSource reads root bytes once through the host path policy, without
// parsing or reading dependencies. File sources use their absolute directory;
// inline sources retain dir for admission to resolve under the same policy.
func ReadScenarioSource(ctx context.Context, path, text, dir string, pol Policy) ([]byte, string, error) {
	if err := pol.Validate(); err != nil {
		return nil, "", err
	}
	if (path == "") == (text == "") {
		return nil, "", fmt.Errorf("%w: exactly one of scenario or scenario_yaml is required", ErrInvalidRequest)
	}
	if path == "" {
		return []byte(text), dir, nil
	}
	abs, err := pol.Paths.Abs(path)
	if err != nil {
		return nil, "", err
	}
	// Reuse admission's bounded root reader and its input-limit errors.
	capture := &inputCapture{ctx: ctx, files: map[string][]byte{}, paths: pol.Paths}
	raw, err := capture.read(abs)
	if err != nil {
		return nil, "", err
	}
	return raw, filepath.Dir(abs), nil
}

func sourceFromBytes(path string, raw []byte) Source {
	sum := sha256.Sum256(raw)
	return Source{Path: path, SHA256: hex.EncodeToString(sum[:])}
}
