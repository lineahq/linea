package platform

import (
	"context"
	"fmt"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/catalog"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/upgrade"
)

// The upgrade check: a manifest moved to another release, and one
// configuration with its scenarios run on the current and the target
// runtime. The definitions live in internal/debugger/upgrade.
type (
	UpgradeCheck         = upgrade.Check
	UpgradeVerdict       = upgrade.Verdict
	UpgradeRuntime       = upgrade.Runtime
	UpgradeConfiguration = upgrade.Configuration
	UpgradeFeatureGates  = upgrade.FeatureGates
	UpgradeGateChange    = upgrade.GateChange
	UpgradeGateState     = upgrade.GateState
	UpgradeScenario      = upgrade.Scenario
	UpgradeReason        = upgrade.Reason
	FeatureGate          = release.Gate
)

// Upgrade verdicts and the reason codes of an inconclusive check.
const (
	UpgradeCompatible          = upgrade.Compatible
	UpgradeConfigurationBreaks = upgrade.ConfigurationBreaks
	UpgradeBehaviorChanges     = upgrade.BehaviorChanges
	UpgradeInconclusive        = upgrade.Inconclusive

	UpgradeFromInvalid          = upgrade.ReasonFromInvalid
	UpgradeScenarioInconclusive = upgrade.ReasonScenarioInconclusive
	UpgradeScenarioFailed       = upgrade.ReasonScenarioFailed
)

// RetargetManifestRequest moves a manifest to a verified release.
type RetargetManifestRequest struct {
	// Manifest is a path under the path policy, text or a saved revision of
	// kind manifest.
	Manifest Source
	// Version is the target release; empty means the newest verified one.
	Version string
}

// RetargetedManifest is a manifest moved to another release. Moved names
// each module whose version changed; Missing the core and contrib modules
// the target does not have (left unchanged; the build refuses them);
// Custom the other modules, which keep their version until the user
// confirms one.
type RetargetedManifest struct {
	Manifest string
	From, To string
	Moved    []string
	Missing  []string
	Custom   []string
	Notes    []string
}

// RetargetManifest moves a manifest to another release from that release's
// catalog: the core and contrib modules take the release's versions
// and the rest of the text is unchanged. It never saves or builds.
func (s *Service) RetargetManifest(ctx context.Context, caller Caller, req RetargetManifestRequest) (_ *RetargetedManifest, err error) {
	defer s.audit(&caller, "RetargetManifest", &err)()
	if err := s.begin(ctx, caller, authz.RuntimeRead); err != nil {
		return nil, err
	}
	target, err := s.verifiedRelease(req.Version)
	if err != nil {
		return nil, err
	}
	src := req.Manifest
	if _, err := s.resolveSource(ctx, caller, &src, artifact.Manifest, "manifest"); err != nil {
		return nil, err
	}
	raw, err := (config.Source{Path: src.Path, YAML: src.Text}).BytesWith(s.policy.Paths)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	if len(raw) > maxManifestBytes {
		return nil, manifestDiagnostics("manifest_too_large", fmt.Sprintf("the manifest is larger than %d bytes", maxManifestBytes))
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, classify(manifestDiagnostics("invalid_manifest", err.Error()), CodeValidationFailed)
	}
	var from *CollectorCatalog
	if r := s.versionTable().Release(m.CollectorVersion()); r != nil {
		from = r.Catalog
	}
	out, err := catalog.Retarget(from, target.Catalog, raw)
	if err != nil {
		return nil, classify(manifestDiagnostics("invalid_manifest", err.Error()), CodeValidationFailed)
	}
	return &RetargetedManifest{Manifest: string(out.Manifest), From: out.From, To: out.To,
		Moved: out.Moved, Missing: out.Missing, Custom: out.Custom, Notes: out.Notes}, nil
}

// UpgradeCheckRequest checks one configuration, and the scenarios that
// exercise it, on the current runtime and a target runtime.
type UpgradeCheckRequest struct {
	// From is the current runtime and To the target; empty or
	// BuiltInRuntime is the debugger's own.
	From, To string
	Config   Source
	// Scenarios each run on both runtimes on one captured input.
	Scenarios []ScenarioInput
	// Ignore lists OTTL paths whose differences do not count, as in
	// CompareRequest.
	Ignore []string
}

