package platform

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func teamAccess(t *testing.T) *Policy {
	t.Helper()
	access, err := NewPolicy([]Binding{{Subject: Subject{Group: "admins"}, Role: "admin", Scope: ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func holds(s *Service, c Caller, perm authz.Permission) bool {
	return slices.Contains(s.Permissions(c), string(perm))
}

// TestManagedRoleBindingsExtendTheServerConfig: a managed binding grants what
// its role holds, on every instance after it follows the change feed;
// listing shows server config bindings first and the roles; removing the
// binding takes the grant away; only access.manage lists or changes
// bindings, and each change is audited.
func TestManagedRoleBindingsExtendTheServerConfig(t *testing.T) {
	data := memory.NewData()
	opts := Options{Shared: true, Access: teamAccess(t)}
	a := openPlatform(t, data, opts)
	b := openPlatform(t, data, opts)
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	alice := Caller{Principal: Principal{ID: UserID("idp", "alice"), Kind: PrincipalUser, Groups: []string{"platform"}}, Client: "http"}

	if holds(a, alice, authz.RunSubmit) {
		t.Fatal("alice submits runs before any binding")
	}
	if _, err := a.ListRoleBindings(ctx, alice); ErrorCode(err) != CodeForbidden {
		t.Fatalf("alice listing bindings = %v", err)
	}
	if _, err := a.AddRoleBinding(ctx, alice, AddRoleBindingRequest{Group: "platform", Role: "admin"}); ErrorCode(err) != CodeForbidden {
		t.Fatalf("alice granting herself admin = %v", err)
	}
	list, err := a.ListRoleBindings(ctx, root)
	if err != nil || list.Revision != 0 || len(list.Bindings) != 1 || list.Bindings[0].Source != SourceConfig || list.Bindings[0].Group != "admins" {
		t.Fatalf("bindings before a change = %+v, %v", list, err)
	}
	if !slices.ContainsFunc(list.Roles, func(r RoleInfo) bool { return r.Name == "engineer" && slices.Contains(r.Permissions, "run.submit") }) {
		t.Errorf("roles = %+v", list.Roles)
	}

	list, err = a.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 0, Group: "platform", Role: "engineer"})
	if err != nil || list.Revision != 1 || len(list.Bindings) != 2 {
		t.Fatalf("adding = %+v, %v", list, err)
	}
	added := list.Bindings[1]
	if added.Source != SourceManaged || added.Scope != "*" || added.CreatedBy != root.Principal.ID || added.Created == nil {
		t.Errorf("added binding = %+v", added)
	}
	if !holds(a, alice, authz.RunSubmit) {
		t.Error("alice cannot submit runs on the instance that added the binding")
	}
	if err := b.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if !holds(b, alice, authz.RunSubmit) {
		t.Error("alice cannot submit runs on the other instance after it followed the feed")
	}

	if _, err := b.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 0, Group: "ops", Role: "viewer"}); ErrorCode(err) != CodeConflict {
		t.Errorf("adding at a stale revision = %v", err)
	}
	if _, err := b.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 1, Group: "platform", Role: "engineer", Scope: "*"}); ErrorCode(err) != CodeConflict {
		t.Errorf("adding a managed duplicate = %v", err)
	}
	if _, err := b.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 1, Group: "admins", Role: "admin"}); ErrorCode(err) != CodeConflict {
		t.Errorf("adding a server config duplicate = %v", err)
	}
	if _, err := b.RemoveRoleBinding(ctx, root, 1, "config-1"); ErrorCode(err) != CodeForbidden {
		t.Errorf("removing a server config binding = %v", err)
	}
	if _, err := b.RemoveRoleBinding(ctx, root, 1, "00000000000000000000000000000000"); ErrorCode(err) != CodeNotFound {
		t.Errorf("removing an unknown binding = %v", err)
	}
	list, err = b.RemoveRoleBinding(ctx, root, 1, added.ID)
	if err != nil || list.Revision != 2 || len(list.Bindings) != 1 {
		t.Fatalf("removing = %+v, %v", list, err)
	}
	if err := a.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if holds(a, alice, authz.RunSubmit) || holds(b, alice, authz.RunSubmit) {
		t.Error("alice still submits runs after the binding was removed")
	}

	page, err := a.ListAuditEvents(ctx, root, AuditFilter{Principal: root.Principal.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]int{}
	for _, e := range page.Events {
		if e.Outcome == AuditAllowed && e.Code == "" {
			ops[e.Operation]++
		}
	}
	if ops["AddRoleBinding"] != 1 || ops["RemoveRoleBinding"] != 1 || ops["ListRoleBindings"] != 1 {
		t.Errorf("successful audited operations = %v", ops)
	}
}

