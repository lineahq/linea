package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/container"
)

var ErrWorkspaceRequired = errors.New("a persistent workspace is required for saved artifacts")

// Open starts a service on repository, which it owns from then on: Close
// closes it, and so does a failed Open. Recovery completes before the worker
// starts; unfinished runs with a bundle are scheduled again. The repository's
// own limits apply. A BuildExport that names no image repository fails it.
func Open(repository artifact.Repository, opts Options) (*Service, error) {
	if opts.BuildExport != "" {
		name, err := container.ParseRepository(opts.BuildExport)
		if err != nil {
			repository.Close()
			return nil, fmt.Errorf("build export repository: %w", err)
		}
		opts.BuildExport = name
	}
	if err := repository.EnableRuns(context.Background()); err != nil {
		repository.Close()
		return nil, err
	}
	s := newService(opts, repository)
	if s.shared {
		// Changes committed during recovery are applied after it.
		head, err := repository.ChangeHead(context.Background())
		if err != nil {
			s.stop()
			repository.Close()
			return nil, err
		}
		s.cursor = head
	}
	for _, step := range []func() error{s.recoverCaptures, s.recoverRuns, s.recoverTombstones, s.recoverSuites, s.recoverDefaultRuntime, s.recoverRoleBindings, s.recoverProjects} {
		if err := step(); err != nil {
			s.stop()
			repository.Close()
			return nil, err
		}
	}
	s.startWorker()
	return s, nil
}

func (s *Service) artifactStore(ctx context.Context, caller Caller, perm authz.Permission) (artifact.Repository, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	if s.repository == nil {
		return nil, classify(ErrWorkspaceRequired, CodeUnavailable)
	}
	return s.repository, nil
}

// artifactError classifies a repository failure and replaces storage details
// with a safe message.
func artifactError(err error) error {
	err = classify(err, CodeStorageError)
	var e *Error
	if errors.As(err, &e) && e.Code == CodeStorageError {
		safe := *e
		safe.Message = artifact.ErrStorage.Error()
		if errors.Is(err, artifact.ErrCorrupt) {
			safe.Message = artifact.ErrCorrupt.Error()
		}
		return &safe
	}
	return err
}

func (s *Service) GetWorkspaceInfo(ctx context.Context, caller Caller) (_ *WorkspaceInfo, err error) {
	defer s.audit(&caller, "GetWorkspaceInfo", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	info := repository.Info()
	return &info, nil
}

// StorageUsage is a workspace's storage limits and the bytes it stores.
// Used may exceed Limit when the limit was lowered; new work is then refused.
// Runs counts the stored runs and KeptRuns the kept ones; OldestRun is when
// the oldest was admitted. Retention is the server's policy, zero without one.
type StorageUsage struct {
	Limit     int64
	RunLimit  int64
	Used      int64
	Runs      int64
	KeptRuns  int64
	OldestRun *time.Time
	Retention RetentionPolicy
}

// GetStorageUsage reports the workspace's storage limits and usage; an
// ephemeral service has none (unavailable).
func (s *Service) GetStorageUsage(ctx context.Context, caller Caller) (_ *StorageUsage, err error) {
	defer s.audit(&caller, "GetStorageUsage", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	used, err := repository.Usage(ctx)
	if err != nil {
		return nil, artifactError(err)
	}
	limits := repository.Limits()
	usage := &StorageUsage{Limit: limits.StorageBytes, RunLimit: limits.RunBytes, Used: used, Retention: s.retention}
	if r, ok := repository.(artifact.Retention); ok {
		stats, err := r.RunStats(ctx)
		if err != nil {
			return nil, artifactError(err)
		}
		usage.Runs, usage.KeptRuns = stats.Runs, stats.Kept
		if !stats.Oldest.IsZero() {
			oldest := stats.Oldest
			usage.OldestRun = &oldest
		}
	}
	return usage, nil
}

// SaveArtifact publishes original supplied bytes; it neither validates Collector
// semantics nor rewrites imported source files. The revision's actor is the
// caller's principal, never a client-provided identity.
func (s *Service) SaveArtifact(ctx context.Context, caller Caller, req SaveArtifactRequest) (_ *SavedArtifact, err error) {
	defer s.audit(&caller, "SaveArtifact", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactWrite)
	if err != nil {
		return nil, err
	}
	save := req.artifact(caller.Principal.ID)
	if save.ArtifactID == "" {
		err = s.workIn(&caller, authz.ArtifactWrite)
		save.Project = caller.Project
	} else {
		_, err = s.artifactIn(ctx, caller, authz.ArtifactWrite, repository, save.ArtifactID)
	}
	if err != nil {
		return nil, err
	}
	saved, err := saveArtifact(ctx, repository, save)
	if err == nil {
		caller.touch(saved.Artifact.ID)
	}
	return saved, artifactError(err)
}

// artifactIn returns an artifact's metadata once the caller holds perm in its
// project.
func (s *Service) artifactIn(ctx context.Context, caller Caller, perm authz.Permission, repository artifact.Repository, id string) (ArtifactMetadata, error) {
	m, err := repository.Get(ctx, id)
	if err != nil {
		return ArtifactMetadata{}, artifactError(err)
	}
	caller.touch(id)
	if perm == authz.ArtifactRead && m.Project == artifact.LibraryProject && !s.visible(caller, perm, m.Project) {
		// A library item is read through a project it is shared with.
		if project, ok := s.sharedReader(caller, id); ok {
			caller.decide(perm, "project:"+project, true)
			return m, nil
		}
	}
	if err := s.authorizeIn(caller, perm, m.Project); err != nil {
		return ArtifactMetadata{}, err
	}
	return m, nil
}

func (s *Service) GetArtifact(ctx context.Context, caller Caller, id string) (_ ArtifactMetadata, err error) {
	defer s.audit(&caller, "GetArtifact", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	return s.artifactIn(ctx, caller, authz.ArtifactRead, repository, id)
}

// GetArtifactRevision returns a revision of an artifact in this workspace.
func (s *Service) GetArtifactRevision(ctx context.Context, caller Caller, artifactID, revisionID string) (_ *ArtifactRevision, err error) {
	defer s.audit(&caller, "GetArtifactRevision", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	m, err := s.artifactIn(ctx, caller, authz.ArtifactRead, repository, artifactID)
	if err != nil {
		return nil, err
	}
	revision, err := repository.Revision(ctx, artifact.Ref{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, RevisionID: revisionID})
	return revision, artifactError(err)
}

func (s *Service) ListArtifacts(ctx context.Context, caller Caller, req ListArtifactsRequest) (_ *ArtifactPage, err error) {
	defer s.audit(&caller, "ListArtifacts", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	projects, err := s.listIn(caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	page, err := repository.List(ctx, artifact.ListRequest{Kind: req.Kind, Projects: projects, Limit: req.Limit, Cursor: req.Cursor})
	return page, artifactError(err)
}

// ListArtifactRevisions pages an artifact's revision metadata newest first.
// Pages stay stable while the artifact gains revisions.
func (s *Service) ListArtifactRevisions(ctx context.Context, caller Caller, req ListArtifactRevisionsRequest) (_ *ArtifactRevisionPage, err error) {
	defer s.audit(&caller, "ListArtifactRevisions", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	if _, err := s.artifactIn(ctx, caller, authz.ArtifactRead, repository, req.ArtifactID); err != nil {
		return nil, err
	}
	page, err := repository.Revisions(ctx, artifact.RevisionsRequest{ArtifactID: req.ArtifactID, Limit: req.Limit, Cursor: req.Cursor})
	return page, artifactError(err)
}
