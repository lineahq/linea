package platform

import (
	"context"
	"fmt"
	"slices"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// The authorization vocabulary adapters compose a team server with; the
// decisions stay in the platform.
type (
	Permission = authz.Permission
	Policy     = authz.Policy
	Binding    = authz.Binding
	Subject    = authz.Subject
	Scope      = authz.Scope
)

// ScopeAll is the scope of every resource.
var ScopeAll = authz.All

// ParseScope reads a binding's scope: "*", "project:<id>" or
// "fleet-group:<id>".
func ParseScope(v string) (Scope, error) { return authz.ParseScope(v) }

// NewPolicy validates role bindings against the built-in roles.
func NewPolicy(bindings []Binding) (*Policy, error) { return authz.NewPolicy(bindings) }

func identity(p Principal) authz.Identity { return authz.Identity{ID: p.ID, Groups: p.Groups} }

// authorize is every operation's entry check. A project-scoped permission
// must be held in some project, and the operation checks the resource's own
// project with authorizeIn; any other permission must be held in scope *.
func (s *Service) authorize(caller Caller, perm authz.Permission) error {
	id := identity(caller.Principal)
	if authz.ProjectScoped(perm) {
		if len(s.effective.Load().Scopes(id, perm)) != 0 {
			caller.decide(perm, "", true)
			return nil
		}
	} else if s.effective.Load().Allowed(id, perm, authz.All) {
		caller.decide(perm, "*", true)
		return nil
	}
	caller.decide(perm, "*", false)
	return &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: this operation needs the %s permission", perm)}
}

// Permissions lists the catalog permissions caller holds where operations
// first check them: platform-wide ones through scope *, project ones in at
// least one project. Clients use it to show what the caller may
// do; every operation still checks its own.
func (s *Service) Permissions(caller Caller) []string {
	id := identity(caller.Principal)
	out := []string{}
	for _, perm := range authz.Permissions() {
		if perm == authz.Admin {
			continue
		}
		if authz.ProjectScoped(perm) && len(s.effective.Load().Scopes(id, perm)) != 0 || s.effective.Load().Allowed(id, perm, authz.All) {
			out = append(out, string(perm))
		}
	}
	return out
}

// authorizeIn refuses a caller without perm in project. Writing to the
// library needs library.publish instead, whatever the caller's bindings in
// the library grant, and an archived project takes no new work.
func (s *Service) authorizeIn(caller Caller, perm authz.Permission, project string) error {
	if project == artifact.LibraryProject && writesProject(perm) {
		allowed := s.effective.Load().Allowed(identity(caller.Principal), authz.LibraryPublish, authz.All)
		caller.decide(authz.LibraryPublish, "project:"+project, allowed)
		if !allowed {
			return &Error{Code: CodeForbidden, Message: "forbidden: changing the library needs the library.publish permission"}
		}
		return s.projectWriteError(perm, project)
	}
	allowed := s.visible(caller, perm, project)
	caller.decide(perm, "project:"+project, allowed)
	if !allowed {
		return &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: this operation needs the %s permission in project %s", perm, project)}
	}
	if writesProject(perm) {
		return s.projectWriteError(perm, project)
	}
	return nil
}

func (s *Service) visible(caller Caller, perm authz.Permission, project string) bool {
	return s.effective.Load().Allowed(identity(caller.Principal), perm, authz.Scope{Kind: authz.ScopeProject, ID: project})
}

// projects is the list filter for perm: nil when the caller holds it in every
// project, otherwise the projects it holds it in (possibly none).
func (s *Service) projects(caller Caller, perm authz.Permission) []string {
	projects, all := s.effective.Load().Projects(identity(caller.Principal), perm)
	if all {
		return nil
	}
	return append([]string{}, projects...)
}

// runIn checks perm in the project of run id, read from the repository when
// not cached. An unknown run passes, so the operation reports it as it
// always has.
func (s *Service) runIn(ctx context.Context, caller Caller, perm authz.Permission, id string) error {
	if err := s.load(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		caller.touch(id)
		return s.authorizeIn(caller, perm, r.project)
	}
	return nil
}

// runProject is the project of a cached run, empty when it is unknown.
func (s *Service) runProject(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[id]; ok {
		return r.project
	}
	return ""
}

// captureProject is the project of capture id, empty when it is unknown.
func (s *Service) captureProject(id string) string {
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.sessions[id]; c != nil {
		return c.view.Project
	}
	return ""
}

// suiteIn checks perm in the project of suite id; an unknown suite passes.
func (s *Service) suiteIn(caller Caller, perm authz.Permission, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if suite, ok := s.suites[id]; ok {
		caller.touch(id)
		return s.authorizeIn(caller, perm, suite.Project)
	}
	return nil
}

// inProjects reports whether project passes a filter from projects.
func inProjects(filter []string, project string) bool {
	return filter == nil || slices.Contains(filter, project)
}

// captureIn checks perm in the project of capture id; an unknown capture
// passes.
func (s *Service) captureIn(caller Caller, perm authz.Permission, id string) error {
	r := s.collectors
	r.mu.Lock()
	c := r.sessions[id]
	r.mu.Unlock()
	if c == nil {
		return nil
	}
	caller.touch(id)
	return s.authorizeIn(caller, perm, c.view.Project)
}

// captureDataIn checks reading a capture's records: capture.read in its
// project and, when it holds raw data, capture.raw there too.
func (s *Service) captureDataIn(caller Caller, id string) error {
	if err := s.captureIn(caller, authz.CaptureRead, id); err != nil {
		return err
	}
	r := s.collectors
	r.mu.Lock()
	c := r.sessions[id]
	raw, project := c != nil && c.view.Data.ContainsRaw, ""
	if c != nil {
		project = c.view.Project
	}
	r.mu.Unlock()
	if !raw {
		return nil
	}
	return s.authorizeIn(caller, authz.CaptureRaw, project)
}
