package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// Role bindings come from two sources. The server config's are Options.Access:
// fixed while the server runs and never changed through an operation, so
// the admins it names cannot be locked out. Managed bindings live in the
// repository, change through the operations below and reach every instance
// through the change feed. A principal holds what either source grants.

// RoleBindingSource names where a binding comes from.
type RoleBindingSource string

const (
	SourceConfig  RoleBindingSource = "config"
	SourceManaged RoleBindingSource = "managed"
)

// configBindingPrefix starts the IDs listed for server config bindings, which are
// their position in the file.
const configBindingPrefix = "config-"

// RoleBinding grants Role in Scope to exactly one of Group and Principal.
// CreatedBy and Created are empty for a server config binding.
type RoleBinding struct {
	ID        string
	Source    RoleBindingSource
	Group     string
	Principal string
	Role      string
	Scope     string
	CreatedBy string
	Created   *time.Time
}

// RoleInfo is a built-in role and its permissions.
type RoleInfo struct {
	Name        string
	Permissions []string
}

// RoleBindingList is every binding, server config ones first, the managed set's
// revision and the built-in roles.
type RoleBindingList struct {
	Revision uint64
	Bindings []RoleBinding
	Roles    []RoleInfo
}

// AddRoleBindingRequest grants Role in Scope ("*" when empty) to Group or
// Principal at the managed set's Revision.
type AddRoleBindingRequest struct {
	Revision  uint64
	Group     string
	Principal string
	Role      string
	Scope     string
}

func (s *Service) roleBindingStore() (artifact.RoleBindings, error) {
	if s.repository == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "role bindings need a workspace"}
	}
	return s.repository, nil
}

// managedProjects is the scope of the caller's access management: nil when
// it holds access.manage everywhere, otherwise the projects it holds it in.
func (s *Service) managedProjects(caller Caller) []string {
	return s.projects(caller, authz.AccessManage)
}

// bindingVisible reports whether a binding of scope falls within managed,
// a managedProjects result.
func bindingVisible(managed []string, scope string) bool {
	if managed == nil {
		return true
	}
	project, ok := strings.CutPrefix(scope, "project:")
	return ok && slices.Contains(managed, project)
}

// ListRoleBindings returns every binding within the caller's access
// management and the built-in roles. A project's access manager sees the
// bindings of its projects only.
func (s *Service) ListRoleBindings(ctx context.Context, caller Caller) (_ *RoleBindingList, err error) {
	defer s.audit(&caller, "ListRoleBindings", &err)()
	if err := s.begin(ctx, caller, authz.AccessManage); err != nil {
		return nil, err
	}
	store, err := s.roleBindingStore()
	if err != nil {
		return nil, err
	}
	set, err := store.RoleBindings(ctx)
	if err != nil {
		return nil, artifactError(err)
	}
	s.applyRoleBindings(set)
	return s.roleBindingList(caller, set), nil
}

