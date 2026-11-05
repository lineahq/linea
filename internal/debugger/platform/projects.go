package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// Projects separate the runs, suites, saved work and captures of the teams
// that share a server. Two always exist: default, where requests that name
// no project land, and library, which holds the work admins share with
// other projects. Every other project is a record in the repository's
// project set, which also holds library audiences and runtime availability;
// every instance follows the set through the change feed.

// Project is a project, its settings and the permissions the caller holds
// in it. A zero setting means the installation's. Created is empty for a
// built-in project that was never changed.
type Project struct {
	ID       string
	Name     string
	BuiltIn  bool
	Archived bool
	ProjectSettings
	Permissions []string
	CreatedBy   string
	Created     *time.Time
	UpdatedBy   string
	Updated     *time.Time
}

// ProjectSettings are a project's own settings. DefaultRuntime is the
// runtime of the project's requests that name none, before the server's.
// StorageLimit bounds the bytes of the project's run evidence and bundles:
// retention evicts its oldest evictable runs beyond it.
// RetentionRuns and RetentionAge replace the server's retention policy for
// the project's runs when either is set.
type ProjectSettings struct {
	DefaultRuntime string
	StorageLimit   int64
	RetentionRuns  int
	RetentionAge   time.Duration
}

// ProjectList is the projects a caller may see, in ID order, and the
// project set's revision.
type ProjectList struct {
	Revision uint64
	Projects []Project
}

// CreateProjectRequest creates project ID at the project set's Revision.
type CreateProjectRequest struct {
	Revision uint64
	ID       string
	Name     string
	ProjectSettings
}

// UpdateProjectRequest replaces project ID's name and settings at the
// project set's Revision.
type UpdateProjectRequest struct {
	Revision uint64
	ID       string
	Name     string
	ProjectSettings
}

func builtInProject(id string) bool {
	return id == artifact.DefaultProject || id == artifact.LibraryProject
}

// projectState is the project set this instance follows.
func (s *Service) projectState() *artifact.ProjectSet {
	if p := s.projectSet.Load(); p != nil {
		return p
	}
	return &artifact.ProjectSet{}
}

// projectRecord returns project id: its record, or a built-in project's
// defaults.
func (s *Service) projectRecord(id string) (artifact.ProjectRecord, bool) {
	if p, ok := s.projectState().Project(id); ok {
		return p, true
	}
	switch id {
	case artifact.DefaultProject:
		return artifact.ProjectRecord{ID: id, Name: "Default"}, true
	case artifact.LibraryProject:
		return artifact.ProjectRecord{ID: id, Name: "Library"}, true
	}
	return artifact.ProjectRecord{}, false
}

// requestProject is the project a new resource joins: the requested one,
// default when empty. The project must exist and caller must hold perm in
// it; an unknown project is not found only for callers who could see it,
// so others cannot probe for IDs.
func (s *Service) requestProject(caller Caller, perm authz.Permission, id string) (string, error) {
	if id == "" {
		id = artifact.DefaultProject
	}
	if !artifact.ValidProject(id) {
		return "", &Error{Code: CodeInvalidRequest, Message: "invalid request: a project ID is 1 to 64 lowercase letters, digits and hyphens"}
	}
	if _, ok := s.projectRecord(id); !ok {
		if s.effective.Load().Allowed(identity(caller.Principal), perm, authz.All) {
			return "", &Error{Code: CodeNotFound, Message: fmt.Sprintf("not found: no project %s", id)}
		}
		return "", s.authorizeIn(caller, perm, id)
	}
	return id, s.authorizeIn(caller, perm, id)
}

// workIn resolves the caller's project for an operation that creates or
// runs something with perm and keeps the resolved ID on the caller.
func (s *Service) workIn(caller *Caller, perm authz.Permission) error {
	project, err := s.requestProject(*caller, perm, caller.Project)
	if err != nil {
		return err
	}
	caller.Project = project
	return nil
}

// writesProject reports whether perm creates or changes resources in a
// project, which an archived project refuses and only library.publish may
// do in the library.
func writesProject(perm authz.Permission) bool {
	switch perm {
	case authz.RunSubmit, authz.RunDelete, authz.ArtifactWrite, authz.CaptureStart, authz.Admin:
		return true
	}
	return false
}

