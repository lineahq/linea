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

// Reader, operator and admin callers reach a plugin's API and are forwarded
// with their own role; other roles are refused before any upstream call.
func TestPluginAPICallerRoles(t *testing.T) {
	config := fixtureConfig("")
	config.APIVersion = "1"
	var mu sync.Mutex
	var roles []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == pluginsdk.ManifestPath {
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32), APIVersion: "1"})
			return
		}
		mu.Lock()
		roles = append(roles, req.Header.Get(pluginsdk.CallerRoleHeader))
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	config.Endpoint = server.URL
	r := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer r.Close()
	r.probe(r.entries[0])
	for _, tc := range []struct {
		role   string
		method string
		want   int
	}{
		{"reader", "GET", 200}, {"operator", "GET", 200}, {"admin", "GET", 200},
		{"operator", "POST", 200}, {"operator", "DELETE", 200},
		{"agent", "GET", 403}, {"", "GET", 403}, {"owner", "POST", 403},
	} {
		mu.Lock()
		roles = nil
		mu.Unlock()
		req := httptest.NewRequest(tc.method, "http://fleet/api/v1/plugins/"+config.ID+"/api/v1/custom", strings.NewReader("{}"))
		w := httptest.NewRecorder()
		r.ServeAPI(w, req, config.ID, "v1/custom", auth.Principal{ID: "caller", Role: tc.role})
		mu.Lock()
		forwarded := append([]string(nil), roles...)
		mu.Unlock()
		if w.Code != tc.want || (tc.want == 200) != (len(forwarded) == 1 && forwarded[0] == tc.role) || (tc.want != 200 && len(forwarded) != 0) {
			t.Fatalf("%s %s: status %d, forwarded roles %v", tc.role, tc.method, w.Code, forwarded)
		}
	}
	for _, role := range []string{"reader", "operator", "admin"} {
		if _, err := r.Status(context.Background(), config.ID, auth.Principal{ID: "caller", Role: role}); errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("%s was refused plugin status", role)
		}
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
