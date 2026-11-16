package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

// Only a credential granted access to the plugin reaches its API, whatever
// its role, and is forwarded with the granted level. Status needs a grant too.
func TestPluginAPICallerAccess(t *testing.T) {
	config := fixtureConfig("")
	config.APIVersion = "1"
	var mu sync.Mutex
	var levels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == pluginsdk.ManifestPath {
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32), APIVersion: "1"})
			return
		}
		mu.Lock()
		levels = append(levels, req.Header.Get(pluginsdk.CallerAccessHeader))
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	config.Endpoint = server.URL
	r := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer r.Close()
	r.probe(r.entries[0])
	grant := func(level string) map[string]string { return map[string]string{config.ID: level} }
	for _, tc := range []struct {
		role   string
		access map[string]string
		method string
		want   int
	}{
		{"reader", grant("read"), "GET", 200}, {"reader", grant("write"), "POST", 200}, {"operator", grant("admin"), "DELETE", 200},
		{"admin", nil, "GET", 403}, {"operator", nil, "POST", 403}, {"admin", map[string]string{"other": "admin"}, "GET", 403},
		{"reader", grant("owner"), "GET", 403}, {"reader", grant(""), "GET", 403},
	} {
		mu.Lock()
		levels = nil
		mu.Unlock()
		req := httptest.NewRequest(tc.method, "http://fleet/api/v1/plugins/"+config.ID+"/api/v1/custom", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		r.ServeAPI(w, req, config.ID, "v1/custom", auth.Principal{ID: "caller", Role: tc.role, PluginAccess: tc.access})
		mu.Lock()
		forwarded := append([]string(nil), levels...)
		mu.Unlock()
		if w.Code != tc.want || (tc.want == 200) != (len(forwarded) == 1 && forwarded[0] == tc.access[config.ID]) || (tc.want != 200 && len(forwarded) != 0) {
			t.Fatalf("%s %v %s: status %d, forwarded levels %v", tc.role, tc.access, tc.method, w.Code, forwarded)
		}
	}
	for _, level := range []string{"read", "write", "admin"} {
		if _, err := r.Status(context.Background(), config.ID, auth.Principal{ID: "caller", Role: "reader", PluginAccess: grant(level)}); errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("%s was refused plugin status", level)
		}
	}
	if _, err := r.Status(context.Background(), config.ID, auth.Principal{ID: "caller", Role: "admin"}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("an admin without a grant read plugin status")
	}
}

// Plugin lifecycle stays with the admin.
func TestPluginLifecycleIsAdminOnly(t *testing.T) {
	r := New(context.Background(), nil, fleet.NewStore(1))
	defer r.Close()
	for _, role := range []string{"reader", "operator", "agent", ""} {
		if _, err := r.Lifecycle(context.Background(), "example", "disable", auth.Principal{ID: "caller", Role: role}, 0); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("%q: %v", role, err)
		}
	}
	if _, err := r.Lifecycle(context.Background(), "example", "disable", auth.Principal{ID: "caller", Role: "admin"}, 0); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatalf("admin: %v", err)
	}
}
