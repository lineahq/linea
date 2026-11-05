package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// oidcTeam writes a server config signing people in with the provider only.
func oidcTeam(t *testing.T, issuer string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client-secret"), []byte("web-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTeamSecrets(t, dir)
	text := strings.Replace(teamYAML(), `  trusted_headers:
    proxies: [127.0.0.1]
    issuer: proxy
    user: X-Forwarded-User
    groups: X-Forwarded-Groups
`, `  oidc:
    issuer: `+issuer+`
    client_id: web
    client_secret_file: client-secret
    redirect_url: http://127.0.0.1/auth/callback
    cli_client_id: cli
    session_ttl: 1h
`, 1)
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTeamServerSignsInWithOIDC: a browser signs in with the code flow and
// changes things only with the session's CSRF token; MCP accepts the
// provider's JWTs; the CLI signs in with the device flow, renews an expired
// sign-in with its refresh token and forgets it on logout.
func TestTeamServerSignsInWithOIDC(t *testing.T) {
	idp := oidctest.New(t)
	server := startTeam(t, oidcTeam(t, idp.Issuer))
	send := teamRequests(t, server)

	login := send(http.MethodGet, "/auth/login?return_to=/runs", nil, nil)
	login.Body.Close()
	if login.StatusCode != http.StatusFound || login.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(login.Header.Get("Location"), idp.Issuer+"/auth?") {
		t.Fatalf("login = %d %s", login.StatusCode, login.Header.Get("Location"))
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	atIdP, err := noFollow.Get(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	atIdP.Body.Close()
	back, _ := url.Parse(atIdP.Header.Get("Location"))
	cb := send(http.MethodGet, "/auth/callback?"+back.RawQuery, nil, nil, login.Cookies()...)
	cb.Body.Close()
	var session *http.Cookie
	for _, c := range cb.Cookies() {
		if c.Name == "linea-session" {
			session = c
		}
	}
	if cb.StatusCode != http.StatusSeeOther || cb.Header.Get("Location") != "/runs" || session == nil || session.Secure || !session.HttpOnly {
		t.Fatalf("callback = %d %s, %+v", cb.StatusCode, cb.Header.Get("Location"), session)
	}
	resp := send(http.MethodGet, "/api/v1/session", nil, nil, session)
	var who api.Session
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil || resp.StatusCode != http.StatusOK || who.Principal.Name != "Alice" ||
		who.Principal.ID != platform.UserID(idp.Issuer, "alice") || who.CSRFToken == "" {
		t.Fatalf("session = %d %+v, %v", resp.StatusCode, who, err)
	}
	resp.Body.Close()
	tokenReq := api.TokenRequest{Label: "laptop"}
	if resp := send(http.MethodPost, "/api/v1/tokens", tokenReq, nil, session); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST without the CSRF token = %d", resp.StatusCode)
	}
	if resp := send(http.MethodPost, "/api/v1/tokens", tokenReq, map[string]string{platform.CSRFHeader: who.CSRFToken}, session); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST with the CSRF token = %d", resp.StatusCode)
	}

	ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}
	mcpHeader := func(token string) map[string]string {
		return map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + token}
	}
	if resp := send(http.MethodPost, "/mcp", ping, mcpHeader(idp.Token(t, "cli", oidctest.User{Subject: "alice", Groups: []string{"platform"}}, time.Hour, nil))); resp.StatusCode != http.StatusOK {
		t.Fatalf("MCP with the provider's token = %d", resp.StatusCode)
	}
	if resp := send(http.MethodPost, "/mcp", ping, mcpHeader(idp.Token(t, "another-app", oidctest.User{Subject: "alice"}, time.Hour, nil))); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP with another audience's token = %d", resp.StatusCode)
	}

	config := t.TempDir()
	for _, env := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		t.Setenv(env, config)
	}
	t.Setenv(serverTokenEnv, "")
	loginWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { loginWait = nil })
	code, out, stderr := runCLI(t, "login", "--server", server)
	if code != exitOK || !strings.Contains(stderr, "To sign in, open "+idp.Issuer+"/device") || !strings.Contains(out, "as Alice ("+who.Principal.ID+")") {
		t.Fatalf("login: exit %d, %s%s", code, out, stderr)
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitOK {
		t.Fatalf("runs after login: exit %d, %s", code, stderr)
	}
	path, err := client.CredentialsPath()
	if err != nil || !strings.HasPrefix(path, config) {
		t.Fatalf("credentials path %s, %v", path, err)
	}
	cred, err := client.LoadCredential(path, server)
	if err != nil || cred == nil {
		t.Fatalf("stored = %+v, %v", cred, err)
	}
	used := cred.RefreshToken
	cred.Expiry = time.Now().Add(-time.Minute)
	if err := client.SaveCredential(path, server, cred); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitOK {
		t.Fatalf("runs with an expired sign-in: exit %d, %s", code, stderr)
	}
	if renewed, _ := client.LoadCredential(path, server); renewed == nil || renewed.RefreshToken == used || time.Until(renewed.Expiry) < 30*time.Minute {
		t.Fatalf("renewed = %+v", renewed)
	}
	if code, out, _ := runCLI(t, "logout", "--server", server); code != exitOK || !strings.Contains(out, "signed out") {
		t.Fatalf("logout: exit %d, %s", code, out)
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitError || !strings.Contains(stderr, "sign in or present an API token") {
		t.Fatalf("runs after logout: exit %d, %s", code, stderr)
	}
	if code, out, _ := runCLI(t, "logout", "--server", server); code != exitOK || !strings.Contains(out, "not signed in") {
		t.Fatalf("second logout: exit %d, %s", code, out)
	}

	unreachable := oidcTeam(t, "http://127.0.0.1:1")
	if code, _, stderr := runServer(t, "--config", unreachable, "--addr", "127.0.0.1:0"); code != exitError || !strings.Contains(stderr, "OIDC provider http://127.0.0.1:1") {
		t.Fatalf("serve with an unreachable provider: exit %d, %s", code, stderr)
	}
}

// TestServerCommandsTrustTheSignInCA: the CA kept by login --ca verifies
// the team server for later --server commands, also when
// $LINEA_TOKEN supplies the token.
func TestServerCommandsTrustTheSignInCA(t *testing.T) {
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"runs":[]}`))
	}))
	t.Cleanup(srv.Close)
	config := t.TempDir()
	for _, env := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		t.Setenv(env, config)
	}
	t.Setenv(serverTokenEnv, "api-token")
	if code, _, stderr := runCLI(t, "runs", "--server", srv.URL); code != exitError || !strings.Contains(stderr, "certificate") {
		t.Fatalf("without a saved CA: exit %d, %s", code, stderr)
	}
	path, err := client.CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := client.SaveCredential(path, srv.URL, &client.Credential{IDToken: "a.b.c", Expiry: time.Now().Add(-time.Hour), CA: string(ca)}); err != nil {
		t.Fatal(err)
	}
	// The API token wins, so the expired sign-in is not renewed.
	if code, _, stderr := runCLI(t, "runs", "--server", srv.URL); code != exitOK || auth != "Bearer api-token" {
		t.Fatalf("with the saved CA: exit %d, %s (Authorization %q)", code, stderr, auth)
	}
	if code, _, stderr := runCLI(t, "login", "--server", srv.URL, "--ca", filepath.Join(config, "absent.pem")); code != exitError || !strings.Contains(stderr, "--ca") {
		t.Fatalf("login with a missing CA file: exit %d, %s", code, stderr)
	}
}

// TestRoleBindingWritesNeedTheCSRFToken: on a team server with OIDC, a web
// session's grant or removal of a role binding without the session's CSRF
// token is refused before it is authorized, and works with it.
func TestRoleBindingWritesNeedTheCSRFToken(t *testing.T) {
	idp := oidctest.New(t)
	path := oidcTeam(t, idp.Issuer)
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	admin := strings.Replace(string(text), "  - group: platform\n    role: viewer\n", "  - group: platform\n    role: admin\n", 1)
	if admin == string(text) {
		t.Fatal("the server config binds no platform viewer to promote")
	}
	if err := os.WriteFile(path, []byte(admin), 0o600); err != nil {
		t.Fatal(err)
	}
	send := teamRequests(t, startTeam(t, path))

	login := send(http.MethodGet, "/auth/login?return_to=/access", nil, nil)
	login.Body.Close()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	atIdP, err := noFollow.Get(login.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	atIdP.Body.Close()
	back, _ := url.Parse(atIdP.Header.Get("Location"))
	cb := send(http.MethodGet, "/auth/callback?"+back.RawQuery, nil, nil, login.Cookies()...)
	cb.Body.Close()
	var session *http.Cookie
	for _, c := range cb.Cookies() {
		if c.Name == "linea-session" {
			session = c
		}
	}
	if session == nil {
		t.Fatalf("callback = %d without a session", cb.StatusCode)
	}
	resp := send(http.MethodGet, "/api/v1/session", nil, nil, session)
	var who api.Session
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil || who.CSRFToken == "" {
		t.Fatalf("session = %+v, %v", who, err)
	}
	resp.Body.Close()
	csrf := map[string]string{platform.CSRFHeader: who.CSRFToken}
	wrong := map[string]string{platform.CSRFHeader: strings.Repeat("0", len(who.CSRFToken))}
	grant := api.RoleBindingRequest{Group: "sre", Role: "admin"}
	expect := func(name string, resp *http.Response, want int) *http.Response {
		t.Helper()
		if resp.StatusCode != want {
			t.Fatalf("%s = %d, want %d", name, resp.StatusCode, want)
		}
		return resp
	}

	expect("listing with the session", send(http.MethodGet, "/api/v1/role-bindings", nil, nil, session), http.StatusOK).Body.Close()
	expect("granting without the CSRF token", send(http.MethodPost, "/api/v1/role-bindings", grant, nil, session), http.StatusUnauthorized).Body.Close()
	expect("granting with a wrong CSRF token", send(http.MethodPost, "/api/v1/role-bindings", grant, wrong, session), http.StatusUnauthorized).Body.Close()
	expect("granting with the token but no session", send(http.MethodPost, "/api/v1/role-bindings", grant, csrf), http.StatusUnauthorized).Body.Close()
	added := expect("granting with the CSRF token", send(http.MethodPost, "/api/v1/role-bindings", grant, csrf, session), http.StatusCreated)
	var list api.RoleBindings
	if err := json.NewDecoder(added.Body).Decode(&list); err != nil || list.Revision != 1 {
		t.Fatalf("granted = %+v, %v", list, err)
	}
	added.Body.Close()
	id := list.Bindings[len(list.Bindings)-1].ID
	remove := "/api/v1/role-bindings/" + id + "?revision=1"
	expect("removing without the CSRF token", send(http.MethodDelete, remove, nil, nil, session), http.StatusUnauthorized).Body.Close()
	expect("removing with the CSRF token", send(http.MethodDelete, remove, nil, csrf, session), http.StatusOK).Body.Close()
}
