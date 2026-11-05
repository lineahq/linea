// Package authz decides what a principal may do: permissions,
// roles, (subject, role, scope) bindings and a default-deny policy. Only the
// platform calls it; adapters see its decisions as forbidden errors.
package authz

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Permission is a named action.
type Permission string

// The permission catalog. Every platform operation declares one of these.
const (
	RunRead          Permission = "run.read"
	RunSubmit        Permission = "run.submit"
	RunDelete        Permission = "run.delete"
	ArtifactRead     Permission = "artifact.read"
	ArtifactWrite    Permission = "artifact.write"
	CaptureRead      Permission = "capture.read"
	CaptureStart     Permission = "capture.start"
	CaptureRaw       Permission = "capture.raw"
	FleetRead        Permission = "fleet.read"
	FleetConfigWrite Permission = "fleet.config.write"
	FleetRollout     Permission = "fleet.rollout"
	RuntimeRead      Permission = "runtime.read"
	RuntimeManage    Permission = "runtime.manage"
	// RuntimeBuild queues runtime builds on a team server and reads and
	// cancels one's own; RuntimeManage reads and cancels all.
	RuntimeBuild Permission = "runtime.build"
	AuditRead    Permission = "audit.read"
	// TokenOwn creates, lists and revokes one's own personal API tokens;
	// TokenManage does so for service principals and for every token.
	TokenOwn    Permission = "token.own"
	TokenManage Permission = "token.manage"
	// AccessManage lists and changes the role bindings managed beside the
	// server config's: every binding through scope *, a project's own bindings
	// through that project.
	AccessManage Permission = "access.manage"
	// ProjectManage creates, changes and archives projects and their
	// settings.
	ProjectManage Permission = "project.manage"
	// LibraryPublish saves, revises and deletes the work in the library
	// project and sets who it is shared with. No binding in the library
	// project grants a write there; only this permission does.
	LibraryPublish Permission = "library.publish"
	// Admin holds every permission.
	Admin Permission = "admin"
)

// Permissions is the catalog in a stable order.
func Permissions() []Permission {
	return []Permission{RunRead, RunSubmit, RunDelete, ArtifactRead, ArtifactWrite, CaptureRead, CaptureStart, CaptureRaw,
		FleetRead, FleetConfigWrite, FleetRollout, RuntimeRead, RuntimeManage, RuntimeBuild, AuditRead, TokenOwn, TokenManage, AccessManage, ProjectManage, LibraryPublish, Admin}
}

// ProjectScoped reports whether perm acts on resources that belong to a
// project (runs, suites, artifacts, captures, and the project's role
// bindings). Every other permission acts on platform-wide resources and is
// held only through scope *.
func ProjectScoped(perm Permission) bool {
	switch perm {
	case RunRead, RunSubmit, RunDelete, ArtifactRead, ArtifactWrite, CaptureRead, CaptureStart, CaptureRaw, AccessManage:
		return true
	}
	return false
}

// Role is a named set of permissions.
type Role struct {
	Name        string
	Permissions []Permission
}

var (
	viewer   = []Permission{RunRead, ArtifactRead, CaptureRead, FleetRead, RuntimeRead, TokenOwn}
	engineer = append(slices.Clone(viewer), RunSubmit, RunDelete, ArtifactWrite, CaptureStart, RuntimeBuild)
)

// Roles are the built-in roles. Raw capture data is a separate grant:
// no role but admin and raw-capture holds it.
func Roles() []Role {
	return []Role{
		{Name: "viewer", Permissions: slices.Clone(viewer)},
		{Name: "engineer", Permissions: slices.Clone(engineer)},
		{Name: "fleet-operator", Permissions: append(slices.Clone(engineer), FleetConfigWrite, FleetRollout)},
		{Name: "raw-capture", Permissions: []Permission{CaptureRaw}},
		{Name: "admin", Permissions: []Permission{Admin}},
	}
}

// ScopeKind says what a scope covers.
type ScopeKind string

const (
	// ScopeAll covers every resource.
	ScopeAll ScopeKind = "*"
	// ScopeProject covers one project's runs, suites, artifacts and captures.
	ScopeProject ScopeKind = "project"
	// ScopeFleetGroup covers one fleet group's resources; reserved.
	ScopeFleetGroup ScopeKind = "fleet-group"
)

// Scope is the part of the platform a binding applies to.
type Scope struct {
	Kind ScopeKind
	ID   string
}

// All is the scope of every resource.
var All = Scope{Kind: ScopeAll}

func (s Scope) String() string {
	if s.Kind == ScopeAll {
		return "*"
	}
	return string(s.Kind) + ":" + s.ID
}

