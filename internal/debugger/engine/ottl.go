package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/collector/component"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// OTTLRequest evaluates one transform processor of a configuration on an
// OTLP JSON payload without running the Collector: the OTTL playground.
// The configuration is typically an edited copy of the
// user's; nothing is written. A scenario supplies ${env:NAME} values.
type OTTLRequest struct {
	Config       string          `json:"config,omitempty"`
	ConfigYAML   string          `json:"config_yaml,omitempty"`
	Scenario     string          `json:"scenario,omitempty"`
	ScenarioYAML string          `json:"scenario_yaml,omitempty"`
	ScenarioDir  string          `json:"scenario_dir,omitempty"`
	Processor    string          `json:"processor"`
	Input        json.RawMessage `json:"input"`
}

// OTTLEvaluation is the playground result. An invalid configuration (for
// example a statement that does not parse) is reported through Valid and
// Diagnostics. Output is the replayed payload, absent when processing
// stopped; Diff compares it with the input.
type OTTLEvaluation struct {
	Valid       bool                `json:"valid"`
	Diagnostics []config.Diagnostic `json:"diagnostics"`
	Replay      *ottldebug.Report   `json:"replay,omitempty"`
	Output      json.RawMessage     `json:"output,omitempty"`
	Diff        *analysis.Diff      `json:"diff,omitempty"`
	// Runtime is the runtime that answered. The platform
	// sets it; a worker leaves it empty.
	Runtime *ReportRuntime `json:"runtime,omitempty"`
}

// EvaluateOTTL resolves the configuration and evaluates the processor's
// statements on the input with the upstream OTTL APIs. ${env:NAME} falls back
// to the host environment (legacy local behavior).
func EvaluateOTTL(ctx context.Context, req OTTLRequest) (*OTTLEvaluation, error) {
	return EvaluateOTTLWithEnvironment(ctx, req, config.EnvironmentPolicy{LegacyHost: true})
}

// EvaluateOTTLWithEnvironment evaluates under trusted environment policy, as
// runs prepare: an ${env:NAME} the policy does not bind is a diagnostic. Host
// paths are unrestricted.
func EvaluateOTTLWithEnvironment(ctx context.Context, req OTTLRequest, policy config.EnvironmentPolicy) (*OTTLEvaluation, error) {
	return EvaluateOTTLWithPolicy(ctx, req, Policy{Environment: policy, Paths: unrestricted})
}

// EvaluateOTTLWithPolicy evaluates under the composition's policy. Every host
// file the request names opens through the path policy; a refusal is an
// error, never a diagnostic.
func EvaluateOTTLWithPolicy(ctx context.Context, req OTTLRequest, pol Policy) (*OTTLEvaluation, error) {
	in, final, err := CaptureOTTL(ctx, req, pol)
	if err != nil || final != nil {
		return final, err
	}
	return CompleteOTTL(ctx, in)
}

// CaptureOTTL performs every host read and the request checks that need no
// factories. A source that does not resolve is a final evaluation with
// diagnostics; otherwise the input completes with CompleteOTTL.
func CaptureOTTL(ctx context.Context, req OTTLRequest, pol Policy) (*OTTLInput, *OTTLEvaluation, error) {
	if err := pol.Validate(); err != nil {
		return nil, nil, err
	}
	if nonEmpty(req.Config, req.ConfigYAML) != 1 {
		return nil, nil, fmt.Errorf("%w: exactly one of config or config_yaml is required", ErrInvalidRequest)
	}
	if req.Scenario != "" && req.ScenarioYAML != "" {
		return nil, nil, fmt.Errorf("%w: give scenario or scenario_yaml, not both", ErrInvalidRequest)
	}
	var id component.ID
	if err := id.UnmarshalText([]byte(req.Processor)); err != nil || id.Type().String() != "transform" {
		return nil, nil, fmt.Errorf("%w: processor must name a transform processor, got %q", ErrInvalidRequest, req.Processor)
	}
	if len(req.Input) == 0 {
		return nil, nil, fmt.Errorf("%w: input (OTLP JSON) is required", ErrInvalidRequest)
	}
	if _, err := telemetry.Decode(req.Input); err != nil {
		return nil, nil, fmt.Errorf("%w: input: %v", ErrInvalidRequest, err)
	}
	var env map[string]string
	var mocks mock.Spec
	if req.Scenario != "" || req.ScenarioYAML != "" {
		s, err := loadScenarioWith(req.Scenario, req.ScenarioYAML, req.ScenarioDir, pol.Paths)
		if err != nil {
			return nil, nil, err
		}
		env = s.Environment
		// Mock files only answer ${file:} references here; a preview with
		// unusable mocks still evaluates.
		mocks, _ = s.MockSpec()
	}
	out := &OTTLEvaluation{Diagnostics: []config.Diagnostic{}}
	snapshot, err := config.CaptureSnapshot(ctx, config.Source{Path: req.Config, YAML: req.ConfigYAML}, env, pol.captureOptions(mocks.Files))
	if config.Refused(err) {
		return nil, nil, err
	}
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, config.AsDiagnostics(err)...)
		return nil, out, nil
	}
	return &OTTLInput{snapshot: snapshot, processor: req.Processor, input: append(json.RawMessage(nil), req.Input...)}, nil, nil
}

// CompleteOTTL resolves the captured configuration with this process's
// registry and evaluates the processor. It reads no host file.
func CompleteOTTL(ctx context.Context, in *OTTLInput) (*OTTLEvaluation, error) {
	if in == nil || in.snapshot == nil {
		return nil, fmt.Errorf("%w: OTTL input is required", ErrInvalidRequest)
	}
	var id component.ID
	if err := id.UnmarshalText([]byte(in.processor)); err != nil || id.Type().String() != "transform" {
		return nil, fmt.Errorf("%w: processor must name a transform processor, got %q", ErrInvalidRequest, in.processor)
	}
	input, err := telemetry.Decode(in.input)
	if err != nil {
		return nil, fmt.Errorf("%w: input: %v", ErrInvalidRequest, err)
	}
	_, factories, err := inProcessFactories()
	if err != nil {
		return nil, err
	}
	out := &OTTLEvaluation{Diagnostics: []config.Diagnostic{}}
	resolved, err := in.snapshot.Resolve(ctx, factories)
	if err != nil {
		out.Diagnostics = append(out.Diagnostics, config.AsDiagnostics(err)...)
		return out, nil
	}
	cfg, ok := resolved.Config.Processors[id]
	if !ok {
		return nil, fmt.Errorf("%w: processor %q is not configured", ErrInvalidRequest, in.processor)
	}
	out.Valid = true
	out.Diagnostics = append(out.Diagnostics, resolved.Warnings...)
	out.Diagnostics = append(out.Diagnostics, config.StandInDiagnostics(resolved.References)...)
	evaluate := features.Evaluate()
	if evaluate == nil {
		return nil, fmt.Errorf("%w: this runtime has no transform processor, so it cannot replay OTTL (missing feature %s)", ErrInvalidRequest, features.OTTLReplay)
	}
	rep, result, err := evaluate(ctx, in.processor, cfg, input)
	if err != nil {
		return nil, err
	}
	out.Replay = rep
	if result != nil {
		if out.Output, err = result.JSON(); err != nil {
			return nil, err
		}
		d, err := analysis.DiffPayloads(input, *result)
		if err != nil {
			return nil, err
		}
		out.Diff = &d
	}
	return out, nil
}