// CheckUpgrade answers whether moving to the target runtime keeps the
// configuration loading and the scenarios behaving the same. It
// inspects the configuration on both runtimes and compares their feature
// gates. When both accept the configuration, it runs each scenario on both
// runtimes on one captured input (SubmitComparison across runtimes), one
// pair at a time, and compares each pair with CompareRuns. It waits for
// the runs it submits; when ctx ends first, the check is abandoned: it
// cancels the pair it was waiting for and returns the context's error. The
// runtime difference alone never makes a scenario different.
func (s *Service) CheckUpgrade(ctx context.Context, caller Caller, req UpgradeCheckRequest) (_ *UpgradeCheck, err error) {
	defer s.audit(&caller, "CheckUpgrade", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	fromName, err := s.projectRuntime(caller.Project, req.From)
	if err != nil {
		return nil, err
	}
	toName, err := s.projectRuntime(caller.Project, req.To)
	if err != nil {
		return nil, err
	}
	if fromName == toName {
		return nil, &Error{Code: CodeInvalidRequest, Message: "an upgrade check needs two different runtimes"}
	}
	cfg := req.Config
	if _, err := s.resolveSource(ctx, caller, &cfg, artifact.Configuration, "config"); err != nil {
		return nil, err
	}
	in := upgrade.Input{}
	if in.From, in.FromGates, err = s.upgradeRuntime(ctx, fromName); err != nil {
		return nil, err
	}
	if in.To, in.ToGates, err = s.upgradeRuntime(ctx, toName); err != nil {
		return nil, err
	}
	for i, name := range []string{fromName, toName} {
		ins, err := s.InspectConfiguration(ctx, caller, InspectRequest{Config: cfg, Runtime: name})
		if err != nil {
			return nil, err
		}
		side := upgrade.Inspection{Valid: ins.Valid, Diagnostics: ins.Diagnostics}
		if i == 0 {
			in.FromConfig = side
		} else {
			in.ToConfig = side
		}
	}
	if raw, err := (config.Source{Path: cfg.Path, YAML: cfg.Text}).BytesWith(s.policy.Paths); err == nil {
		// A configuration that is not a YAML mapping has no component gates;
		// the inspections already say why.
		in.Components, _ = catalog.ConfigComponents(raw)
	}
	if in.FromConfig.Valid && in.ToConfig.Valid {
		for i, sc := range req.Scenarios {
			result, err := s.upgradeScenario(ctx, caller, cfg, sc, fromName, toName, req.Ignore)
			if err != nil {
				return nil, err
			}
			result.Name = scenarioName(sc.Scenario, i)
			in.Scenarios = append(in.Scenarios, result)
		}
	}
	return upgrade.Assess(in), nil
}

func scenarioName(src Source, i int) string {
	switch {
	case src.Label != "":
		return src.Label
	case src.Path != "":
		return src.Path
	case src.Artifact != nil:
		return "artifact " + src.Artifact.ArtifactID
	}
	return "scenario " + strconv.Itoa(i+1)
}

// gateLister is a runtime worker that lists its feature gates.
type gateLister interface {
	FeatureGates() []release.Gate
}

// upgradeRuntime describes one side of a check with its feature gates
// (nil when its worker does not list them).
func (s *Service) upgradeRuntime(ctx context.Context, name string) (upgrade.Runtime, []release.Gate, error) {
	if !s.selects(name) {
		var gates []release.Gate
		if c, err := engine.InProcess(); err == nil {
			gates = c.FeatureGates()
		}
		return upgrade.Runtime{Name: BuiltInRuntime, CollectorVersion: release.CollectorVersion, Verified: release.Verified}, gates, nil
	}
	b, err := s.bind(ctx, name)
	if err != nil {
		return upgrade.Runtime{}, nil, err
	}
	rt := upgrade.Runtime{Name: b.info.Name, CollectorVersion: b.info.CollectorVersion, Verified: b.info.Verified}
	if l, ok := b.worker.(gateLister); ok {
		return rt, l.FeatureGates(), nil
	}
	return rt, nil, nil
}

// upgradeScenario runs one scenario on both runtimes and compares the
// runs. A scenario that cannot be admitted or compared is a result that
// says why; only the caller's context ending is an error.
func (s *Service) upgradeScenario(ctx context.Context, caller Caller, cfg Source, sc ScenarioInput, from, to string, ignore []string) (upgrade.Scenario, error) {
	runs, err := s.SubmitComparison(ctx, caller, ComparisonRequest{Before: cfg, After: cfg, Mode: ModeScenario,
		Scenario: &sc, Runtime: from, AfterRuntime: to})
	if err != nil {
		if ctx.Err() != nil {
			return upgrade.Scenario{}, ctx.Err()
		}
		return upgrade.Scenario{Error: err.Error()}, nil
	}
	for _, id := range []string{runs.Before.ID, runs.After.ID} {
		// A failed run is compared, and the comparison says so.
		if _, err := s.GetRunReport(ctx, caller, id, true); err != nil && ctx.Err() != nil {
			// Nobody waits for the abandoned pair; a finished run refuses
			// cancellation, which is fine.
			cctx := context.WithoutCancel(ctx)
			_, _ = s.CancelRun(cctx, caller, runs.Before.ID)
			_, _ = s.CancelRun(cctx, caller, runs.After.ID)
			return upgrade.Scenario{}, ctx.Err()
		}
	}
	c, err := s.CompareRuns(ctx, caller, CompareRequest{Before: runs.Before.ID, After: runs.After.ID, Ignore: ignore})
	if err != nil {
		if ctx.Err() != nil {
			return upgrade.Scenario{}, ctx.Err()
		}
		return upgrade.Scenario{Before: runs.Before.ID, After: runs.After.ID, Error: err.Error()}, nil
	}
	return upgrade.ScenarioOf("", c), nil
}