// ParseScope reads "*", "project:<id>" or "fleet-group:<id>".
func ParseScope(v string) (Scope, error) {
	if v == "*" {
		return All, nil
	}
	kind, id, ok := strings.Cut(v, ":")
	if !ok || id == "" || (ScopeKind(kind) != ScopeProject && ScopeKind(kind) != ScopeFleetGroup) {
		return Scope{}, fmt.Errorf("scope %q: want *, project:<id> or fleet-group:<id>", v)
	}
	return Scope{Kind: ScopeKind(kind), ID: id}, nil
}

// Covers reports whether s includes every resource of o.
func (s Scope) Covers(o Scope) bool { return s.Kind == ScopeAll || s == o }

// Subject is who a binding grants a role to: one principal or every member of
// a group from the identity provider.
type Subject struct {
	Principal string
	Group     string
}

// Binding grants a role to a subject in a scope.
type Binding struct {
	Subject Subject
	Role    string
	Scope   Scope
}

// Identity is what a decision knows about a principal.
type Identity struct {
	ID     string
	Groups []string
}

func (b Binding) matches(id Identity) bool {
	if b.Subject.Principal != "" {
		return b.Subject.Principal == id.ID
	}
	return slices.Contains(id.Groups, b.Subject.Group)
}

// Policy is a validated set of bindings. A principal holds a permission in a
// scope only when a binding grants it; everything else is denied.
type Policy struct {
	bindings []binding
}

type binding struct {
	Binding
	permissions []Permission
}

// ErrReservedScope refuses a scope the platform does not enforce yet.
var ErrReservedScope = errors.New("fleet-group scopes are reserved")

// NewPolicy validates bindings against the built-in roles. Every binding
// names exactly one subject and a known role; fleet-group scopes are
// reserved until fleet resources carry them.
func NewPolicy(bindings []Binding) (*Policy, error) {
	roles := map[string][]Permission{}
	for _, r := range Roles() {
		roles[r.Name] = r.Permissions
	}
	p := &Policy{}
	for i, b := range bindings {
		if (b.Subject.Principal == "") == (b.Subject.Group == "") {
			return nil, fmt.Errorf("binding %d: name exactly one principal or group", i+1)
		}
		perms, ok := roles[b.Role]
		if !ok {
			return nil, fmt.Errorf("binding %d: unknown role %q", i+1, b.Role)
		}
		if b.Scope.Kind != ScopeAll && b.Scope.Kind != ScopeProject {
			return nil, fmt.Errorf("binding %d: scope %s: %w", i+1, b.Scope, ErrReservedScope)
		}
		p.bindings = append(p.bindings, binding{Binding: b, permissions: perms})
	}
	return p, nil
}

// Bindings returns the policy's bindings in their order.
func (p *Policy) Bindings() []Binding {
	out := make([]Binding, len(p.bindings))
	for i, b := range p.bindings {
		out[i] = b.Binding
	}
	return out
}

// Extend returns a policy with p's bindings followed by extra, which
// NewPolicy validates the same way.
func (p *Policy) Extend(extra []Binding) (*Policy, error) {
	return NewPolicy(append(p.Bindings(), extra...))
}

// Local is the policy of a platform composed without a server config (the CLI's
// in-process commands, tests): the local owner is admin everywhere.
func Local(owner string) *Policy {
	p, _ := NewPolicy([]Binding{{Subject: Subject{Principal: owner}, Role: "admin", Scope: All}})
	return p
}

// Allowed reports whether id holds perm on resources in scope.
func (p *Policy) Allowed(id Identity, perm Permission, scope Scope) bool {
	for _, b := range p.bindings {
		if b.matches(id) && b.Scope.Covers(scope) && (slices.Contains(b.permissions, perm) || slices.Contains(b.permissions, Admin)) {
			return true
		}
	}
	return false
}

// Scopes returns the scopes in which id holds perm, for filtering lists.
func (p *Policy) Scopes(id Identity, perm Permission) []Scope {
	var out []Scope
	for _, b := range p.bindings {
		if b.matches(id) && (slices.Contains(b.permissions, perm) || slices.Contains(b.permissions, Admin)) && !slices.Contains(out, b.Scope) {
			out = append(out, b.Scope)
		}
	}
	return out
}

// Projects returns the projects in which id holds perm; all reports that a
// binding in scope * holds it, so every project is covered.
func (p *Policy) Projects(id Identity, perm Permission) (projects []string, all bool) {
	for _, s := range p.Scopes(id, perm) {
		switch s.Kind {
		case ScopeAll:
			return nil, true
		case ScopeProject:
			if !slices.Contains(projects, s.ID) {
				projects = append(projects, s.ID)
			}
		}
	}
	slices.Sort(projects)
	return projects, false
}

// Permitted returns the catalog permissions id holds in scope, for clients
// that show only what a user may do.
func (p *Policy) Permitted(id Identity, scope Scope) []Permission {
	var out []Permission
	for _, perm := range Permissions() {
		if perm != Admin && p.Allowed(id, perm, scope) {
			out = append(out, perm)
		}
	}
	return out
}
