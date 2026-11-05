package platform

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// LibraryItem is a library artifact and the projects it is shared with.
type LibraryItem struct {
	Artifact ArtifactMetadata
	// Audience is the projects the item is shared with, IDs or "*" alone;
	// empty while it is shared with none. A caller outside the library sees
	// only its own projects of the audience.
	Audience []string
	SharedBy string
	Shared   *time.Time
}

// LibraryPage is one page of the library. A page may hold fewer items than
// the limit while NextCursor continues it. Revision is the project set's,
// which SetLibraryAudience expects.
type LibraryPage struct {
	Revision   uint64
	Items      []LibraryItem
	NextCursor string
}

type ListLibraryRequest struct {
	Kind   ArtifactKind
	Limit  int
	Cursor string
}

// SetLibraryAudienceRequest shares a library artifact with Projects, IDs or
// "*" alone, at the project set's Revision; an empty list shares it with
// none.
type SetLibraryAudienceRequest struct {
	Revision   uint64
	ArtifactID string
	Projects   []string
}

// SetRuntimeAvailabilityRequest makes runtime Runtime available to
// Projects, IDs or "*" alone, at the project set's Revision; an empty list
// makes it available to every project again.
type SetRuntimeAvailabilityRequest struct {
	Revision uint64
	Runtime  string
	Projects []string
}

// Sharing is a sharing record after a change, with the project set's new
// revision.
type Sharing struct {
	Revision uint64
	Projects []string
	SetBy    string
	Set      time.Time
}

// CopyArtifactRequest copies a revision of an artifact the caller can read,
// its head when RevisionID is empty, into the caller's project as a new
// artifact. Label defaults to the source's.
type CopyArtifactRequest struct {
	ArtifactID string
	RevisionID string
	Label      string
}