// TestRoleBindingRequestsAreValidated: a request names one subject, a
// built-in role and scope * or project:<id> of a known project; a project
// binding grants only in its project.
func TestRoleBindingRequestsAreValidated(t *testing.T) {
	s := openPlatform(t, memory.NewData(), Options{Access: teamAccess(t)})
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "cli"}
	for name, req := range map[string]AddRoleBindingRequest{
		"no subject":      {Role: "viewer"},
		"two subjects":    {Group: "g", Principal: "service:ci", Role: "viewer"},
		"unknown role":    {Group: "g", Role: "owner"},
		"bad scope":       {Group: "g", Role: "viewer", Scope: "team:x"},
		"reserved scope":  {Group: "g", Role: "viewer", Scope: "fleet-group:edge"},
		"bad principal":   {Principal: "alice", Role: "viewer"},
		"control chars":   {Group: "g\a", Role: "viewer"},
		"padded":          {Group: " g", Role: "viewer"},
		"unknown project": {Group: "g", Role: "viewer", Scope: "project:payments"},
	} {
		if _, err := s.AddRoleBinding(ctx, root, req); ErrorCode(err) != CodeInvalidRequest {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := s.CreateProject(ctx, root, CreateProjectRequest{ID: "payments", Name: "Payments"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Principal: "service:ci", Role: "engineer", Scope: "project:payments"})
	if err != nil || list.Bindings[1].Scope != "project:payments" {
		t.Fatalf("project binding = %+v, %v", list, err)
	}
	ci := identity(Principal{ID: "service:ci", Kind: PrincipalService})
	p := s.effective.Load()
	if !p.Allowed(ci, authz.RunSubmit, authz.Scope{Kind: authz.ScopeProject, ID: "payments"}) || p.Allowed(ci, authz.RunSubmit, authz.Scope{Kind: authz.ScopeProject, ID: "default"}) {
		t.Error("a project binding grants outside its project or not in it")
	}
}

// TestRemovingYourOwnAccessManagementIsRefused: an admin granted only by a
// managed binding cannot remove it; another admin can. A server config admin
// removes managed admin bindings freely.
func TestRemovingYourOwnAccessManagementIsRefused(t *testing.T) {
	data := memory.NewData()
	s := openPlatform(t, data, Options{Access: teamAccess(t)})
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	bob := Caller{Principal: Principal{ID: UserID("idp", "bob"), Kind: PrincipalUser, Groups: []string{"sre"}}, Client: "http"}

	list, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	id := list.Bindings[1].ID
	if _, err := s.RemoveRoleBinding(ctx, bob, list.Revision, id); ErrorCode(err) != CodeConflict {
		t.Fatalf("bob removing his own admin binding = %v", err)
	}
	if !holds(s, bob, authz.AccessManage) {
		t.Fatal("bob lost access management after a refused removal")
	}
	if _, err := s.RemoveRoleBinding(ctx, root, list.Revision, id); err != nil {
		t.Fatalf("root removing bob's admin binding = %v", err)
	}
	if holds(s, bob, authz.AccessManage) {
		t.Error("bob still manages access")
	}

	// Bindings survive a restart.
	if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 2, Group: "sre", Role: "viewer"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	again := openPlatform(t, data, Options{Access: teamAccess(t)})
	if !holds(again, bob, authz.RunRead) {
		t.Error("a managed binding did not survive reopening")
	}
}

// TestProjectAccessManagersStayInTheirProject: admin in one project, from
// the server config or a managed binding, lists and changes that project's
// bindings only: it cannot grant elsewhere, grant a role it does not hold,
// see or remove other bindings, or remove its own last access management.
func TestProjectAccessManagersStayInTheirProject(t *testing.T) {
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Group: "admins"}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Group: "payments-admins"}, Role: "admin", Scope: authz.Scope{Kind: authz.ScopeProject, ID: "payments"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := openPlatform(t, memory.NewData(), Options{Access: access})
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	pat := Caller{Principal: Principal{ID: UserID("idp", "pat"), Kind: PrincipalUser, Groups: []string{"payments-admins"}}, Client: "http"}
	bob := Caller{Principal: Principal{ID: UserID("idp", "bob"), Kind: PrincipalUser, Groups: []string{"sre"}}, Client: "http"}
	if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "admin", Scope: "project:payments"}); ErrorCode(err) != CodeInvalidRequest {
		t.Errorf("binding in an unknown project = %v", err)
	}
	for _, id := range []string{"payments", "checkout"} {
		list, err := s.ListProjects(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateProject(ctx, root, CreateProjectRequest{Revision: list.Revision, ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "admin", Scope: "project:payments"})
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: list.Revision, Group: "web", Role: "viewer", Scope: "project:checkout"})
	if err != nil {
		t.Fatal(err)
	}
	other := ""
	for _, b := range checkout.Bindings {
		if b.Group == "web" {
			other = b.ID
		}
	}
	revision := checkout.Revision
	for name, c := range map[string]Caller{"server config project admin": pat, "managed project admin": bob} {
		if s.effective.Load().Allowed(identity(c.Principal), authz.AccessManage, authz.All) {
			t.Errorf("%s holds access.manage everywhere", name)
		}
		got, err := s.ListRoleBindings(ctx, c)
		if err != nil {
			t.Fatalf("%s listing = %v", name, err)
		}
		for _, b := range got.Bindings {
			if b.Scope != "project:payments" {
				t.Errorf("%s sees binding %+v", name, b)
			}
		}
		for what, req := range map[string]AddRoleBindingRequest{
			"platform admin":          {Principal: c.Principal.ID, Role: "admin"},
			"admin in another":        {Principal: c.Principal.ID, Role: "admin", Scope: "project:checkout"},
			"admin in the library":    {Principal: c.Principal.ID, Role: "admin", Scope: "project:library"},
			"admin in an unknown one": {Principal: c.Principal.ID, Role: "admin", Scope: "project:nowhere"},
		} {
			req.Revision = revision
			if _, err := s.AddRoleBinding(ctx, c, req); ErrorCode(err) != CodeForbidden && ErrorCode(err) != CodeInvalidRequest {
				t.Errorf("%s granting %s = %v", name, what, err)
			}
		}
		if _, err := s.RemoveRoleBinding(ctx, c, revision, "config-1"); ErrorCode(err) != CodeForbidden {
			t.Errorf("%s removing the platform admin = %v", name, err)
		}
		if _, err := s.RemoveRoleBinding(ctx, c, revision, other); ErrorCode(err) != CodeNotFound {
			t.Errorf("%s removing another project's binding = %v", name, err)
		}
	}
	added, err := s.AddRoleBinding(ctx, pat, AddRoleBindingRequest{Revision: revision, Group: "payments-devs", Role: "engineer", Scope: "project:payments"})
	if err != nil {
		t.Fatalf("project admin granting in its project = %v", err)
	}
	own := ""
	for _, b := range added.Bindings {
		if b.Group == "sre" {
			own = b.ID
		}
	}
	if _, err := s.RemoveRoleBinding(ctx, bob, added.Revision, own); ErrorCode(err) != CodeConflict {
		t.Errorf("managed project admin removing its own last binding = %v", err)
	}
	if _, err := s.RemoveRoleBinding(ctx, pat, added.Revision, own); err != nil {
		t.Errorf("server config project admin removing a managed project admin = %v", err)
	}
}

