package api

import (
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestSessionNamesTheSignedInPrincipal: GET /api/v1/session answers who the
// credential signs in, the local owner in local mode, and needs a
// credential on a team server; without a web session it has no CSRF token.
func TestSessionNamesTheSignedInPrincipal(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Access: bindPlatform(t, "viewer")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, team := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}})
	resp := identityRequest(t, http.MethodGet, team.URL+"/api/v1/session", nil, map[string]string{"X-Test-User": "alice", "X-Test-Group": "platform"})
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	s := decodeBody[Session](t, resp, http.StatusOK)
	if s.Principal.ID != "user:alice" || s.Principal.Kind != "user" || s.Principal.Name != "alice" ||
		len(s.Principal.Groups) != 1 || s.Principal.Groups[0] != "platform" || s.CSRFToken != "" {
		t.Fatalf("session = %+v", s)
	}
	if got := strings.Join(s.Permissions, " "); got != "run.read artifact.read capture.read fleet.read runtime.read token.own" {
		t.Errorf("viewer permissions = %s", got)
	}
	decodeBody[Error](t, identityRequest(t, http.MethodGet, team.URL+"/api/v1/session", nil, nil), http.StatusUnauthorized)

	outsider := identityRequest(t, http.MethodGet, team.URL+"/api/v1/session", nil, map[string]string{"X-Test-User": "mallory", "X-Test-Group": "sre"})
	if s := decodeBody[Session](t, outsider, http.StatusOK); s.Permissions == nil || len(s.Permissions) != 0 {
		t.Errorf("unbound permissions = %#v", s.Permissions)
	}

	alone, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(alone.Close)
	_, owner := newServerOptions(t, Options{Platform: alone})
	if s := decodeBody[Session](t, identityRequest(t, http.MethodGet, owner.URL+"/api/v1/session", nil, nil), http.StatusOK); !slices.Contains(s.Permissions, "token.manage") || slices.Contains(s.Permissions, "admin") {
		t.Errorf("local owner permissions = %v", s.Permissions)
	}
	_, local := newServerOptions(t, Options{Platform: service})
	if s := decodeBody[Session](t, identityRequest(t, http.MethodGet, local.URL+"/api/v1/session", nil, nil), http.StatusOK); s.Principal.ID != platform.LocalOwner || s.Principal.Kind != string(platform.PrincipalLocal) {
		t.Fatalf("local session = %+v", s)
	}
}