// projectWriteError refuses a write that the project's state forbids: any
// new work in an archived project. Deleting stays allowed there.
func (s *Service) projectWriteError(perm authz.Permission, project string) error {
	if perm == authz.RunDelete || perm == authz.Admin {
		return nil
	}
	if p, ok := s.projectRecord(project); ok && p.Archived {
		return &Error{Code: CodeConflict, Message: fmt.Sprintf("conflict: project %s is archived and takes no new work", project)}
	}
	return nil
}

// ListProjects returns the projects in which the caller holds any
// permission, with those permissions; project.manage holders see all.
// The library is listed for every caller who holds a permission anywhere,
// and a caller who holds none is refused.
func (s *Service) ListProjects(ctx context.Context, caller Caller) (_ *ProjectList, err error) {
	defer s.audit(&caller, "ListProjects", &err)()
	if err := caller.validate(); err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	if len(s.Permissions(caller)) == 0 {
		return nil, &Error{Code: CodeForbidden, Message: "forbidden: listing projects needs a permission in at least one project"}
	}
	if err := ctx.Err(); err != nil {
		return nil, classify(err, CodeCancelled)
	}
	if err := s.refreshProjects(ctx); err != nil {
		return nil, err
	}
	set := s.projectState()
	policy, id := s.effective.Load(), identity(caller.Principal)
	manager := policy.Allowed(id, authz.ProjectManage, authz.All)
	ids := []string{artifact.DefaultProject, artifact.LibraryProject}
	for _, p := range set.Projects {
		if !builtInProject(p.ID) {
			ids = append(ids, p.ID)
		}
	}
	slices.Sort(ids)
	out := &ProjectList{Revision: set.Revision, Projects: []Project{}}
	for _, pid := range ids {
		perms := []string{}
		for _, p := range policy.Permitted(id, authz.Scope{Kind: authz.ScopeProject, ID: pid}) {
			if authz.ProjectScoped(p) {
				perms = append(perms, string(p))
			}
		}
		if !manager && len(perms) == 0 && pid != artifact.LibraryProject {
			continue
		}
		r, _ := s.projectRecord(pid)
		out.Projects = append(out.Projects, projectView(r, perms))
	}
	return out, nil
}

func projectView(r artifact.ProjectRecord, perms []string) Project {
	p := Project{ID: r.ID, Name: r.Name, BuiltIn: builtInProject(r.ID), Archived: r.Archived, Permissions: perms, CreatedBy: r.CreatedBy, UpdatedBy: r.UpdatedBy,
		ProjectSettings: ProjectSettings{DefaultRuntime: r.DefaultRuntime, StorageLimit: r.StorageLimit, RetentionRuns: r.RetentionRuns, RetentionAge: r.RetentionAge}}
	if !r.Created.IsZero() {
		created := r.Created
		p.Created = &created
	}
	if !r.Updated.IsZero() {
		updated := r.Updated
		p.Updated = &updated
	}
	return p
}

// CreateProject records a new project. An existing ID, built-in ones
// included, is a conflict.
func (s *Service) CreateProject(ctx context.Context, caller Caller, req CreateProjectRequest) (_ *Project, err error) {
	defer s.audit(&caller, "CreateProject", &err)()
	store, err := s.beginProjects(ctx, caller, authz.ProjectManage)
	if err != nil {
		return nil, err
	}
	caller.touch(req.ID)
	if !artifact.ValidProject(req.ID) {
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: a project ID is 1 to 64 lowercase letters, digits and hyphens"}
	}
	if err := s.validateProjectSettings(req.Name, req.ProjectSettings); err != nil {
		return nil, err
	}
	if _, ok := s.projectRecord(req.ID); ok {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("conflict: project %s exists", req.ID)}
	}
	record := artifact.ProjectRecord{ID: req.ID, Name: req.Name, CreatedBy: caller.Actor().Principal, Created: s.now().UTC().Truncate(time.Second)}
	record = withSettings(record, req.ProjectSettings)
	set, err := store.SaveProject(ctx, req.Revision, record)
	if err != nil {
		return nil, projectError(err)
	}
	return s.savedProject(caller, set, req.ID), nil
}

