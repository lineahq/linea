package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// adminServer is a team server that offers the sign-in methods in info,
// accepts the admin password "right", and names the principal a request's
// bearer token signs in: admin for the API token it issued, device for any
// other.
type adminServer struct {
	url      string
	throttle bool
	attempts int
}

func startAdminServer(t *testing.T, info platform.CLISignIn) *adminServer {
	t.Helper()
	as := &adminServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli":
			_ = json.NewEncoder(w).Encode(info)
		case "/auth/password":
			var in platform.PasswordSignIn
			if !info.Password || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil {
				http.NotFound(w, r)
				return
			}
			as.attempts++
			switch {
			case as.throttle:
				w.Header().Set("Retry-After", "42")
				http.Error(w, "too many failed sign-ins", http.StatusTooManyRequests)
			case in.User != "admin" || in.Password != "right" || in.Client != "cli":
				http.Error(w, "wrong user name or password", http.StatusUnauthorized)
			default:
				_ = json.NewEncoder(w).Encode(platform.PasswordToken{Token: "odt_issued", Expires: time.Now().Add(time.Hour).UTC()})
			}
		case "/api/v1/session":
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer odt_issued" {
				_, _ = w.Write([]byte(`{"principal":{"id":"user:admin","kind":"user","name":"admin"},"permissions":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"principal":{"id":"user:device","kind":"user","name":"device"},"permissions":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	as.url = srv.URL
	return as
}

// loginStdinIs replaces the password sign-in's standard input, and whether
// it is a terminal, for the test; a terminal answers the prompt with input.
func loginStdinIs(t *testing.T, input string, terminal bool) {
	t.Helper()
	oldIn, oldTerm, oldRead := loginStdin, loginIsTerminal, loginReadSecret
	t.Cleanup(func() { loginStdin, loginIsTerminal, loginReadSecret = oldIn, oldTerm, oldRead })
	loginStdin = strings.NewReader(input)
	loginIsTerminal = func() bool { return terminal }
	loginReadSecret = func() ([]byte, error) { return []byte(input), nil }
}

// isolateCredentials points the credentials file at a temporary directory
// and returns its path.
func isolateCredentials(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv(serverTokenEnv, "")
	path, err := client.CredentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoginSignsInWithTheAdminPassword: a server that offers only the
// password, or either method with --password-stdin or --admin, signs in with
// the password; the stored credential is the returned API token, which later
// --server commands present, and the password appears in no output.
func TestLoginSignsInWithTheAdminPassword(t *testing.T) {
	both := platform.CLISignIn{Issuer: "http://127.0.0.1:1", ClientID: "cli", Password: true}
	for _, tc := range []struct {
		name     string
		info     platform.CLISignIn
		args     []string
		input    string
		terminal bool
	}{
		{name: "password only, stdin", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: "right\n"},
		{name: "password only, --admin prompt", info: platform.CLISignIn{Password: true}, args: []string{"--admin"}, input: "right", terminal: true},
		{name: "password only, CRLF line", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: "right\r\nignored\n"},
		{name: "password only, no newline", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: "right"},
		{name: "password only, terminal prompt without a flag", info: platform.CLISignIn{Password: true}, input: "right", terminal: true},
		{name: "both, --password-stdin", info: both, args: []string{"--password-stdin"}, input: "right\n"},
		{name: "both, --admin", info: both, args: []string{"--admin"}, input: "right", terminal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := isolateCredentials(t)
			loginStdinIs(t, tc.input, tc.terminal)
			srv := startAdminServer(t, tc.info)
			code, out, stderr := runCLI(t, append([]string{"login", "--server", srv.url}, tc.args...)...)
			if code != exitOK || !strings.Contains(out, "as admin (user:admin)") || strings.Contains(out+stderr, "right") {
				t.Fatalf("login: exit %d, %s%s", code, out, stderr)
			}
			if tc.terminal && !strings.Contains(stderr, "Admin password for "+srv.url) {
				t.Fatalf("no password prompt: %s", stderr)
			}
			cred, err := client.LoadCredential(path, srv.url)
			if err != nil || cred == nil || cred.IDToken != "odt_issued" || cred.RefreshToken != "" || time.Until(cred.Expiry) < 59*time.Minute {
				t.Fatalf("stored credential = %+v, %v", cred, err)
			}
			if got, err := signedIn(srv.url, true); err != nil || got == nil || got.IDToken != "odt_issued" {
				t.Fatalf("signedIn = %+v, %v", got, err)
			}
		})
	}
}

// TestLoginUsesTheDeviceFlowWhenBothMethodsAreOffered: without --admin or
// --password-stdin a server offering both signs in through its provider.
func TestLoginUsesTheDeviceFlowWhenBothMethodsAreOffered(t *testing.T) {
	path := isolateCredentials(t)
	loginStdinIs(t, "right\n", false)
	oldWait := loginWait
	loginWait = func(context.Context, time.Duration) error { return nil }
	t.Cleanup(func() { loginWait = oldWait })
	idp := oidctest.New(t)
	srv := startAdminServer(t, platform.CLISignIn{Issuer: idp.Issuer, ClientID: "cli", Scopes: []string{"openid", "offline_access"}, Password: true})
	code, out, stderr := runCLI(t, "login", "--server", srv.url)
	if code != exitOK || !strings.Contains(out, "as device (user:device)") || !strings.Contains(stderr, "To sign in, open ") || srv.attempts != 0 {
		t.Fatalf("login: exit %d, %s%s, %d password attempts", code, out, stderr, srv.attempts)
	}
	if cred, err := client.LoadCredential(path, srv.url); err != nil || cred == nil || cred.Issuer != idp.Issuer || cred.RefreshToken == "" {
		t.Fatalf("stored credential = %+v, %v", cred, err)
	}
}

// TestLoginExplainsAFailedPasswordSignIn: a wrong password, a throttled
// client, a server that does not accept the password and input that is not a
// password are each a clear error that stores nothing and never prints the
// password.
func TestLoginExplainsAFailedPasswordSignIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		info     platform.CLISignIn
		throttle bool
		args     []string
		input    string
		want     string
	}{
		{name: "wrong password", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: "hunter2-nope\n", want: "sign-in refused: wrong admin password"},
		{name: "throttled", info: platform.CLISignIn{Password: true}, throttle: true, args: []string{"--password-stdin"}, input: "right\n", want: "too many failed sign-ins; retry after 42 seconds"},
		{name: "no password on the server", info: platform.CLISignIn{Issuer: "http://127.0.0.1:1", ClientID: "cli"}, args: []string{"--password-stdin"}, input: "right\n", want: "does not accept the admin password"},
		{name: "not a terminal", info: platform.CLISignIn{Password: true}, input: "right\n", want: "pass --password-stdin"},
		{name: "empty stdin", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, want: "no password on standard input"},
		{name: "empty password", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: "\n", want: "the admin password is empty"},
		{name: "too long", info: platform.CLISignIn{Password: true}, args: []string{"--password-stdin"}, input: strings.Repeat("x", 2000) + "\n", want: "longer than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := isolateCredentials(t)
			loginStdinIs(t, tc.input, false)
			srv := startAdminServer(t, tc.info)
			srv.throttle = tc.throttle
			code, out, stderr := runCLI(t, append([]string{"login", "--server", srv.url}, tc.args...)...)
			if code != exitError || !strings.Contains(stderr, tc.want) || out != "" || strings.Contains(out+stderr, "hunter2-nope") {
				t.Fatalf("login: exit %d, %s%s", code, out, stderr)
			}
			if cred, err := client.LoadCredential(path, srv.url); err != nil || cred != nil {
				t.Fatalf("stored credential = %+v, %v", cred, err)
			}
		})
	}
}
