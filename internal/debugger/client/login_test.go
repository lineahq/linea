package client

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// cliServer is a debugger server that only describes its CLI sign-in.
func cliServer(t *testing.T, info *platform.CLISignIn) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/cli" || info == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(info)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestLoginUsesTheDeviceFlowAndRefreshes: login asks the server for its
// provider, shows where to approve, polls through authorization_pending and
// slow_down at the provider's pace, and the refresh token renews the
// sign-in once, being rotated.
func TestLoginUsesTheDeviceFlowAndRefreshes(t *testing.T) {
	idp := oidctest.New(t)
	idp.PendingPolls = 2
	server := cliServer(t, &platform.CLISignIn{Issuer: idp.Issuer, ClientID: "cli", Scopes: []string{"openid", "offline_access"}})
	var waits []time.Duration
	var prompted string
	cred, err := Login(context.Background(), server, LoginOptions{
		Prompt: func(where, code string) { prompted = where + " " + code },
		Wait: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prompted != idp.Issuer+"/device?user_code=WDJB-MJHT WDJB-MJHT" || len(waits) != 3 || waits[0] != time.Second {
		t.Fatalf("prompt %q, waits %v", prompted, waits)
	}
	if cred.Issuer != idp.Issuer || cred.ClientID != "cli" || cred.TokenURL != idp.Issuer+"/token" || cred.RefreshToken == "" || cred.IDToken == "" ||
		time.Until(cred.Expiry) < 59*time.Minute || time.Until(cred.Expiry) > time.Hour {
		t.Fatalf("credential = %+v", cred)
	}

	renewed, err := Refresh(context.Background(), cred, nil)
	if err != nil || renewed.IDToken == "" || renewed.RefreshToken == "" || renewed.RefreshToken == cred.RefreshToken {
		t.Fatalf("refresh = %+v, %v", renewed, err)
	}
	if _, err := Refresh(context.Background(), cred, nil); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("a used refresh token = %v", err)
	}

	if _, err := Login(context.Background(), cliServer(t, nil), LoginOptions{}); err == nil || !strings.Contains(err.Error(), "no CLI sign-in") {
		t.Fatalf("a server without CLI sign-in = %v", err)
	}
	if _, err := Login(context.Background(), "https://", LoginOptions{}); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("an invalid server = %v", err)
	}
	remote := cliServer(t, &platform.CLISignIn{Issuer: "http://idp.example.com", ClientID: "cli"})
	if _, err := Login(context.Background(), remote, LoginOptions{}); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("a plain-HTTP provider off loopback = %v", err)
	}
}

// TestCredentialsAreKeptPerServer: credentials.json keeps one sign-in per
// normalized server URL, readable by the user only, and forgets one on
// logout.
func TestCredentialsAreKeptPerServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "linea", "credentials.json")
	if c, err := LoadCredential(path, "https://debugger.example.com"); c != nil || err != nil {
		t.Fatalf("no file = %+v, %v", c, err)
	}
	a := &Credential{Issuer: "https://idp", ClientID: "cli", TokenURL: "https://idp/token", RefreshToken: "r", IDToken: "a.b.c", Expiry: time.Unix(1e9, 0).UTC()}
	if err := SaveCredential(path, "https://debugger.example.com/", a); err != nil {
		t.Fatal(err)
	}
	if err := SaveCredential(path, "http://localhost:7070", &Credential{IDToken: "x.y.z"}); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadCredential(path, "https://debugger.example.com"); err != nil || c == nil || *c != *a {
		t.Fatalf("loaded = %+v, %v", c, err)
	}
	if info, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("file mode = %v, %v", info, err)
	}
	if err := SaveCredential(path, "https://debugger.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := LoadCredential(path, "https://debugger.example.com"); c != nil {
		t.Fatalf("after logout = %+v", c)
	}
	if c, _ := LoadCredential(path, "http://localhost:7070/"); c == nil || c.IDToken != "x.y.z" {
		t.Fatalf("the other server's sign-in = %+v", c)
	}
	if err := SaveCredential(path, "http://debugger.example.com", a); err == nil {
		t.Fatal("a plain-HTTP remote server was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"schema":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredential(path, "http://localhost:7070"); err == nil || !strings.Contains(err.Error(), "not a credentials file") {
		t.Fatalf("a foreign file = %v", err)
	}
}

// TestLoginTrustsThePrivateCA: a team server whose certificate a private CA
// signs is refused on the system roots alone; with that CA login reaches it,
// the credential keeps the CA through storage and refresh, and a client
// built with it verifies the server.
func TestLoginTrustsThePrivateCA(t *testing.T) {
	idp := oidctest.New(t)
	info := platform.CLISignIn{Issuer: idp.Issuer, ClientID: "cli", Scopes: []string{"openid", "offline_access"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/cli":
			_ = json.NewEncoder(w).Encode(info)
		case "/api/v1/session":
			_ = json.NewEncoder(w).Encode(map[string]any{"principal": map[string]string{"id": "user:alice", "name": "Alice"}, "permissions": []string{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	opts := LoginOptions{Wait: func(context.Context, time.Duration) error { return nil }}
	if _, err := Login(context.Background(), srv.URL, opts); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA = %v", err)
	}
	opts.CA = ca
	cred, err := Login(context.Background(), srv.URL, opts)
	if err != nil || cred.CA != string(ca) {
		t.Fatalf("with the CA = %+v, %v", cred, err)
	}
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := SaveCredential(path, srv.URL, cred); err != nil {
		t.Fatal(err)
	}
	if stored, err := LoadCredential(path, srv.URL); err != nil || stored == nil || stored.CA != string(ca) {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if renewed, err := Refresh(context.Background(), cred, nil); err != nil || renewed.CA != string(ca) {
		t.Fatalf("refreshed = %+v, %v", renewed, err)
	}
	c, err := NewWithCA(srv.URL, cred.IDToken, ca)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := c.Session(context.Background()); err != nil || s.Principal.Name != "Alice" {
		t.Fatalf("session = %+v, %v", s, err)
	}
	if _, err := NewWithCA(srv.URL, cred.IDToken, []byte("not a certificate")); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("a CA file without certificates = %v", err)
	}
}