// AddRoleBinding adds a managed binding. A binding the server config or the
// managed set already grants is a conflict, as is a stale revision. A
// project's access manager grants in its projects only, and only roles
// whose every permission it holds there.
func (s *Service) AddRoleBinding(ctx context.Context, caller Caller, req AddRoleBindingRequest) (_ *RoleBindingList, err error) {
	defer s.audit(&caller, "AddRoleBinding", &err)()
	if err := s.begin(ctx, caller, authz.AccessManage); err != nil {
		return nil, err
	}
	store, err := s.roleBindingStore()
	if err != nil {
		return nil, err
	}
	binding, err := validateRoleBinding(req)
	if err != nil {
		return nil, err
	}
	if err := s.grantable(caller, binding); err != nil {
		return nil, err
	}
	for _, b := range s.access.Bindings() {
		if b == binding {
			return nil, &Error{Code: CodeConflict, Message: "conflict: the server config already grants this role binding"}
		}
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	record := artifact.RoleBindingRecord{ID: hex.EncodeToString(id[:]), Group: binding.Subject.Group, Principal: binding.Subject.Principal,
		Role: binding.Role, Scope: binding.Scope.String(), CreatedBy: caller.Actor().Principal, Created: s.now().UTC().Truncate(time.Second)}
	set, err := store.AddRoleBinding(ctx, req.Revision, record)
	if err != nil {
		return nil, roleBindingError(err)
	}
	caller.touch(record.ID)
	s.applyRoleBindings(set)
	return s.roleBindingList(caller, set), nil
}

// grantable refuses a binding outside the caller's access management: a
// project that does not exist, a scope it does not manage, or a role with
// a permission it does not hold in that scope.
func (s *Service) grantable(caller Caller, b authz.Binding) error {
	if b.Scope.Kind == authz.ScopeProject {
		if _, ok := s.projectRecord(b.Scope.ID); !ok {
			return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: no project %s", b.Scope.ID)}
		}
	}
	policy := s.effective.Load()
	id := identity(caller.Principal)
	allowed := policy.Allowed(id, authz.AccessManage, b.Scope)
	caller.decide(authz.AccessManage, b.Scope.String(), allowed)
	if !allowed {
		return &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: granting in scope %s needs the %s permission there", b.Scope, authz.AccessManage)}
	}
	if policy.Allowed(id, authz.AccessManage, authz.All) {
		return nil
	}
	for _, r := range authz.Roles() {
		if r.Name != b.Role {
			continue
		}
		for _, p := range r.Permissions {
			if !policy.Allowed(id, p, b.Scope) {
				return &Error{Code: CodeForbidden, Message: fmt.Sprintf("forbidden: role %s grants %s, which you do not hold in scope %s", r.Name, p, b.Scope)}
			}
		}
	}
	return nil
}

// RemoveRoleBinding removes managed binding id. Server config bindings cannot
// be removed, and neither can a binding whose removal would leave the
// caller unable to manage access.
func (s *Service) RemoveRoleBinding(ctx context.Context, caller Caller, revision uint64, id string) (_ *RoleBindingList, err error) {
	defer s.audit(&caller, "RemoveRoleBinding", &err)()
	if err := s.begin(ctx, caller, authz.AccessManage); err != nil {
		return nil, err
	}
	if strings.HasPrefix(id, configBindingPrefix) {
		return nil, &Error{Code: CodeForbidden, Message: "forbidden: server config bindings change only in the server config"}
	}
	store, err := s.roleBindingStore()
	if err != nil {
		return nil, err
	}
	caller.touch(id)
	current, err := store.RoleBindings(ctx)
	if err != nil {
		return nil, artifactError(err)
	}
	managed := s.managedProjects(caller)
	for _, b := range current.Bindings {
		if b.ID == id && !bindingVisible(managed, b.Scope) {
			// Outside the caller's access management it does not exist.
			return nil, roleBindingError(artifact.ErrNotFound)
		}
	}
	if current.Revision == revision {
		rest := artifact.RoleBindingSet{Revision: current.Revision}
		var removed artifact.RoleBindingRecord
		for _, b := range current.Bindings {
			if b.ID != id {
				rest.Bindings = append(rest.Bindings, b)
			} else {
				removed = b
			}
		}
		// The caller keeps managing access where it did: everywhere, or in
		// the removed binding's project.
		keep := authz.All
		if managed != nil {
			keep, _ = authz.ParseScope(removed.Scope)
		}
		if len(rest.Bindings) < len(current.Bindings) && !s.extended(rest).Allowed(identity(caller.Principal), authz.AccessManage, keep) {
			return nil, &Error{Code: CodeConflict, Message: "conflict: removing this binding would take away your own access management; another admin can remove it"}
		}
	}
	set, err := store.RemoveRoleBinding(ctx, revision, id)
	if err != nil {
		return nil, roleBindingError(err)
	}
	s.applyRoleBindings(set)
	return s.roleBindingList(caller, set), nil
}

func roleBindingError(err error) error {
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		return &Error{Code: CodeNotFound, Message: "not found: no managed role binding has this ID", cause: err}
	case errors.Is(err, artifact.ErrConflict):
		return &Error{Code: CodeConflict, Message: "conflict: the role bindings changed or already grant this; reload", cause: err}
	case errors.Is(err, artifact.ErrLimit):
		return &Error{Code: CodeResourceExhausted, Message: fmt.Sprintf("resource exhausted: at most %d managed role bindings", artifact.MaxRoleBindings), cause: err}
	}
	return artifactError(err)
}