// TestRevokingABindingStopsAPITokens: a personal API token keeps its
// creator's groups, but bindings are read on every request, so removing the
// binding takes the token's permissions away at once, on every instance.
func TestRevokingABindingStopsAPITokens(t *testing.T) {
	data := memory.NewData()
	opts := Options{Shared: true, Access: teamAccess(t)}
	a := openPlatform(t, data, opts)
	b := openPlatform(t, data, opts)
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	bob := Caller{Principal: Principal{ID: UserID("idp", "bob"), Kind: PrincipalUser, Groups: []string{"sre"}}, Client: "http"}
	list, err := a.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "engineer"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := a.CreateAPIToken(ctx, bob, CreateAPITokenRequest{Label: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	viaToken := func(s *Service) Caller {
		t.Helper()
		p, err := s.AuthenticateToken(ctx, created.Secret)
		if err != nil {
			t.Fatal(err)
		}
		return Caller{Principal: p, Client: "cli"}
	}
	if !holds(b, viaToken(b), authz.RunSubmit) {
		t.Fatal("the token does not act with its creator's binding")
	}
	if _, err := a.RemoveRoleBinding(ctx, root, list.Revision, list.Bindings[1].ID); err != nil {
		t.Fatal(err)
	}
	if holds(a, viaToken(a), authz.RunSubmit) || holds(a, viaToken(a), authz.RunRead) {
		t.Error("the token keeps the removed binding's permissions")
	}
	if err := b.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if holds(b, viaToken(b), authz.RunSubmit) {
		t.Error("the token keeps the removed binding's permissions on the other instance")
	}
}

// TestBindingSubjectsMatchExactly: a group binding grants only the exact
// group name: no case folding, no trimming and no look-alike characters;
// invisible characters are refused when granting.
func TestBindingSubjectsMatchExactly(t *testing.T) {
	s := openPlatform(t, memory.NewData(), Options{Access: teamAccess(t)})
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	list, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "engineer"})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"SRE", "Sre", "sre ", " sre", "ѕre", "sre\u200b", "sr", "sre-team"} {
		c := Caller{Principal: Principal{ID: UserID("idp", "eve"), Kind: PrincipalUser, Groups: []string{group}}, Client: "http"}
		if holds(s, c, authz.RunRead) {
			t.Errorf("group %q matches the binding for sre", group)
		}
	}
	principal := Caller{Principal: Principal{ID: "user:sre", Kind: PrincipalUser}, Client: "http"}
	if holds(s, principal, authz.RunRead) {
		t.Error("a principal named like the group matches the group binding")
	}
	for _, name := range []string{"sre\u200b", "\u202esre", "sre\u00ad", "sre\n"} {
		if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: list.Revision, Group: name, Role: "viewer"}); ErrorCode(err) != CodeInvalidRequest {
			t.Errorf("granting to %q = %v", name, err)
		}
	}
	if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: list.Revision, Group: strings.Repeat("g", 257), Role: "viewer"}); ErrorCode(err) != CodeInvalidRequest {
		t.Errorf("granting to a 257-character group = %v", err)
	}
	if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: list.Revision, Group: "Platform Team", Role: "viewer"}); err != nil {
		t.Errorf("granting to a group with an inner space = %v", err)
	}
}

