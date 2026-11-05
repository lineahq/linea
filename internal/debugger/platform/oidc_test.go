package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func newOIDC(t *testing.T, opts OIDCOptions) (*OIDC, *oidctest.Provider, *Service) {
	t.Helper()
	idp := oidctest.New(t)
	s := openPlatform(t, memory.NewData(), Options{})
	opts.Issuer, opts.ClientID, opts.ClientSecret, opts.RedirectURL = idp.Issuer, "web", "web-secret", "https://debugger.example.com/auth/callback"
	o, err := s.OIDC(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return o, idp, s
}

// serve sends a request to the sign-in handler with the cookies.
func serveAuth(o *OIDC, method, target string, cookies []*http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	o.Handler().ServeHTTP(w, r)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// signIn runs the browser's side of the code flow and returns the callback's
// answer.
func signIn(t *testing.T, o *OIDC, returnTo string) *httptest.ResponseRecorder {
	t.Helper()
	return signInAcross(t, o, o, returnTo)
}

// signInAcross starts the sign-in on one instance and completes it on another.
func signInAcross(t *testing.T, start, finish *OIDC, returnTo string) *httptest.ResponseRecorder {
	t.Helper()
	login := serveAuth(start, http.MethodGet, "/auth/login?return_to="+url.QueryEscape(returnTo), nil, nil)
	state := cookieNamed(login, loginCookie)
	if login.Code != http.StatusFound || state == nil || !state.HttpOnly || state.Path != "/auth/" {
		t.Fatalf("login = %d, %+v", login.Code, state)
	}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Path != "/auth/callback" {
		t.Fatalf("provider redirect = %s, %v", resp.Header.Get("Location"), err)
	}
	return serveAuth(finish, http.MethodGet, "/auth/callback?"+back.RawQuery, []*http.Cookie{state}, nil)
}

// TestOIDCSignsPeopleInWithServerSideSessions: the code flow with PKCE ends
// in a session whose cookie is HttpOnly, SameSite=Lax and __Host- bound; the
// workspace keeps only its hash; unsafe requests need the CSRF token;
// signing out ends the session at once.
func TestOIDCSignsPeopleInWithServerSideSessions(t *testing.T) {
	o, idp, s := newOIDC(t, OIDCOptions{})
	cb := signIn(t, o, "/runs?x=1")
	session := cookieNamed(cb, sessionCookie)
	if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/runs?x=1" || session == nil || !session.HttpOnly || !session.Secure ||
		session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.MaxAge != int(DefaultSessionTTL.Seconds()) {
		t.Fatalf("callback = %d %s, %+v", cb.Code, cb.Header().Get("Location"), session)
	}
	if _, err := s.repository.SessionByHash(context.Background(), tokenHash(session.Value)); err != nil {
		t.Fatalf("the workspace does not keep the session by its hash: %v", err)
	}
	if _, err := s.repository.SessionByHash(context.Background(), session.Value); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("the workspace finds a session by its secret: %v", err)
	}

	request := func(method string, header map[string]string) *http.Request {
		r := httptest.NewRequest(method, "/api/v1/runs", nil)
		r.AddCookie(session)
		for k, v := range header {
			r.Header.Set(k, v)
		}
		return r
	}
	p, err := o.Authenticate(request(http.MethodGet, nil))
	if err != nil || p.ID != UserID(idp.Issuer, "alice") || p.Kind != PrincipalUser || p.Name != "Alice" || len(p.Groups) != 1 || p.Groups[0] != "platform" {
		t.Fatalf("principal = %+v, %v", p, err)
	}
	csrf := CSRFToken(request(http.MethodGet, nil))
	if len(csrf) != 64 || csrf == session.Value {
		t.Fatalf("csrf = %q", csrf)
	}
	if _, err := o.Authenticate(request(http.MethodPost, nil)); !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNoCredential) || !strings.Contains(err.Error(), CSRFHeader) {
		t.Fatalf("POST without CSRF = %v", err)
	}
	if _, err := o.Authenticate(request(http.MethodDelete, map[string]string{CSRFHeader: strings.Repeat("0", 64)})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("DELETE with a wrong CSRF token = %v", err)
	}
	if _, err := o.Authenticate(request(http.MethodPost, map[string]string{CSRFHeader: csrf})); err != nil {
		t.Fatalf("POST with CSRF = %v", err)
	}

	if w := serveAuth(o, http.MethodPost, "/auth/logout", []*http.Cookie{session}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d", w.Code)
	}
	out := serveAuth(o, http.MethodPost, "/auth/logout", []*http.Cookie{session}, map[string]string{CSRFHeader: csrf})
	if cleared := cookieNamed(out, sessionCookie); out.Code != http.StatusNoContent || cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout = %d, %+v", out.Code, cleared)
	}
	if _, err := o.Authenticate(request(http.MethodGet, nil)); !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNoCredential) || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("after sign-out = %v", err)
	}
	if _, err := o.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil)); err != ErrNoCredential {
		t.Fatalf("no cookie = %v", err)
	}
}

