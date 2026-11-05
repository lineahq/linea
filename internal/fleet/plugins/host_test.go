package plugins

import (
	"context"
	"crypto/sha256"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

type groupControl struct {
	fleet.ControlRepository
	groups map[string]fleet.Group
}

func (c groupControl) GetGroup(_ context.Context, id string) (fleet.Group, error) {
	g, ok := c.groups[id]
	if !ok {
		return fleet.Group{}, fleet.ErrNotFound
	}
	return g, nil
}

type labelIntrospector struct{ token string }

func (i labelIntrospector) Introspect(_ context.Context, sum [32]byte) (auth.Introspection, error) {
	if sum != sha256.Sum256([]byte(i.token)) {
		return auth.Introspection{}, nil
	}
	return auth.Introspection{Active: true, CredentialID: strings.Repeat("c", 32), Role: "agent", Labels: map[string]string{"team": "edge"}}, nil
}

// readyHost serves a registry whose single entry is ready, so tests reach the
// host services through the public SDK client.
func readyHost(t *testing.T, store fleet.Repository, permissions ...string) (*Registry, pluginsdk.HostClient, pluginsdk.Session) {
	t.Helper()
	config := fixtureConfig("http://127.0.0.1:1")
	config.Permissions = permissions
	registry := New(context.Background(), []Config{config}, store)
	t.Cleanup(registry.Close)
	e := registry.entries[0]
	now := time.Now()
	e.info.InstanceID, e.info.State, e.info.CheckedAt = pluginsdk.RandomID(), "ready", &now
	server := httptest.NewServer(registry)
	t.Cleanup(server.Close)
	client := pluginsdk.HostClient{Endpoint: server.URL, Token: config.hostToken, InstanceID: e.info.InstanceID, HTTP: pluginsdk.NewHTTPClient()}
	session, err := client.Session(context.Background())
	if err != nil {
		t.Fatal("session:", err)
	}
	return registry, client, session
}

func TestHostServices(t *testing.T) {
	store := fleet.NewStore(10)
	ctx := context.Background()
	credential, other := strings.Repeat("c", 32), strings.Repeat("d", 32)
	seed := func(id, cred, pod string, connected bool) {
		if _, _, err := store.ConnectAgent(ctx, id, cred, fleet.Report{Attributes: map[string]string{"k8s.pod.name": pod, "k8s.namespace.name": "obs", "service.name": "collector"}}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if !connected {
			g, _ := store.GetAgent(ctx, id)
			_ = store.DisconnectAgent(ctx, id, g.Generation, time.Now())
		}
	}
	seed(strings.Repeat("1", 32), credential, "pod-a", true)
	seed(strings.Repeat("2", 32), credential, "pod-b", true)
	seed(strings.Repeat("3", 32), other, "pod-a", true)
	seed(strings.Repeat("4", 32), credential, "pod-c", false)
	registry, client, session := readyHost(t, store, pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect)
	staticGroup, selectorGroup := strings.Repeat("e", 32), strings.Repeat("f", 32)
	registry.SetHost(groupControl{groups: map[string]fleet.Group{
		staticGroup:   {ID: staticGroup, Kind: fleet.GroupStatic, Revision: 3},
		selectorGroup: {ID: selectorGroup, Kind: fleet.GroupSelector, Selector: "k8s.pod.name=pod-a", Revision: 7},
	}}, labelIntrospector{token: "agent-token"})

	connected := true
	page, err := client.Agents(ctx, session, pluginsdk.AgentQuery{Selector: "k8s.pod.name in (pod-a,pod-c)", Connected: &connected})
	if err != nil || len(page.Items) != 2 || page.Items[0].Attributes["k8s.namespace.name"] != "obs" {
		t.Fatalf("directory: %+v %v", page, err)
	}

	resolved, err := client.ResolveAgents(ctx, session, pluginsdk.ResolveRequest{CredentialID: credential, Attributes: map[string]string{"k8s.pod.name": "pod-a", "k8s.namespace.name": "obs"}})
	if err != nil || len(resolved.AgentIDs) != 1 || resolved.AgentIDs[0] != strings.Repeat("1", 32) || resolved.More {
		t.Fatalf("resolve by credential: %+v %v", resolved, err)
	}
	if resolved, err = client.ResolveAgents(ctx, session, pluginsdk.ResolveRequest{Attributes: map[string]string{"k8s.pod.name": "pod-a"}, Limit: 1}); err != nil || len(resolved.AgentIDs) != 1 || !resolved.More {
		t.Fatalf("ambiguous resolve not flagged: %+v %v", resolved, err)
	}
	if resolved, err = client.ResolveAgents(ctx, session, pluginsdk.ResolveRequest{CredentialID: credential, Attributes: map[string]string{"k8s.pod.name": "pod-c"}}); err != nil || len(resolved.AgentIDs) != 0 {
		t.Fatalf("disconnected agent resolved: %+v %v", resolved, err)
	}

	group, err := client.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{GroupID: selectorGroup, Connected: &connected})
	if err != nil || len(group.AgentIDs) != 2 || group.Revision != 7 {
		t.Fatalf("selector group: %+v %v", group, err)
	}
	if group, err = client.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{Selector: "service.name=collector"}); err != nil || len(group.AgentIDs) != 4 {
		t.Fatalf("selector expression: %+v %v", group, err)
	}
	if _, err = client.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{Selector: "service.name=collector", Limit: 3}); pluginsdk.StatusOf(err) != 422 {
		t.Fatalf("oversized group truncated: %v", err)
	}
	if _, err = client.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{GroupID: strings.Repeat("9", 32)}); pluginsdk.StatusOf(err) != 404 {
		t.Fatalf("unknown group: %v", err)
	}

	got, err := client.Introspect(ctx, session, "agent-token")
	if err != nil || !got.Active || got.CredentialID != credential || got.Labels["team"] != "edge" {
		t.Fatalf("introspect: %+v %v", got, err)
	}
	if got, err = client.Introspect(ctx, session, "other"); err != nil || got.Active || got.CredentialID != "" {
		t.Fatalf("inactive introspection disclosed: %+v %v", got, err)
	}

	for _, bad := range []struct{ path, body string }{
		{pluginsdk.AgentsResolvePath, `{"attributes":{}}`},
		{pluginsdk.AgentsResolvePath, `{"attributes":{"a":"b"},"credential_id":"Not Valid"}`},
		{pluginsdk.AgentsResolvePath, `{"attributes":{"a":"b"},"limit":101}`},
		{pluginsdk.AgentsResolvePath, `{"attributes":{"a b":"c"}}`},
		{pluginsdk.AgentsResolvePath, `{"attributes":{"a":"b"},"extra":1}`},
		{pluginsdk.GroupsResolvePath, `{"group_id":"` + staticGroup + `","selector":"a=b"}`},
		{pluginsdk.GroupsResolvePath, `{}`},
		{pluginsdk.GroupsResolvePath, `{"selector":"=="}`},
		{pluginsdk.IntrospectPath, `{"token_sha256":"ABCD"}`},
		{pluginsdk.IntrospectPath, `{"token_sha256":"` + strings.Repeat("A", 64) + `"}`},
		{pluginsdk.IntrospectPath, `{"token_sha256":"` + strings.Repeat("a", 64) + `"}{}`},
		{pluginsdk.IntrospectPath, `{"token_sha256":"` + strings.Repeat("a", 5000) + `"}`},
	} {
		if w := hostPost(registry, bad.path, bad.body); w.Code != 400 {
			t.Fatalf("%s %s: got %d", bad.path, bad.body, w.Code)
		}
	}
	if w := hostRequest(registry, "GET", pluginsdk.IntrospectPath, strings.Repeat("a", 64)); w.Code != 405 {
		t.Fatalf("introspect GET: %d", w.Code)
	}
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath+"?connected=yes", strings.Repeat("a", 64)); w.Code != 400 {
		t.Fatalf("invalid connected filter: %d", w.Code)
	}
}

