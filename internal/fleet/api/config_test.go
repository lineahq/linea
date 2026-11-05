package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// roles authenticates a bearer token as the role of the same name.
type roles struct{}

func (roles) Authenticate(_ context.Context, r *http.Request) (auth.Principal, error) {
	role := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if role != "admin" && role != "reader" {
		return auth.Principal{}, auth.ErrUnauthorized
	}
	return auth.Principal{ID: role + "-id", Role: role}, nil
}
func (roles) Active(_ context.Context, ps []auth.Principal) (map[string]bool, error) {
	out := map[string]bool{}
	for _, p := range ps {
		out[p.ID] = true
	}
	return out, nil
}

type fakeConfigs struct {
	configmgmt.Repository
	bodies  map[int][]byte
	targets []configmgmt.Target
	cleared []string
	sets    map[string]configmgmt.Components
}

func (f *fakeConfigs) ComponentSet(_ context.Context, hash string) (configmgmt.Components, error) {
	if c, ok := f.sets[hash]; ok {
		return c, nil
	}
	return nil, fleet.ErrNotFound
}

func (f *fakeConfigs) CreateConfig(_ context.Context, actor string, c configmgmt.Config) (configmgmt.Config, error) {
	c.Revision, c.CreatedBy = 1, actor
	return c, nil
}
func (f *fakeConfigs) CreateVersion(_ context.Context, actor, id string, body []byte, message string) (configmgmt.Version, error) {
	n := len(f.bodies) + 1
	f.bodies[n] = body
	return configmgmt.Version{ConfigID: id, Version: n, Size: len(body), Message: message, CreatedBy: actor}, nil
}
func (f *fakeConfigs) GetVersion(_ context.Context, id string, v int, body bool) (configmgmt.Version, error) {
	b, ok := f.bodies[v]
	if !ok {
		return configmgmt.Version{}, fleet.ErrNotFound
	}
	out := configmgmt.Version{ConfigID: id, Version: v, Size: len(b)}
	if body {
		text := string(b)
		out.Body = &text
	}
	return out, nil
}
func (f *fakeConfigs) Targets(context.Context) ([]configmgmt.Target, error) { return f.targets, nil }
func (f *fakeConfigs) ClearHold(_ context.Context, _, id string) error {
	f.cleared = append(f.cleared, id)
	return nil
}

type fakeCredentials struct {
	auth.CredentialRepository
	c auth.Credential
}

func (f *fakeCredentials) Credential(_ context.Context, id string) (auth.Credential, error) {
	if id != f.c.ID {
		return auth.Credential{}, fleet.ErrNotFound
	}
	return f.c, nil
}
func (f *fakeCredentials) SetCredentialRemoteConfig(_ context.Context, _, id string, enabled bool) (auth.Credential, error) {
	if id != f.c.ID {
		return auth.Credential{}, fleet.ErrNotFound
	}
	f.c.RemoteConfig = enabled
	return f.c, nil
}

func call(t *testing.T, h http.Handler, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, path, reader)
	r.Header.Set("Authorization", "Bearer "+role)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if method == "PATCH" || method == "DELETE" {
		r.Header.Set("If-Match", `"1"`)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Error.Code
}