// validateRoleBinding reads a request as a binding the policy accepts: one
// subject, a built-in role, and scope * or project:<id>.
func validateRoleBinding(req AddRoleBindingRequest) (authz.Binding, error) {
	invalid := func(message string) (authz.Binding, error) {
		return authz.Binding{}, &Error{Code: CodeInvalidRequest, Message: "invalid request: " + message}
	}
	if (req.Group == "") == (req.Principal == "") {
		return invalid("name exactly one group or principal")
	}
	subject := req.Group + req.Principal
	if len(subject) > 256 || strings.ContainsFunc(subject, func(r rune) bool { return !unicode.IsPrint(r) }) || strings.TrimSpace(subject) != subject {
		return invalid("a group or principal is at most 256 printable characters without surrounding spaces")
	}
	if req.Principal != "" && !strings.HasPrefix(req.Principal, "user:") && !strings.HasPrefix(req.Principal, "service:") {
		return invalid("a principal is user:<id> or service:<id>, as token list and the audit log show it")
	}
	if req.Scope == "" {
		req.Scope = "*"
	}
	scope, err := authz.ParseScope(req.Scope)
	if err != nil {
		return invalid(err.Error())
	}
	b := authz.Binding{Subject: authz.Subject{Group: req.Group, Principal: req.Principal}, Role: req.Role, Scope: scope}
	if _, err := authz.NewPolicy([]authz.Binding{b}); err != nil {
		return invalid(strings.TrimPrefix(err.Error(), "binding 1: "))
	}
	return b, nil
}

// recoverRoleBindings reads the managed bindings when the service opens.
func (s *Service) recoverRoleBindings() error { return s.loadRoleBindings(s.ctx) }

// loadRoleBindings reads the managed bindings from the repository again.
func (s *Service) loadRoleBindings(ctx context.Context) error {
	if s.repository == nil {
		return nil
	}
	set, err := s.repository.RoleBindings(ctx)
	if err != nil {
		return err
	}
	s.applyRoleBindings(set)
	return nil
}

// applyRoleBindings makes set the managed bindings unless a newer revision
// is already applied.
func (s *Service) applyRoleBindings(set artifact.RoleBindingSet) {
	s.bindingMu.Lock()
	defer s.bindingMu.Unlock()
	if set.Revision < s.managed.Revision {
		return
	}
	s.managed = set
	s.effective.Store(s.extended(set))
}

// extended is the server config's policy with set's valid bindings. A stored
// binding the policy refuses, such as a role this release does not know,
// grants nothing.
func (s *Service) extended(set artifact.RoleBindingSet) *authz.Policy {
	var extra []authz.Binding
	for _, r := range set.Bindings {
		b, ok := policyBinding(r)
		if !ok {
			s.logError("managed role binding "+r.ID, errors.New("refused by the policy; it grants nothing"))
			continue
		}
		extra = append(extra, b)
	}
	p, err := s.access.Extend(extra)
	if err != nil {
		// Every binding was validated on its own above.
		s.logError("managed role bindings", err)
		return s.access
	}
	return p
}

func policyBinding(r artifact.RoleBindingRecord) (authz.Binding, bool) {
	scope, err := authz.ParseScope(r.Scope)
	if err != nil {
		return authz.Binding{}, false
	}
	b := authz.Binding{Subject: authz.Subject{Group: r.Group, Principal: r.Principal}, Role: r.Role, Scope: scope}
	if _, err := authz.NewPolicy([]authz.Binding{b}); err != nil {
		return authz.Binding{}, false
	}
	return b, true
}

func (s *Service) roleBindingList(caller Caller, set artifact.RoleBindingSet) *RoleBindingList {
	out := &RoleBindingList{Revision: set.Revision, Bindings: []RoleBinding{}, Roles: []RoleInfo{}}
	managed := s.managedProjects(caller)
	for i, b := range s.access.Bindings() {
		if !bindingVisible(managed, b.Scope.String()) {
			continue
		}
		out.Bindings = append(out.Bindings, RoleBinding{ID: fmt.Sprintf("%s%d", configBindingPrefix, i+1), Source: SourceConfig,
			Group: b.Subject.Group, Principal: b.Subject.Principal, Role: b.Role, Scope: b.Scope.String()})
	}
	for _, r := range set.Bindings {
		if !bindingVisible(managed, r.Scope) {
			continue
		}
		created := r.Created
		out.Bindings = append(out.Bindings, RoleBinding{ID: r.ID, Source: SourceManaged, Group: r.Group, Principal: r.Principal,
			Role: r.Role, Scope: r.Scope, CreatedBy: r.CreatedBy, Created: &created})
	}
	for _, r := range authz.Roles() {
		info := RoleInfo{Name: r.Name, Permissions: []string{}}
		for _, p := range r.Permissions {
			info.Permissions = append(info.Permissions, string(p))
		}
		out.Roles = append(out.Roles, info)
	}
	return out
}
