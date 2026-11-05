package authz

import (
	"errors"
	"slices"
	"testing"
)

func TestDefaultDeny(t *testing.T) {
	p, err := NewPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, perm := range Permissions() {
		if p.Allowed(Identity{ID: "user:alice"}, perm, All) {
			t.Errorf("an empty policy allows %s", perm)
		}
	}
}

func TestBindingsGrantTheirRolesPermissions(t *testing.T) {
	p, err := NewPolicy([]Binding{
		{Subject: Subject{Group: "platform"}, Role: "engineer", Scope: All},
		{Subject: Subject{Principal: "user:bob"}, Role: "viewer", Scope: All},
		{Subject: Subject{Principal: "service:ci"}, Role: "fleet-operator", Scope: All},
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := Identity{ID: "user:alice", Groups: []string{"platform"}}
	bob := Identity{ID: "user:bob", Groups: []string{"sales"}}
	ci := Identity{ID: "service:ci"}
	for _, c := range []struct {
		id    Identity
		perm  Permission
		allow bool
	}{
		{alice, RunSubmit, true},
		{alice, CaptureStart, true},
		{alice, CaptureRaw, false},
		{alice, FleetRollout, false},
		{alice, RuntimeManage, false},
		{bob, RunRead, true},
		{bob, RunSubmit, false},
		{ci, FleetRollout, true},
		{ci, Admin, false},
		{Identity{ID: "user:carol"}, RunRead, false},
	} {
		if got := p.Allowed(c.id, c.perm, All); got != c.allow {
			t.Errorf("%s %s = %v", c.id.ID, c.perm, got)
		}
	}
	if got := p.Scopes(alice, RunRead); !slices.Equal(got, []Scope{All}) {
		t.Errorf("scopes = %v", got)
	}
	if got := p.Scopes(bob, RunSubmit); got != nil {
		t.Errorf("scopes = %v", got)
	}
}

func TestAdminHoldsEveryPermission(t *testing.T) {
	p := Local("local-owner")
	for _, perm := range Permissions() {
		if !p.Allowed(Identity{ID: "local-owner"}, perm, All) {
			t.Errorf("admin lacks %s", perm)
		}
	}
	if p.Allowed(Identity{ID: "user:local-owner"}, RunRead, All) {
		t.Error("another principal is the local owner")
	}
	if got := p.Permitted(Identity{ID: "local-owner"}, All); len(got) != len(Permissions())-1 {
		t.Errorf("permitted = %v", got)
	}
}

func TestPolicyRefusesInvalidBindings(t *testing.T) {
	for name, b := range map[string]Binding{
		"no subject":     {Role: "viewer", Scope: All},
		"two subjects":   {Subject: Subject{Principal: "user:a", Group: "g"}, Role: "viewer", Scope: All},
		"unknown role":   {Subject: Subject{Group: "g"}, Role: "owner", Scope: All},
		"reserved scope": {Subject: Subject{Group: "g"}, Role: "viewer", Scope: Scope{Kind: ScopeFleetGroup, ID: "edge"}},
	} {
		if _, err := NewPolicy([]Binding{b}); err == nil {
			t.Errorf("%s: accepted", name)
		} else if name == "reserved scope" && !errors.Is(err, ErrReservedScope) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestScopes(t *testing.T) {
	for v, want := range map[string]Scope{
		"*":                {Kind: ScopeAll},
		"project:default":  {Kind: ScopeProject, ID: "default"},
		"fleet-group:edge": {Kind: ScopeFleetGroup, ID: "edge"},
	} {
		got, err := ParseScope(v)
		if err != nil || got != want || got.String() != v {
			t.Errorf("%s = %v, %v", v, got, err)
		}
	}
	for _, v := range []string{"", "project:", "team:x", "project"} {
		if _, err := ParseScope(v); err == nil {
			t.Errorf("%q parsed", v)
		}
	}
	p := Scope{Kind: ScopeProject, ID: "a"}
	if !All.Covers(p) || !p.Covers(p) || p.Covers(All) || p.Covers(Scope{Kind: ScopeProject, ID: "b"}) {
		t.Error("covers")
	}
}

func TestProjectBindings(t *testing.T) {
	a, b := Scope{Kind: ScopeProject, ID: "a"}, Scope{Kind: ScopeProject, ID: "b"}
	p, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: b},
		{Subject: Subject{Principal: "user:alice"}, Role: "viewer", Scope: a},
		{Subject: Subject{Group: "platform"}, Role: "viewer", Scope: All},
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := Identity{ID: "user:alice"}
	if !p.Allowed(alice, RunSubmit, b) || p.Allowed(alice, RunSubmit, a) || !p.Allowed(alice, RunRead, a) {
		t.Error("project grants")
	}
	if got, all := p.Projects(alice, RunRead); all || !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("projects = %v, %v", got, all)
	}
	if got, all := p.Projects(alice, ArtifactWrite); all || !slices.Equal(got, []string{"b"}) {
		t.Errorf("write projects = %v, %v", got, all)
	}
	if got, all := p.Projects(Identity{ID: "user:x", Groups: []string{"platform"}}, RunRead); !all || got != nil {
		t.Errorf("platform projects = %v, %v", got, all)
	}
	if got, all := p.Projects(Identity{ID: "user:x"}, RunRead); all || len(got) != 0 {
		t.Errorf("unbound projects = %v, %v", got, all)
	}
	if !ProjectScoped(CaptureRaw) || ProjectScoped(FleetRead) || ProjectScoped(Admin) {
		t.Error("project-scoped permissions")
	}
}