func TestConfigManagementDisabledByDefault(t *testing.T) {
	s := fleet.NewStore(3)
	id := strings.Repeat("a1", 16)
	_, _ = s.Connect(id, fleet.Report{}, time.Now())
	h := NewAuthorized(s, roles{}, ServerInfo{ServerID: "test"}, nil, nil)
	for _, path := range []string{"/api/v1/configs", "/api/v1/configs/" + strings.Repeat("c1", 16) + "/versions", "/api/v1/config-assignments",
		"/api/v1/config-assignments/" + strings.Repeat("e1", 16), "/api/v1/agents/" + id + "/config"} {
		if w := call(t, h, "GET", path, "admin", nil); w.Code != 404 || errorCode(t, w) != "config_management_disabled" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := call(t, h, "GET", "/api/v1/agents/"+id, "reader", nil); w.Code != 200 {
		t.Fatalf("inventory with config management off: %d", w.Code)
	}
	var info ServerInfo
	if w := call(t, h, "GET", "/api/v1/server", "reader", nil); json.Unmarshal(w.Body.Bytes(), &info) != nil || info.ConfigManagement {
		t.Fatalf("server info = %s", w.Body)
	}
}

func TestConfigVersionsAPI(t *testing.T) {
	repo := &fakeConfigs{bodies: map[int][]byte{}}
	h := NewAuthorized(fleet.NewStore(3), roles{}, ServerInfo{ServerID: "test"}, nil, nil)
	h.SetConfigManagement(repo, []string{"receiver:otlp", "exporter:debug"})
	config := strings.Repeat("c1", 16)
	versions := "/api/v1/configs/" + config + "/versions"

	if w := call(t, h, "POST", "/api/v1/configs", "reader", map[string]string{"id": config, "name": "base"}); w.Code != 403 {
		t.Fatalf("reader create: %d", w.Code)
	}
	if w := call(t, h, "POST", "/api/v1/configs", "admin", map[string]string{"id": config, "name": "base"}); w.Code != 201 || w.Header().Get("ETag") != `"1"` {
		t.Fatalf("admin create: %d %s", w.Code, w.Body)
	}
	// Rejections name the path and reason, never the value.
	secret := "receivers:\n  otlp:\n    auth:\n      password: hunter2-literal\n"
	w := call(t, h, "POST", versions, "admin", map[string]string{"body": secret})
	if w.Code != 422 || errorCode(t, w) != "inline_secret" || strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("inline secret: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "POST", versions, "admin", map[string]string{"body": "receivers:\n  filelog: {}\n"}); w.Code != 422 || errorCode(t, w) != "invalid_config" {
		t.Fatalf("policy: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "POST", versions, "reader", map[string]string{"body": "{}"}); w.Code != 403 {
		t.Fatalf("reader version: %d", w.Code)
	}
	// Bodies above the generic 64 KiB request bound are accepted up to 128 KiB.
	large := "receivers:\n  otlp:\n    # " + strings.Repeat("x", 100*1024) + "\n"
	if w := call(t, h, "POST", versions, "admin", map[string]string{"body": large, "message": "large"}); w.Code != 201 || w.Header().Get("Location") != versions+"/1" {
		t.Fatalf("large version: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "POST", versions, "admin", map[string]string{"body": large + large}); w.Code != 422 {
		t.Fatalf("oversized version: %d", w.Code)
	}
	// Only admins read bodies.
	var v configmgmt.Version
	if w := call(t, h, "GET", versions+"/1", "reader", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Body != nil || strings.Contains(w.Body.String(), "xxxx") {
		t.Fatalf("reader version read: %d", w.Code)
	}
	if w := call(t, h, "GET", versions+"/1", "admin", nil); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Body == nil || *v.Body != large {
		t.Fatalf("admin version read: %d", w.Code)
	}
	for _, path := range []string{versions + "/0", versions + "/01", versions + "/x"} {
		if w := call(t, h, "GET", path, "admin", nil); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w := call(t, h, "GET", "/api/v1/configs/"+config+"/other", "admin", nil); w.Code != 404 {
		t.Fatalf("unknown sub-resource: %d", w.Code)
	}
}

func TestAgentConfigAPI(t *testing.T) {
	s := fleet.NewStore(3)
	id := strings.Repeat("a1", 16)
	credential := auth.Credential{ID: strings.Repeat("72", 16), Role: "agent", Labels: map[string]string{"env": "prod"}, RemoteConfig: true}
	digest := bytes.Repeat([]byte{1}, 32)
	base, _ := configmgmt.NewTarget(configmgmt.Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: strings.Repeat("c1", 16), Version: 3, Layer: 10, Selector: "service.name=edge"}, digest, "", nil)
	prod, _ := configmgmt.NewTarget(configmgmt.Assignment{ID: strings.Repeat("e2", 16), Name: "prod", ConfigID: strings.Repeat("c1", 16), Version: 4, Layer: 50, Selector: "service.name=edge",
		CredentialLabels: map[string]string{"env": "prod"}}, digest, "", nil)
	other, _ := configmgmt.NewTarget(configmgmt.Assignment{ID: strings.Repeat("e3", 16), Name: "other", ConfigID: strings.Repeat("c1", 16), Version: 1, Layer: 20, Selector: "service.name=core"}, digest, "", nil)
	repo := &fakeConfigs{targets: []configmgmt.Target{other, prod, base}}
	creds := &fakeCredentials{c: credential}
	h := NewAuthorized(s, roles{}, ServerInfo{ServerID: "test"}, nil, &auth.Service{Repository: creds})
	h.SetConfigManagement(repo, nil)

	if _, _, err := s.ConnectAgent(context.Background(), id, credential.ID, fleet.Report{Capabilities: 3, Attributes: map[string]string{"service.name": "edge"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var view agentConfigView
	w := call(t, h, "GET", "/api/v1/agents/"+id+"/config", "reader", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatalf("view: %d %s", w.Code, w.Body)
	}
	if !view.CredentialRemoteConfig || !view.AcceptsRemoteConfig || view.Desired == nil || len(view.Desired.Layers) != 2 ||
		view.Desired.Layers[0].Name != "10-base" || view.Desired.Layers[1].Name != "50-prod" || view.Reported != nil || view.InSync {
		t.Fatalf("view = %+v", view)
	}
	// The reported hash equal to the desired one, applied, is in sync.
	if _, _, err := s.ConnectAgent(context.Background(), id, credential.ID, fleet.Report{Capabilities: 3, Attributes: map[string]string{"service.name": "edge"},
		RemoteConfig: &fleet.RemoteConfig{Hash: view.Desired.Hash, Status: "applied"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := call(t, h, "GET", "/api/v1/agents/"+id+"/config", "reader", nil); json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.InSync {
		t.Fatalf("in sync view = %s", w.Body)
	}
	// Without the label the agent loses the prod layer; with no match at
	// all a formerly managed agent is released with the unmanaged entry.
	creds.c.Labels = nil
	if w := call(t, h, "GET", "/api/v1/agents/"+id+"/config", "reader", nil); json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view.Desired.Layers) != 1 || view.InSync {
		t.Fatalf("unlabelled view = %s", w.Body)
	}
	// A build that lacks a type of the desired layers is reported, with
	// types only.
	repo.bodies = map[int][]byte{3: []byte("receivers: {otlp: {}}\nexporters: {kafka: {}}\n")}
	repo.sets = map[string]configmgmt.Components{"abcd": {"receivers": {"otlp"}, "exporters": {"debug"}}}
	if _, _, err := s.ConnectAgent(context.Background(), id, credential.ID, fleet.Report{Capabilities: 3, Attributes: map[string]string{"service.name": "edge"}, AvailableComponents: "abcd"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := call(t, h, "GET", "/api/v1/agents/"+id+"/config", "reader", nil); json.Unmarshal(w.Body.Bytes(), &view) != nil || len(view.MissingComponents) != 1 ||
		view.MissingComponents[0] != "exporter:kafka" || strings.Contains(w.Body.String(), "receivers") {
		t.Fatalf("incompatible view = %s", w.Body)
	}
	repo.targets = []configmgmt.Target{other}
	if w := call(t, h, "GET", "/api/v1/agents/"+id+"/config", "reader", nil); json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.Desired.Unmanaged || view.Desired.Layers[0].Name != configmgmt.UnmanagedEntry {
		t.Fatalf("released view = %s", w.Body)
	}

	if w := call(t, h, "POST", "/api/v1/agents/"+id+"/config:retry", "reader", nil); w.Code != 403 {
		t.Fatalf("reader retry: %d", w.Code)
	}
	if w := call(t, h, "POST", "/api/v1/agents/"+id+"/config:retry", "admin", nil); w.Code != 204 || len(repo.cleared) != 1 || repo.cleared[0] != id {
		t.Fatalf("retry: %d %v", w.Code, repo.cleared)
	}
	if w := call(t, h, "GET", "/api/v1/agents/bad/config", "admin", nil); w.Code != 400 {
		t.Fatalf("bad agent ID: %d", w.Code)
	}

	// The credential opt-in is the only mutable credential field.
	if w := call(t, h, "PATCH", "/api/v1/credentials/"+credential.ID, "admin", map[string]bool{"remote_config": false}); w.Code != 200 || creds.c.RemoteConfig {
		t.Fatalf("patch credential: %d %s", w.Code, w.Body)
	}
	if w := call(t, h, "PATCH", "/api/v1/credentials/"+credential.ID, "admin", map[string]string{"name": "x"}); w.Code != 400 {
		t.Fatalf("patch other field: %d", w.Code)
	}
	if w := call(t, h, "PATCH", "/api/v1/credentials/"+credential.ID, "reader", map[string]bool{"remote_config": true}); w.Code != 403 {
		t.Fatalf("reader patch: %d", w.Code)
	}
}