func TestHostPermissionsAreSeparate(t *testing.T) {
	registry, client, session := readyHost(t, fleet.NewStore(1), pluginsdk.AgentsRead)
	registry.SetHost(nil, labelIntrospector{token: "agent-token"})
	if _, err := client.Introspect(context.Background(), session, "agent-token"); pluginsdk.StatusOf(err) != 403 {
		t.Fatalf("introspection without permission: %v", err)
	}
	if _, err := client.ResolveGroup(context.Background(), session, pluginsdk.GroupResolveRequest{GroupID: strings.Repeat("e", 32)}); pluginsdk.StatusOf(err) != 404 {
		t.Fatalf("group without control repository: %v", err)
	}
	registry, client, session = readyHost(t, fleet.NewStore(1), pluginsdk.CredentialsIntrospect)
	if _, err := client.Agents(context.Background(), session, pluginsdk.AgentQuery{}); pluginsdk.StatusOf(err) != 403 {
		t.Fatalf("directory without permission: %v", err)
	}
	if got, err := client.Introspect(context.Background(), session, "agent-token"); err != nil || got.Active {
		t.Fatalf("introspection without introspector: %+v %v", got, err)
	}
	_ = registry
}

func hostPost(r *Registry, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	info := r.entries[0].snapshot()
	req.Header.Set(pluginsdk.InstanceHeader, info.InstanceID)
	req.Header.Set(pluginsdk.EpochHeader, info.Epoch)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