// UpdateProject replaces a project's name and settings.
func (s *Service) UpdateProject(ctx context.Context, caller Caller, req UpdateProjectRequest) (_ *Project, err error) {
	defer s.audit(&caller, "UpdateProject", &err)()
	store, err := s.beginProjects(ctx, caller, authz.ProjectManage)
	if err != nil {
		return nil, err
	}
	caller.touch(req.ID)
	if err := s.validateProjectSettings(req.Name, req.ProjectSettings); err != nil {
		return nil, err
	}
	record, err := s.knownProject(req.ID)
	if err != nil {
		return nil, err
	}
	record.Name = req.Name
	record = s.changed(withSettings(record, req.ProjectSettings), caller)
	set, err := store.SaveProject(ctx, req.Revision, record)
	if err != nil {
		return nil, projectError(err)
	}
	return s.savedProject(caller, set, req.ID), nil
}

// ArchiveProject makes a project read-only: its work stays readable and
// deletable, and it takes no new runs, suites, saves or captures. The
// built-in projects cannot be archived.
func (s *Service) ArchiveProject(ctx context.Context, caller Caller, revision uint64, id string) (_ *Project, err error) {
	defer s.audit(&caller, "ArchiveProject", &err)()
	store, err := s.beginProjects(ctx, caller, authz.ProjectManage)
	if err != nil {
		return nil, err
	}
	return s.setArchived(ctx, caller, store, revision, id, true)
}

// UnarchiveProject lets an archived project take new work again.
func (s *Service) UnarchiveProject(ctx context.Context, caller Caller, revision uint64, id string) (_ *Project, err error) {
	defer s.audit(&caller, "UnarchiveProject", &err)()
	store, err := s.beginProjects(ctx, caller, authz.ProjectManage)
	if err != nil {
		return nil, err
	}
	return s.setArchived(ctx, caller, store, revision, id, false)
}

func (s *Service) setArchived(ctx context.Context, caller Caller, store artifact.Projects, revision uint64, id string, archived bool) (*Project, error) {
	caller.touch(id)
	if builtInProject(id) {
		return nil, &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: the built-in project %s cannot be archived", id)}
	}
	record, err := s.knownProject(id)
	if err != nil {
		return nil, err
	}
	record.Archived = archived
	set, err := store.SaveProject(ctx, revision, s.changed(record, caller))
	if err != nil {
		return nil, projectError(err)
	}
	return s.savedProject(caller, set, id), nil
}

func (s *Service) beginProjects(ctx context.Context, caller Caller, perm authz.Permission) (artifact.Projects, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	if s.repository == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "projects need a workspace"}
	}
	return s.repository, nil
}

func (s *Service) knownProject(id string) (artifact.ProjectRecord, error) {
	record, ok := s.projectRecord(id)
	if !ok {
		return artifact.ProjectRecord{}, &Error{Code: CodeNotFound, Message: fmt.Sprintf("not found: no project %s", id)}
	}
	if record.Created.IsZero() {
		// A built-in project's first change records it.
		record.CreatedBy, record.Created = "", s.now().UTC().Truncate(time.Second)
	}
	return record, nil
}

func (s *Service) changed(r artifact.ProjectRecord, caller Caller) artifact.ProjectRecord {
	r.UpdatedBy, r.Updated = caller.Actor().Principal, s.now().UTC().Truncate(time.Second)
	return r
}

func withSettings(r artifact.ProjectRecord, settings ProjectSettings) artifact.ProjectRecord {
	r.DefaultRuntime, r.StorageLimit = settings.DefaultRuntime, settings.StorageLimit
	r.RetentionRuns, r.RetentionAge = settings.RetentionRuns, settings.RetentionAge
	return r
}

func (s *Service) savedProject(caller Caller, set artifact.ProjectSet, id string) *Project {
	s.applyProjects(set)
	r, _ := s.projectRecord(id)
	p := projectView(r, []string{})
	for _, perm := range s.effective.Load().Permitted(identity(caller.Principal), authz.Scope{Kind: authz.ScopeProject, ID: id}) {
		if authz.ProjectScoped(perm) {
			p.Permissions = append(p.Permissions, string(perm))
		}
	}
	return &p
}

