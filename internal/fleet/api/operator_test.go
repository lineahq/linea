package api

import (
	"context"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/plugins"
)

type fakeControl struct{ fleet.ControlRepository }

func (fakeControl) PutGroup(_ context.Context, _ string, g fleet.Group, _ int64) (fleet.Group, error) {
	g.Revision = 1
	return g, nil
}
func (fakeControl) DeleteGroup(context.Context, string, string, int64) error { return nil }

// The operator manages groups, configurations and rollouts like an admin, but
// neither credentials nor plugin lifecycle.
func TestOperatorRoleScope(t *testing.T) {
	repo := &fakeRollouts{fakeConfigs: fakeConfigs{bodies: map[int][]byte{}}}
	creds := &fakeCredentials{c: auth.Credential{ID: strings.Repeat("72", 16), Role: "agent", RemoteConfig: true}}
	store := fleet.NewStore(3)
	h := NewAuthorized(store, roles{}, ServerInfo{ServerID: "test"}, fakeControl{}, &auth.Service{Repository: creds})
	h.SetConfigManagement(repo, nil)
	h.SetPlugins(plugins.New(context.Background(), nil, store))
	group, config, assignment, rollout := strings.Repeat("a1", 16), strings.Repeat("c1", 16), strings.Repeat("e1", 16), strings.Repeat("f1", 16)
	versions := "/api/v1/configs/" + config + "/versions"
	type step struct {
		method, path string
		body         any
	}
	// Writes the operator performs, in an order where each step has what it needs.
	allowed := []step{
		{"PUT", "/api/v1/groups/" + group, map[string]any{"name": "g", "selector": "a=b"}},
		{"POST", "/api/v1/configs", map[string]string{"id": config, "name": "base"}},
		{"POST", versions, map[string]string{"body": "receivers:\n  otlp: {}\n"}},
		{"POST", "/api/v1/config-assignments", map[string]any{"id": assignment, "name": "base", "config_id": config, "version": 1, "layer": 10, "selector": "a=b", "credential_labels": map[string]string{"env": "prod"}}},
		{"POST", "/api/v1/rollouts", map[string]any{"assignment_id": assignment, "to_version": 1}},
		{"POST", "/api/v1/rollouts/" + rollout + ":pause", nil},
		{"POST", "/api/v1/agents/" + strings.Repeat("a1", 16) + "/config:retry", nil},
	}
	for _, s := range allowed {
		admin := call(t, h, s.method, s.path, "admin", s.body)
		operator := call(t, h, s.method, s.path, "operator", s.body)
		if admin.Code >= 300 || operator.Code != admin.Code {
			t.Fatalf("%s %s: operator %d, admin %d", s.method, s.path, operator.Code, admin.Code)
		}
		if reader := call(t, h, s.method, s.path, "reader", s.body); reader.Code != 403 {
			t.Fatalf("%s %s: reader %d", s.method, s.path, reader.Code)
		}
	}
	if w := call(t, h, "GET", versions+"/1", "operator", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"body"`) {
		t.Fatalf("operator version read: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "GET", "/api/v1/agents", "operator", nil); w.Code != 200 {
		t.Fatalf("operator read: %d", w.Code)
	}

	forbidden := []step{
		{"GET", "/api/v1/credentials", nil},
		{"POST", "/api/v1/credentials", map[string]string{"id": strings.Repeat("b1", 16), "name": "x", "role": "reader"}},
		{"GET", "/api/v1/credentials/" + creds.c.ID, nil},
		{"PATCH", "/api/v1/credentials/" + creds.c.ID, map[string]bool{"remote_config": false}},
		{"DELETE", "/api/v1/credentials/" + creds.c.ID, nil},
		{"POST", "/api/v1/plugins/example/lifecycle", map[string]string{"desired": "disabled"}},
	}
	for _, s := range forbidden {
		if w := call(t, h, s.method, s.path, "operator", s.body); w.Code != 403 {
			t.Fatalf("%s %s: operator %d, want 403", s.method, s.path, w.Code)
		}
	}
	if creds.c.RemoteConfig != true {
		t.Fatal("operator changed a credential")
	}
}
