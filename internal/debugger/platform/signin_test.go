package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const testAdminPassword = "correct horse battery staple"

// newAdminSignIn opens a team platform with the built-in admin account on
// data and composes its sign-in, with OIDC when idp is not nil.
func newAdminSignIn(t *testing.T, data *memory.Data, idp *oidctest.Provider) (*SignIn, *Service) {
	t.Helper()
	s := openPlatform(t, data, Options{Shared: true, Access: mustPolicy(t), AdminPassword: testAdminPassword})
	var o *OIDC
	if idp != nil {
		var err error
		if o, err = s.OIDC(context.Background(), OIDCOptions{Issuer: idp.Issuer, ClientID: "web", ClientSecret: "web-secret",
			RedirectURL: "https://debugger.example.com/auth/callback", CLIClientID: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	si, err := s.SignIn(SignInOptions{OIDC: o})
	if err != nil {
		t.Fatal(err)
	}
	return si, s
}

// postPassword sends POST /auth/password from client with body.
func postPassword(si *SignIn, client, body string, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://debugger.example.com/auth/password", strings.NewReader(body))
	r.RemoteAddr = client + ":40000"
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	si.Handler().ServeHTTP(w, r)
	return w
}

func passwordBody(user, password, client string) string {
	b, _ := json.Marshal(PasswordSignIn{User: user, Password: password, Client: client})
	return string(b)
}

// TestAdminPasswordSignsInAsAdminEverywhere: the right password starts a
// web session for the admin user, who holds admin on scope * without a
// configured binding; the session needs the CSRF token like any other.
func TestAdminPasswordSignsInAsAdminEverywhere(t *testing.T) {
	si, s := newAdminSignIn(t, memory.NewData(), nil)
	w := postPassword(si, "192.0.2.1", passwordBody("admin", testAdminPassword, ""), map[string]string{"Origin": "https://debugger.example.com"})
	session := cookieNamed(w, sessionCookie)
	if w.Code != http.StatusNoContent || session == nil || !session.HttpOnly || !session.Secure || session.SameSite != http.SameSiteLaxMode {
		t.Fatalf("POST /auth/password = %d %q, cookie %+v", w.Code, w.Body.String(), session)
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	get.AddCookie(session)
	p, err := si.Authenticate(get)
	if err != nil || p.ID != AdminPrincipalID || p.Name != AdminName || p.Kind != PrincipalUser {
		t.Fatalf("session principal = %+v, %v", p, err)
	}
	if perms := s.Permissions(Caller{Principal: p, Client: "http"}); !slices.Contains(perms, string(authz.TokenManage)) {
		t.Errorf("the admin account's permissions = %v", perms)
	}
	post := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	post.AddCookie(session)
	if _, err := si.Authenticate(post); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("an unsafe request without the CSRF token = %v", err)
	}
	post.Header.Set(CSRFHeader, CSRFToken(get))
	if _, err := si.Authenticate(post); err != nil {
		t.Errorf("an unsafe request with the CSRF token = %v", err)
	}
}

// TestAdminPasswordRefusesBadSignIns: a wrong password or user, a body that
// is not JSON, a sign-in from another site and unknown fields are refused,
// none with a session.
func TestAdminPasswordRefusesBadSignIns(t *testing.T) {
	si, _ := newAdminSignIn(t, memory.NewData(), nil)
	for _, tc := range []struct {
		name   string
		body   string
		header map[string]string
		want   int
	}{
		{"wrong password", passwordBody("admin", testAdminPassword+"x", ""), nil, http.StatusUnauthorized},
		{"wrong user", passwordBody("root", testAdminPassword, ""), nil, http.StatusUnauthorized},
		{"no user", passwordBody("", testAdminPassword, ""), nil, http.StatusUnauthorized},
		{"form post", "user=admin&password=" + url.QueryEscape(testAdminPassword), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"another site", passwordBody("admin", testAdminPassword, ""), map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"unknown field", `{"user":"admin","password":"x","remember":true}`, nil, http.StatusBadRequest},
		{"unknown client", passwordBody("admin", testAdminPassword, "mcp"), nil, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A client address per case keeps the failure limit out of it.
			w := postPassword(si, "198.51.100."+string(rune('1'+len(tc.name)%9)), tc.body, tc.header)
			if w.Code != tc.want || len(w.Result().Cookies()) != 0 {
				t.Errorf("= %d %q with cookies %v, want %d", w.Code, w.Body.String(), w.Result().Cookies(), tc.want)
			}
		})
	}
}

// TestAdminPasswordLimitsFailures: after a client's failures in a window
// even the right password is refused, other clients are not, the instance
// as a whole has a limit too, and the next window allows sign-in again.
func TestAdminPasswordLimitsFailures(t *testing.T) {
	si, s := newAdminSignIn(t, memory.NewData(), nil)
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	s.admin.now = func() time.Time { return now }
	for range adminFailuresPerClient {
		if w := postPassword(si, "192.0.2.1", passwordBody("admin", "wrong", ""), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("a wrong password = %d", w.Code)
		}
	}
	if w := postPassword(si, "192.0.2.1", passwordBody("admin", testAdminPassword, ""), nil); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("the right password after too many failures = %d", w.Code)
	}
	if w := postPassword(si, "192.0.2.2", passwordBody("admin", testAdminPassword, ""), nil); w.Code != http.StatusNoContent {
		t.Errorf("another client's sign-in = %d", w.Code)
	}
	for i := range adminFailuresTotal {
		postPassword(si, "203.0.113."+string(rune('a'+i)), passwordBody("admin", "wrong", ""), nil)
	}
	if w := postPassword(si, "192.0.2.3", passwordBody("admin", testAdminPassword, ""), nil); w.Code != http.StatusTooManyRequests {
		t.Errorf("a sign-in after the instance's limit = %d", w.Code)
	}
	now = now.Add(adminFailureWindow)
	if w := postPassword(si, "192.0.2.1", passwordBody("admin", testAdminPassword, ""), nil); w.Code != http.StatusNoContent {
		t.Errorf("a sign-in in the next window = %d", w.Code)
	}
}

// TestAdminPasswordGivesTheCLIAnAPIToken: client "cli" gets an API token
// for the admin account instead of a session.
func TestAdminPasswordGivesTheCLIAnAPIToken(t *testing.T) {
	si, s := newAdminSignIn(t, memory.NewData(), nil)
	w := postPassword(si, "127.0.0.1", passwordBody("admin", testAdminPassword, "cli"), nil)
	var got PasswordToken
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || !strings.HasPrefix(got.Token, TokenPrefix) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("CLI sign-in = %d %q", w.Code, w.Body.String())
	}
	if !got.Expires.After(time.Now().Add(DefaultTokenTTL - time.Hour)) {
		t.Errorf("token expires %s", got.Expires)
	}
	p, err := s.AuthenticateToken(context.Background(), got.Token)
	if err != nil || p.ID != AdminPrincipalID {
		t.Fatalf("token principal = %+v, %v", p, err)
	}
	tokens, err := s.ListAPITokens(context.Background(), Caller{Principal: p, Client: "cli"}, APITokenFilter{})
	if err != nil || len(tokens) != 1 || tokens[0].Label != passwordTokenLabel {
		t.Errorf("admin's tokens = %+v, %v", tokens, err)
	}
}