// validateProjectSettings refuses a name or settings a project cannot
// have: a storage limit above the installation's or retention beyond the
// server's bounds.
func (s *Service) validateProjectSettings(name string, settings ProjectSettings) error {
	invalid := func(format string, args ...any) error {
		return &Error{Code: CodeInvalidRequest, Message: "invalid request: " + fmt.Sprintf(format, args...)}
	}
	switch {
	case len(name) > artifact.MaxProjectName || strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) || strings.TrimSpace(name) != name:
		return invalid("a project name is at most %d printable bytes without surrounding spaces", artifact.MaxProjectName)
	case len(settings.DefaultRuntime) > 64 || strings.ContainsFunc(settings.DefaultRuntime, func(r rune) bool { return !unicode.IsPrint(r) }):
		return invalid("a default runtime is at most 64 printable bytes")
	case settings.StorageLimit < 0 || settings.RetentionRuns < 0 || settings.RetentionAge < 0:
		return invalid("project settings are not negative")
	case settings.RetentionAge%time.Second != 0:
		return invalid("a retention age is whole seconds")
	}
	if limit := s.installationStorageLimit(); limit > 0 && settings.StorageLimit > limit {
		return invalid("a project's storage limit is at most the installation's, %d bytes", limit)
	}
	if settings.RetentionRuns > MaxRetentionRuns {
		return invalid("a project keeps at most %d runs", MaxRetentionRuns)
	}
	if settings.RetentionAge != 0 && (settings.RetentionAge < MinRetentionAge || settings.RetentionAge > MaxRetentionAge) {
		return invalid("a project's retention age is from %s to %d days", MinRetentionAge, int(MaxRetentionAge/(24*time.Hour)))
	}
	return nil
}

// installationStorageLimit is the repository's storage limit, 0 without
// one.
func (s *Service) installationStorageLimit() int64 {
	if s.repository == nil {
		return 0
	}
	return s.repository.Limits().StorageBytes
}

func projectError(err error) error {
	switch {
	case errors.Is(err, artifact.ErrConflict):
		return &Error{Code: CodeConflict, Message: "conflict: the projects changed; reload", cause: err}
	case errors.Is(err, artifact.ErrLimit):
		return &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("resource exhausted: at most %d projects", artifact.MaxProjects), cause: err}
	}
	return artifactError(err)
}

// recoverProjects reads the project set when the service opens.
func (s *Service) recoverProjects() error { return s.loadProjects(s.ctx) }

// loadProjects reads the project set from the repository again.
func (s *Service) loadProjects(ctx context.Context) error {
	if s.repository == nil {
		return nil
	}
	set, err := s.repository.ProjectSet(ctx)
	if err != nil {
		return err
	}
	s.applyProjects(set)
	return nil
}

// refreshProjects reads the set before an operation that lists it, so a
// list follows every instance's committed writes.
func (s *Service) refreshProjects(ctx context.Context) error {
	if err := s.loadProjects(ctx); err != nil {
		return artifactError(err)
	}
	return nil
}

// applyProjects makes set the project state unless a newer revision is
// already applied.
func (s *Service) applyProjects(set artifact.ProjectSet) {
	s.projectMu.Lock()
	defer s.projectMu.Unlock()
	if current := s.projectSet.Load(); current != nil && set.Revision < current.Revision {
		return
	}
	set = set.Clone()
	s.projectSet.Store(&set)
}

// projectDefaultRuntime is the runtime a run in project uses when it names
// none: the project's default runtime, else the server's.
func (s *Service) projectDefaultRuntime(project string) string {
	if p, ok := s.projectRecord(project); ok && p.DefaultRuntime != "" {
		return p.DefaultRuntime
	}
	return s.defaultRuntimeName()
}

// availableTo reports whether runtime name may run work of project. A
// runtime without an availability record is available to every project.
func (s *Service) availableTo(name, project string) bool {
	a, ok := s.projectState().Availability(name)
	return !ok || artifact.Includes(a.Projects, project)
}

// runtimeAvailable refuses a run in project on a runtime that is not
// available to it; nil is the built-in runtime.
func (s *Service) runtimeAvailable(target *engine.RuntimeTarget, project string) error {
	name := BuiltInRuntime
	if target != nil && target.Name != "" {
		name = target.Name
	}
	if project == "" || s.availableTo(name, project) {
		return nil
	}
	return &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: runtime %s is not available to project %s", name, project)}
}

// projectRuntime is the runtime that work in project naming name uses:
// name, else the project's default, else the server's. It refuses one that
// is not available to the project.
func (s *Service) projectRuntime(project, name string) (string, error) {
	if name == "" {
		name = s.projectDefaultRuntime(project)
	}
	name = s.runtimeName(name)
	return name, s.runtimeAvailable(runtimeSelection(name), project)
}