// ListLibrary pages the library: everything to callers who may read the
// library itself, otherwise the items shared with a project in which the
// caller may read artifacts. A caller project narrows it to the items
// shared with that project.
func (s *Service) ListLibrary(ctx context.Context, caller Caller, req ListLibraryRequest) (_ *LibraryPage, err error) {
	defer s.audit(&caller, "ListLibrary", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	if err := s.refreshProjects(ctx); err != nil {
		return nil, err
	}
	set := s.projectState()
	publisher := s.visible(caller, authz.ArtifactRead, artifact.LibraryProject)
	readers := s.projects(caller, authz.ArtifactRead)
	if caller.Project != "" {
		if !s.visible(caller, authz.ArtifactRead, caller.Project) && !publisher {
			caller.decide(authz.ArtifactRead, "project:"+caller.Project, false)
			return nil, &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: this operation needs the %s permission in project %s", authz.ArtifactRead, caller.Project)}
		}
		readers = []string{caller.Project}
	}
	page, err := repository.List(ctx, artifact.ListRequest{Kind: req.Kind, Projects: []string{artifact.LibraryProject}, Limit: req.Limit, Cursor: req.Cursor})
	if err != nil {
		return nil, artifactError(err)
	}
	out := &LibraryPage{Revision: set.Revision, Items: []LibraryItem{}, NextCursor: page.NextCursor}
	for _, m := range page.Items {
		item := LibraryItem{Artifact: m, Audience: []string{}}
		if a, ok := set.Audience(m.ID); ok {
			item.Audience, item.SharedBy = slices.Clone(a.Projects), a.SetBy
			at := a.Set
			item.Shared = &at
		}
		if caller.Project != "" && !artifact.Includes(item.Audience, caller.Project) && !(publisher && caller.Project == artifact.LibraryProject) {
			continue
		}
		if !publisher {
			item.Audience = visibleAudience(item.Audience, readers)
			if len(item.Audience) == 0 {
				continue
			}
			item.SharedBy = ""
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// visibleAudience is the part of an audience within readers, the caller's
// projects (nil for every project).
func visibleAudience(audience, readers []string) []string {
	if readers == nil || slices.Contains(audience, artifact.AllProjects) {
		return audience
	}
	out := []string{}
	for _, p := range audience {
		if slices.Contains(readers, p) {
			out = append(out, p)
		}
	}
	return out
}

// SetLibraryAudience shares a library artifact with projects.
func (s *Service) SetLibraryAudience(ctx context.Context, caller Caller, req SetLibraryAudienceRequest) (_ *Sharing, err error) {
	defer s.audit(&caller, "SetLibraryAudience", &err)()
	store, err := s.beginProjects(ctx, caller, authz.LibraryPublish)
	if err != nil {
		return nil, err
	}
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactRead)
	if err != nil {
		return nil, err
	}
	m, err := repository.Get(ctx, req.ArtifactID)
	if err != nil {
		return nil, artifactError(err)
	}
	caller.touch(m.ID)
	if m.Project != artifact.LibraryProject {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: artifact %s is in project %s; only library artifacts are shared", m.ID, m.Project)}
	}
	projects, err := s.sharingProjects(req.Projects)
	if err != nil {
		return nil, err
	}
	a := artifact.AudienceRecord{ArtifactID: m.ID, Projects: projects, SetBy: caller.Actor().Principal, Set: s.now().UTC().Truncate(time.Second)}
	set, err := store.SetAudience(ctx, req.Revision, a)
	if err != nil {
		return nil, projectError(err)
	}
	s.applyProjects(set)
	return &Sharing{Revision: set.Revision, Projects: projects, SetBy: a.SetBy, Set: a.Set}, nil
}

// SetRuntimeAvailability limits the projects a runtime runs work for.
func (s *Service) SetRuntimeAvailability(ctx context.Context, caller Caller, req SetRuntimeAvailabilityRequest) (_ *Sharing, err error) {
	defer s.audit(&caller, "SetRuntimeAvailability", &err)()
	store, err := s.beginProjects(ctx, caller, authz.RuntimeManage)
	if err != nil {
		return nil, err
	}
	caller.touch(req.Runtime)
	if req.Runtime != BuiltInRuntime && !validRuntimeName(req.Runtime) {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("runtime name %q is invalid: name %s or a runtime of the catalog", req.Runtime, BuiltInRuntime)}
	}
	projects, err := s.sharingProjects(req.Projects)
	if err != nil {
		return nil, err
	}
	a := artifact.AvailabilityRecord{Runtime: req.Runtime, Projects: projects, SetBy: caller.Actor().Principal, Set: s.now().UTC().Truncate(time.Second)}
	set, err := store.SetAvailability(ctx, req.Revision, a)
	if err != nil {
		return nil, projectError(err)
	}
	s.applyProjects(set)
	return &Sharing{Revision: set.Revision, Projects: projects, SetBy: a.SetBy, Set: a.Set}, nil
}

// sharingProjects validates a sharing list: "*" alone, or distinct known
// projects other than the library.
func (s *Service) sharingProjects(list []string) ([]string, error) {
	out := []string{}
	for _, p := range list {
		switch {
		case p == artifact.AllProjects && len(list) == 1:
		case p == artifact.AllProjects:
			return nil, &Error{Code: CodeInvalidRequest, Message: `invalid request: "*" shares with every project and stands alone`}
		case p == artifact.LibraryProject:
			return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: the library is not shared with itself"}
		case slices.Contains(out, p):
			return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: project %s is listed twice", p)}
		default:
			if _, ok := s.projectRecord(p); !ok {
				return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: no project %s", p)}
			}
		}
		out = append(out, p)
	}
	if len(out) > artifact.MaxProjectList {
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: at most %d projects", artifact.MaxProjectList)}
	}
	return out, nil
}

// CopyArtifact copies an artifact revision into the caller's project as a
// new artifact that keeps the revision's provenance. Copying into the
// library publishes it there; copying out of it uses an item shared with
// the caller's project.
func (s *Service) CopyArtifact(ctx context.Context, caller Caller, req CopyArtifactRequest) (_ *SavedArtifact, err error) {
	defer s.audit(&caller, "CopyArtifact", &err)()
	repository, err := s.artifactStore(ctx, caller, authz.ArtifactWrite)
	if err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
		return nil, err
	}
	m, err := s.artifactIn(ctx, caller, authz.ArtifactRead, repository, req.ArtifactID)
	if err != nil {
		return nil, err
	}
	revisionID := req.RevisionID
	if revisionID == "" {
		revisionID = m.Head
	}
	revision, err := repository.Revision(ctx, artifact.Ref{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, RevisionID: revisionID})
	if err != nil {
		return nil, artifactError(err)
	}
	label := req.Label
	if label == "" {
		label = m.Label
	}
	saved, err := saveArtifact(ctx, repository, artifact.SaveRequest{WorkspaceID: m.WorkspaceID, Project: caller.Project, Kind: m.Kind,
		Label: label, Content: revision.Content, Actor: caller.Principal.ID, Origin: revision.Revision.Origin})
	if err != nil {
		return nil, artifactError(err)
	}
	caller.touch(saved.Artifact.ID)
	return saved, nil
}

// sharedReader reports the project through which caller reads library
// artifact id: the caller's project when the item is shared with it,
// otherwise any project of the audience in which the caller may read
// artifacts.
func (s *Service) sharedReader(caller Caller, id string) (string, bool) {
	a, ok := s.projectState().Audience(id)
	if !ok {
		return "", false
	}
	if caller.Project != "" {
		return caller.Project, artifact.Includes(a.Projects, caller.Project) && s.visible(caller, authz.ArtifactRead, caller.Project)
	}
	readers := s.projects(caller, authz.ArtifactRead)
	for _, p := range a.Projects {
		switch {
		case p == artifact.AllProjects && (readers == nil || len(readers) > 0):
			if readers == nil {
				return artifact.DefaultProject, true
			}
			return readers[0], true
		case readers == nil || slices.Contains(readers, p):
			return p, true
		}
	}
	return "", false
}

// usableIn refuses a saved source outside the work's project: work uses its
// own project's artifacts and the library's items shared with it.
func (s *Service) usableIn(caller Caller, m ArtifactMetadata, role string) error {
	project := caller.Project
	if project == "" {
		project = artifact.DefaultProject
	}
	if m.Project == project {
		return nil
	}
	if a, ok := s.projectState().Audience(m.ID); ok && m.Project == artifact.LibraryProject && artifact.Includes(a.Projects, project) {
		return nil
	}
	return invalidSource("%s: artifact %s is in project %s; work in project %s uses its own artifacts and the library's shared with it", role, m.ID, m.Project, project)
}