// TestAdminAccountOffRefusesItsSessionsAndTokens: once the server runs
// without the admin password, the account's sessions and tokens are refused
// and the account holds no binding.
func TestAdminAccountOffRefusesItsSessionsAndTokens(t *testing.T) {
	data := memory.NewData()
	si, _ := newAdminSignIn(t, data, nil)
	session := cookieNamed(postPassword(si, "127.0.0.1", passwordBody("admin", testAdminPassword, ""), nil), sessionCookie)
	var tok PasswordToken
	if err := json.Unmarshal(postPassword(si, "127.0.0.1", passwordBody("admin", testAdminPassword, "cli"), nil).Body.Bytes(), &tok); err != nil || session == nil {
		t.Fatalf("signing in: %v, %v", err, session)
	}
	off := openPlatform(t, data, Options{Shared: true, Access: mustPolicy(t)})
	if _, err := off.AuthenticateToken(context.Background(), tok.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("the admin's token with the account off = %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
	r.AddCookie(session)
	if _, err := (&webSessions{s: off, store: off.repository}).authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("the admin's session with the account off = %v", err)
	}
	if perms := off.Permissions(Caller{Principal: adminPrincipal(), Client: "http"}); len(perms) != 0 {
		t.Errorf("the admin account off holds %v", perms)
	}
	if _, err := off.SignIn(SignInOptions{}); err == nil {
		t.Error("sign-in composed without OIDC or the admin account")
	}
}

// TestOpenChecksTheAdminPassword: a short password, or one without a team
// access policy, refuses to open.
func TestOpenChecksTheAdminPassword(t *testing.T) {
	for _, opts := range []Options{
		{Access: mustPolicy(t), AdminPassword: strings.Repeat("x", MinAdminPassword-1)},
		{AdminPassword: testAdminPassword},
	} {
		if s, err := openMemory(memory.NewData(), opts); err == nil {
			s.Close()
			t.Errorf("Open(%d-character password, access %v) succeeded", len(opts.AdminPassword), opts.Access != nil)
		}
	}
}

// TestSignInRoutesLoginByMethod: with the password, /auth/login goes to
// the web client's sign-in page, and method=oidc to the provider; with OIDC
// alone it goes to the provider; /auth/methods and /auth/cli list what is
// offered.
func TestSignInRoutesLoginByMethod(t *testing.T) {
	idp := oidctest.New(t)
	both, _ := newAdminSignIn(t, memory.NewData(), idp)
	s := openPlatform(t, memory.NewData(), Options{Shared: true, Access: mustPolicy(t)})
	o, err := s.OIDC(context.Background(), OIDCOptions{Issuer: idp.Issuer, ClientID: "web", RedirectURL: "https://d/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	oidcOnly, err := s.SignIn(SignInOptions{OIDC: o})
	if err != nil {
		t.Fatal(err)
	}
	password, _ := newAdminSignIn(t, memory.NewData(), nil)
	get := func(si *SignIn, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		si.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
		return w
	}
	for _, tc := range []struct {
		name   string
		si     *SignIn
		target string
		want   string
	}{
		{"password only", password, "/auth/login?return_to=%2Fruns%3Fx%3D1", "/sign-in?return_to=%2Fruns%3Fx%3D1"},
		{"password only asked for oidc", password, "/auth/login?method=oidc", "/sign-in?return_to=%2F"},
		{"foreign return_to", password, "/auth/login?return_to=https://evil.example", "/sign-in?return_to=%2F"},
		{"both", both, "/auth/login?return_to=%2Fruns", "/sign-in?return_to=%2Fruns"},
		{"both asked for oidc", both, "/auth/login?method=oidc&return_to=%2Fruns", idp.Issuer},
		{"oidc only", oidcOnly, "/auth/login", idp.Issuer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(tc.si, tc.target)
			if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), tc.want) {
				t.Errorf("GET %s = %d %s, want %s", tc.target, w.Code, w.Header().Get("Location"), tc.want)
			}
		})
	}
	for _, tc := range []struct {
		si          *SignIn
		methods     string
		cli         string
		cliNotFound bool
	}{
		{password, `{"password":true}`, `{"password":true}`, false},
		{both, `{"password":true,"oidc":{"issuer":"` + idp.Issuer + `"}}`, `{"issuer":"` + idp.Issuer + `","client_id":"cli","scopes":["openid","profile","email","offline_access"],"password":true}`, false},
		{oidcOnly, `{"password":false,"oidc":{"issuer":"` + idp.Issuer + `"}}`, "", true},
	} {
		if got := strings.TrimSpace(get(tc.si, "/auth/methods").Body.String()); got != tc.methods {
			t.Errorf("/auth/methods = %s, want %s", got, tc.methods)
		}
		w := get(tc.si, "/auth/cli")
		if tc.cliNotFound {
			if w.Code != http.StatusNotFound {
				t.Errorf("/auth/cli without a CLI method = %d", w.Code)
			}
		} else if got := strings.TrimSpace(w.Body.String()); got != tc.cli {
			t.Errorf("/auth/cli = %s, want %s", got, tc.cli)
		}
	}
	if w := postPassword(oidcOnly, "127.0.0.1", passwordBody("admin", testAdminPassword, ""), nil); w.Code != http.StatusNotFound {
		t.Errorf("POST /auth/password without the account = %d", w.Code)
	}
}
