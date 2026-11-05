package platform

import (
	"context"
	"fmt"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// resolveSource replaces a saved revision reference with the revision's bytes,
// so preparation captures them exactly as inline text. It returns
// the reference for the run to record. The revision's kind must match the role.
func (s *Service) resolveSource(ctx context.Context, caller Caller, src *Source, kind ArtifactKind, role string) (*ArtifactRef, error) {
	if src.Label != "" && src.Path != "" {
		return nil, invalidSource("%s: a label names inline text or a saved revision, not a path", role)
	}
	ref := src.Artifact
	if ref == nil {
		return nil, nil
	}
	if src.Path != "" || src.Text != "" {
		return nil, invalidSource("%s: give exactly one of path, text or artifact", role)
	}
	if ref.ArtifactID == "" || ref.RevisionID == "" {
		return nil, invalidSource("%s: an artifact reference needs an artifact and a revision ID", role)
	}
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	m, err := s.artifactIn(ctx, caller, authz.ArtifactRead, repository, ref.ArtifactID)
	if err != nil {
		return nil, err
	}
	if m.Kind != kind {
		return nil, invalidSource("%s: artifact %s is a %s artifact, not a %s artifact", role, m.ID, m.Kind, kind)
	}
	if err := s.usableIn(caller, m, role); err != nil {
		return nil, err
	}
	revision, err := repository.Revision(ctx, artifact.Ref{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, RevisionID: ref.RevisionID})
	if err != nil {
		return nil, artifactError(err)
	}
	if len(revision.Content) == 0 {
		return nil, invalidSource("%s: revision %s is empty", role, ref.RevisionID)
	}
	src.Text, src.Artifact = string(revision.Content), nil
	src.origin = revision.Revision.Origin
	pinned := *ref
	return &pinned, nil
}

func invalidSource(format string, args ...any) error {
	return classify(fmt.Errorf("%w: "+format, append([]any{engine.ErrInvalidRequest}, args...)...), CodeInvalidRequest)
}

// sourceRefs returns nil unless a source was submitted by reference.
func sourceRefs(config, fixture, scenario *ArtifactRef) *SourceRefs {
	if config == nil && fixture == nil && scenario == nil {
		return nil
	}
	return &SourceRefs{Config: config, Fixture: fixture, Scenario: scenario}
}

// resolveRun resolves the references of a run request on a copy.
func (s *Service) resolveRun(ctx context.Context, caller Caller, req RunRequest) (RunRequest, *SourceRefs, error) {
	config, err := s.resolveSource(ctx, caller, &req.Config, artifact.Configuration, "config")
	if err != nil {
		return req, nil, err
	}
	var fixture, scenario *ArtifactRef
	if req.Quick != nil {
		q := *req.Quick
		if fixture, err = s.resolveSource(ctx, caller, &q.Fixture, artifact.Fixture, "fixture"); err != nil {
			return req, nil, err
		}
		if q.Setup, err = inheritedCaptureSetup(q.Setup, q.Fixture); err != nil {
			return req, nil, err
		}
		req.Quick = &q
	}
	if req.Scenario != nil {
		in := *req.Scenario
		if scenario, err = s.resolveSource(ctx, caller, &in.Scenario, artifact.Scenario, "scenario"); err != nil {
			return req, nil, err
		}
		req.Scenario = &in
	}
	return req, sourceRefs(config, fixture, scenario), nil
}

// resolveContext resolves a configuration and optional scenario context, as
// inspection and preview take them.
func (s *Service) resolveContext(ctx context.Context, caller Caller, config *Source, in **ScenarioInput) error {
	if _, err := s.resolveSource(ctx, caller, config, artifact.Configuration, "config"); err != nil {
		return err
	}
	if *in != nil {
		copied := **in
		if _, err := s.resolveSource(ctx, caller, &copied.Scenario, artifact.Scenario, "scenario"); err != nil {
			return err
		}
		*in = &copied
	}
	return nil
}