// TestLockoutGuardCountsEveryGrant: an admin holding access.manage through
// two managed bindings may remove either, but not the last; a stale
// revision is refused before the guard runs.
func TestLockoutGuardCountsEveryGrant(t *testing.T) {
	s := openPlatform(t, memory.NewData(), Options{Access: teamAccess(t)})
	ctx := context.Background()
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "http"}
	bob := Caller{Principal: Principal{ID: UserID("idp", "bob"), Kind: PrincipalUser, Groups: []string{"sre"}}, Client: "http"}
	if _, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Group: "sre", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.AddRoleBinding(ctx, root, AddRoleBindingRequest{Revision: 1, Principal: bob.Principal.ID, Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	group, own := list.Bindings[1].ID, list.Bindings[2].ID
	if _, err := s.RemoveRoleBinding(ctx, bob, 1, group); ErrorCode(err) != CodeConflict {
		t.Errorf("removing at a stale revision = %v", err)
	}
	list, err = s.RemoveRoleBinding(ctx, bob, list.Revision, group)
	if err != nil {
		t.Fatalf("removing one of two grants = %v", err)
	}
	if _, err := s.RemoveRoleBinding(ctx, bob, list.Revision, own); ErrorCode(err) != CodeConflict {
		t.Errorf("removing the last grant = %v", err)
	}
	if !holds(s, bob, authz.AccessManage) {
		t.Error("bob lost access management")
	}
}
