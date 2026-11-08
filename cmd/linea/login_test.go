package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLoginSaysWhenNoSignInIsNeeded: a server without CLI sign-in that
// already signs requests in, as behind the Compose stack's proxy, needs no
// login; one that refuses them needs an API token.
func TestLoginSaysWhenNoSignInIsNeeded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session bool
		code    int
		want    string
	}{
		{name: "signed in by a proxy", session: true, code: exitOK, want: "no sign-in needed: "},
		{name: "needs a token", code: exitError, want: "has no CLI sign-in; ask for an API token and set LINEA_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("APPDATA", t.TempDir())
			t.Setenv(serverTokenEnv, "")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/v1/session" && tc.session:
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"principal":{"id":"user:local","kind":"user","name":"local"},"permissions":[]}`))
				case r.URL.Path == "/api/v1/session":
					http.Error(w, `{"error":{"code":"unauthenticated","message":"sign in"}}`, http.StatusUnauthorized)
				default:
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, _ = w.Write([]byte("<!doctype html>"))
				}
			}))
			t.Cleanup(srv.Close)
			code, out, stderr := runCLI(t, "login", "--server", srv.URL)
			if code != tc.code || !strings.Contains(out+stderr, tc.want) {
				t.Fatalf("login: exit %d, %s%s", code, out, stderr)
			}
			if tc.session && !strings.Contains(out, "as local (user:local)") {
				t.Fatalf("login names no principal: %s", out)
			}
		})
	}
}
