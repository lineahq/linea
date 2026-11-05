package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/plugins"
	"github.com/lineahq/linea/pluginsdk"
)

func TestPublicPluginAPIAuthenticatesAndContainsUpstream(t *testing.T) {
	hostToken, serviceToken, apiToken := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("m", 32)
	seen := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pluginsdk.ManifestPath {
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: "example", Version: "0.1.0", ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32)})
			return
		}
		seen <- r.Header.Clone()
		w.Header().Set("Set-Cookie", "upstream=must-not-forward")
		_, _ = w.Write([]byte(`{"bounded":true}`))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "host"), []byte(hostToken), 0600)
	_ = os.WriteFile(filepath.Join(dir, "service"), []byte(serviceToken), 0600)
	body, _ := json.Marshal(map[string]any{"plugins": []plugins.Config{{ID: "example", Version: "0.1.0", Endpoint: upstream.URL, Enabled: true, Permissions: []string{pluginsdk.AgentsRead}, HostTokenFile: "host", ServiceTokenFile: "service"}}})
	path := filepath.Join(dir, "registrations.json")
	_ = os.WriteFile(path, body, 0600)
	configs, err := plugins.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store := fleet.NewStore(1)
	registry := plugins.New(context.Background(), configs, store)
	defer registry.Close()
	registry.Start()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for registry.List()[0].State != "ready" {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("plugin did not become ready")
		}
	}
	bearer, _ := auth.New(apiToken)
	h := New(store, bearer, ServerInfo{ServerID: "test"})
	h.SetPlugins(registry)
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/api/v1/plugins", "", 401},
		{"/api/v1/plugins", hostToken, 401},
		{"/api/v1/plugins/example/status", serviceToken, 401},
		{"/api/v1/plugins", apiToken, 200},
		{"/api/v1/plugins/example", apiToken, 200},
		{"/api/v1/plugins/missing/status", apiToken, 404},
		{"/api/v1/plugins/example/status?callback=foreign", apiToken, 400},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: got %d want %d", tc.path, w.Code, tc.want)
		}
	}
	malformed := httptest.NewRequest("GET", "/api/v1/plugins", nil)
	malformed.Header.Set("Authorization", "Bearer "+apiToken)
	malformed.Header.Set(pluginsdk.ActingNameHeader, "no principal")
	refused := httptest.NewRecorder()
	h.ServeHTTP(refused, malformed)
	if refused.Code != 400 || !strings.Contains(refused.Body.String(), "invalid_acting_for") {
		t.Fatalf("malformed acting-for = %d %s", refused.Code, refused.Body)
	}
	r := httptest.NewRequest("GET", "/api/v1/plugins/example/status", nil)
	r.Header.Set("Authorization", "Bearer "+apiToken)
	r.Header.Set(pluginsdk.CallerRoleHeader, "admin")
	r.Header.Set(pluginsdk.CallerIDHeader, "forged-id")
	r.Header.Set("Cookie", "private-client-cookie")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	headers := <-seen
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) || w.Header().Get("Set-Cookie") != "" || headers.Get("Cookie") != "" || headers.Get("Authorization") != "Bearer "+serviceToken || headers.Get(pluginsdk.CallerRoleHeader) != "reader" || headers.Get(pluginsdk.CallerIDHeader) != "memory-reader" {
		t.Fatal("unsafe public plugin boundary")
	}
	for _, path := range []string{"/api/v1/plugins/example/lifecycle"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set(pluginsdk.CallerRoleHeader, "admin")
		result := httptest.NewRecorder()
		h.ServeHTTP(result, req)
		if result.Code != 403 {
			t.Fatal("reader or forged role reached plugin mutation")
		}
	}
	for _, path := range []string{"/api/v1/plugins/example/probe", "/api/v1/plugins/example/operations"} {
		removed := httptest.NewRequest("GET", path, nil)
		removed.Header.Set("Authorization", "Bearer "+apiToken)
		result := httptest.NewRecorder()
		h.ServeHTTP(result, removed)
		if result.Code != 400 {
			t.Fatal("removed host v2 messaging route was still served")
		}
	}
	upstream.Close()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), upstream.URL) || strings.Contains(w.Body.String(), serviceToken) {
		t.Fatal("upstream failure was not sanitized")
	}
}
