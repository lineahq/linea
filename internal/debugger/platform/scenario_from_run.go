package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
)

type ScenarioFromRunRequest struct {
	Assertions string // Optional assertions-only YAML, appended after the run's own.
	Name       string // Optional replacement scenario name.
	Save       bool
	Label      string // Saved-artifact label; defaults to "scenario".
}

type ScenarioFromRun struct {
	YAML                string
	RequiredEnvironment []string
	Saved               *SavedArtifact
	// Export is YAML after the export header (see ExportScenario). A saved
	// revision holds YAML only.
	Export string
}

// CreateScenarioFromRun derives self-contained scenario sources from admission
// evidence, in any run state. It never executes or changes the original run.
func (s *Service) CreateScenarioFromRun(ctx context.Context, caller Caller, runID string, req ScenarioFromRunRequest) (_ *ScenarioFromRun, err error) {
	defer s.audit(&caller, "CreateScenarioFromRun", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if err := s.runIn(ctx, caller, authz.RunRead, runID); err != nil {
		return nil, err
	}
	if project := s.runProject(runID); project != "" {
		caller.Project = project
	}
	var repository artifact.Repository
	if req.Save {
		var err error
		repository, err = s.artifactStore(ctx, caller, authz.ArtifactWrite)
		if err != nil {
			return nil, err
		}
	}
	raw, err := s.getRunBundle(ctx, runID)
	if err != nil {
		return nil, err
	}
	if req.Assertions != "" {
		if _, err := scenario.ParseAssertions([]byte(req.Assertions)); err != nil {
			return nil, classify(fmt.Errorf("assertions: %w", err), CodeInvalidRequest)
		}
	}
	parts, err := engine.ReadBundleParts(raw)
	if err != nil {
		// Preserve the private cause, but do not expose archive parser details.
		e := newError(CodeStorageError, err)
		e.Message = "cannot read captured execution bundle"
		return nil, e
	}
	yaml, err := selfContainedScenario(parts, req.Name, []byte(req.Assertions))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, classify(err, CodeCancelled)
	}
	out := &ScenarioFromRun{YAML: string(yaml), RequiredEnvironment: parts.HostEnvironment, Export: exportHeader(runID, parts) + string(yaml)}
	if req.Save {
		label := req.Label
		if label == "" {
			label = "scenario"
		}
		sum := sha256.Sum256(raw)
		if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
			return nil, err
		}
		out.Saved, err = saveArtifact(ctx, repository, artifact.SaveRequest{WorkspaceID: repository.Info().ID, Project: caller.Project, Kind: artifact.Scenario,
			Label: label, Content: yaml, Actor: caller.Principal.ID,
			Origin: &artifact.Origin{Kind: "run", RunID: runID, BaseDigest: hex.EncodeToString(sum[:])}})
		if err != nil {
			return nil, artifactError(err)
		}
	}
	return out, nil
}

// selfContainedScenario derives a run's scenario with its inputs inlined from
// the captured bundle, optionally renamed and with extra assertions.
func selfContainedScenario(parts *engine.BundleParts, name string, assertions []byte) ([]byte, error) {
	yaml, err := scenario.FromRun(scenario.RunSources{Mode: parts.Mode, Receiver: parts.Receiver, ClientAddress: parts.ClientAddress,
		Fixture: parts.Fixture, Setup: parts.Setup, Assertions: parts.Assertions, Scenario: parts.Scenario,
		Dir: parts.ScenarioDir, Files: parts.Files}, name, assertions)
	if err != nil {
		code := CodeInvalidRequest
		if errors.Is(err, scenario.ErrMissingCapture) {
			code = CodeStorageError
		}
		return nil, classify(err, code)
	}
	return yaml, nil
}