// TestOIDCRefusesForgedSignIns: a callback the browser did not start, a
// replayed one and an off-site return address are refused or neutralized.
func TestOIDCRefusesForgedSignIns(t *testing.T) {
	o, _, _ := newOIDC(t, OIDCOptions{InsecureCookie: true})
	cb := signIn(t, o, "//evil.example.com/")
	if s := cookieNamed(cb, insecureSessionCookie); cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/" || s == nil || s.Secure {
		t.Fatalf("off-site return = %d %s, %+v", cb.Code, cb.Header().Get("Location"), s)
	}
	login := serveAuth(o, http.MethodGet, "/auth/login", nil, nil)
	state := cookieNamed(login, loginCookie)
	q := url.Values{"state": {state.Value}, "code": {"stolen"}}.Encode()
	if w := serveAuth(o, http.MethodGet, "/auth/callback?"+q, nil, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("callback without the login cookie = %d", w.Code)
	}
	other := &http.Cookie{Name: loginCookie, Value: "other"}
	if w := serveAuth(o, http.MethodGet, "/auth/callback?"+q, []*http.Cookie{other}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("callback with another login's cookie = %d", w.Code)
	}
	if w := serveAuth(o, http.MethodGet, "/auth/callback?"+q, []*http.Cookie{state}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("callback with a code the provider never issued = %d", w.Code)
	}
	if w := serveAuth(o, http.MethodGet, "/auth/callback?"+q, []*http.Cookie{state}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback = %d", w.Code)
	}
	if w := serveAuth(o, http.MethodGet, "/auth/callback?error=access_denied", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("provider error = %d", w.Code)
	}
}

// TestOIDCAcceptsTheProvidersBearerTokens: a JWT from the provider for an
// accepted audience signs its user in; other audiences, issuers, expired
// tokens and oversized claims are refused, and API tokens are left to
// TokenAuthenticator.
func TestOIDCAcceptsTheProvidersBearerTokens(t *testing.T) {
	o, idp, _ := newOIDC(t, OIDCOptions{CLIClientID: "cli", GroupsClaim: "roles"})
	bearer := func(token string) (Principal, error) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return o.Authenticate(r)
	}
	bob := oidctest.User{Subject: "bob"}
	p, err := bearer(idp.Token(t, "cli", bob, time.Hour, map[string]any{"preferred_username": "bob@example.com", "roles": "sre, platform,sre"}))
	if err != nil || p.ID != UserID(idp.Issuer, "bob") || p.Name != "bob@example.com" || strings.Join(p.Groups, "|") != "sre|platform" {
		t.Fatalf("bearer principal = %+v, %v", p, err)
	}
	if p, err := bearer(idp.Token(t, "web", bob, time.Hour, map[string]any{"roles": []any{"a", 1, "b"}})); err != nil || p.Name != "bob" || strings.Join(p.Groups, "|") != "a|b" {
		t.Fatalf("web audience = %+v, %v", p, err)
	}
	other := oidctest.New(t)
	for name, token := range map[string]string{
		"audience": idp.Token(t, "someone-else", bob, time.Hour, nil),
		"expired":  idp.Token(t, "cli", bob, -time.Minute, nil),
		"issuer":   other.Token(t, "cli", bob, time.Hour, nil),
		"garbage":  "not.a.jwt",
		"claims":   idp.Token(t, "cli", oidctest.User{Subject: "bob", Name: strings.Repeat("n", maxPrincipalName+1)}, time.Hour, nil),
	} {
		if _, err := bearer(token); !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNoCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := bearer(TokenPrefix + "x"); err != ErrNoCredential {
		t.Fatalf("API token = %v", err)
	}
}

// TestOIDCComposition: OIDC needs a workspace and bounded sessions, and
// tells the CLI how to sign in only when a CLI client is configured.
func TestOIDCComposition(t *testing.T) {
	idp := oidctest.New(t)
	opts := OIDCOptions{Issuer: idp.Issuer, ClientID: "web", RedirectURL: "https://d/auth/callback"}
	if _, err := New(Options{}).OIDC(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("no workspace = %v", err)
	}
	s := openPlatform(t, memory.NewData(), Options{})
	long := opts
	long.SessionTTL = MaxSessionTTL + time.Hour
	if _, err := s.OIDC(context.Background(), long); err == nil {
		t.Fatal("a session longer than the maximum was accepted")
	}
	wrong := opts
	wrong.Issuer = idp.Issuer + "/other"
	if _, err := s.OIDC(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "OIDC provider") {
		t.Fatalf("unknown issuer = %v", err)
	}
	o, err := s.OIDC(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if w := serveAuth(o, http.MethodGet, "/auth/cli", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("no CLI client = %d", w.Code)
	}
	opts.CLIClientID = "cli"
	o, err = s.OIDC(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	w := serveAuth(o, http.MethodGet, "/auth/cli", nil, nil)
	var cli CLISignIn
	if err := json.Unmarshal(w.Body.Bytes(), &cli); err != nil || cli.Issuer != idp.Issuer || cli.ClientID != "cli" || strings.Join(cli.Scopes, " ") != "openid profile email offline_access" {
		t.Fatalf("cli = %s, %v", w.Body, err)
	}
}
