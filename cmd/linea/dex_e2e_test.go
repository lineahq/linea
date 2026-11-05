package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// dexImage is the identity provider of the OIDC gate, pinned.
const dexImage = "ghcr.io/dexidp/dex:v2.41.1"

// dexPassword is "password" hashed, as in Dex's own examples.
const dexPassword = "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"

// TestTeamServerWithDex is the T-6c gate: a team server signs people in
// with a real Dex, in the browser with the login form and from the CLI with
// the device flow; their bindings decide what they may do over HTTP, the CLI
// and MCP; and a misconfigured provider refuses startup. It needs Docker and
// runs only with LINEA_DOCKER_TESTS=1.
func TestTeamServerWithDex(t *testing.T) {
	if os.Getenv("LINEA_DOCKER_TESTS") != "1" {
		t.Skip("set LINEA_DOCKER_TESTS=1 to run the Dex gate")
	}
	dexPort, serverPort := freePort(t), freePort(t)
	server := "http://127.0.0.1:" + serverPort
	issuer := startDex(t, dexPort, server+"/auth/callback")

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "client-secret"), []byte("web-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTeamSecrets(t, dir)
	alice, bob := platform.UserID(issuer, dexSubject("alice-id")), platform.UserID(issuer, dexSubject("bob-id"))
	team := func(issuer string) string {
		text := `version: 1
` + teamStoreYAML + `authentication:
  oidc:
    issuer: ` + issuer + `
    client_id: web
    client_secret_file: client-secret
    redirect_url: ` + server + `/auth/callback
    cli_client_id: cli
bindings:
  - principal: ` + alice + `
    role: engineer
    scope: "*"
  - principal: ` + bob + `
    role: viewer
    scope: "*"
`
		path := filepath.Join(dir, "team.yaml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	wrong := team(issuer + "/elsewhere")
	if code, _, stderr := runServer(t, "--config", wrong, "--addr", "127.0.0.1:"+serverPort); code != exitError || !strings.Contains(stderr, "OIDC provider "+issuer+"/elsewhere") {
		t.Fatalf("serve with the wrong issuer: exit %d, %s", code, stderr)
	}
	if got := startTeamAt(t, team(issuer), "127.0.0.1:"+serverPort); got != server {
		t.Fatalf("served on %s, want %s", got, server)
	}

	// The browser: the login form, then the session and its CSRF token.
	aliceWeb, aliceSession := browserSignIn(t, server, "alice@example.com")
	bobWeb, bobSession := browserSignIn(t, server, "bob@example.com")
	if aliceSession.Principal.ID != alice || aliceSession.Principal.Name != "alice" || bobSession.Principal.ID != bob {
		t.Fatalf("sessions: %+v, %+v", aliceSession, bobSession)
	}
	if code := browserSend(t, aliceWeb, http.MethodPost, server+"/api/v1/tokens", api.TokenRequest{Label: "laptop"}, aliceSession.CSRFToken); code != http.StatusCreated {
		t.Fatalf("alice creates a token: %d", code)
	}
	if code := browserSend(t, aliceWeb, http.MethodPost, server+"/api/v1/tokens", api.TokenRequest{Label: "laptop"}, ""); code != http.StatusUnauthorized {
		t.Fatalf("a change without the CSRF token: %d", code)
	}
	if code := browserSend(t, aliceWeb, http.MethodDelete, server+"/api/v1/runs/run-missing", nil, aliceSession.CSRFToken); code != http.StatusNotFound {
		t.Fatalf("alice, an engineer, deletes a missing run: %d", code)
	}
	if code := browserSend(t, bobWeb, http.MethodDelete, server+"/api/v1/runs/run-missing", nil, bobSession.CSRFToken); code != http.StatusForbidden {
		t.Fatalf("bob, a viewer, deletes a run: %d", code)
	}
	if code := browserSend(t, bobWeb, http.MethodGet, server+"/api/v1/runs", nil, ""); code != http.StatusOK {
		t.Fatalf("bob lists runs: %d", code)
	}

	// The CLI: the device flow, approved in the browser as bob.
	config := t.TempDir()
	for _, env := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		t.Setenv(env, config)
	}
	t.Setenv(serverTokenEnv, "")
	var prompt syncBuffer
	approved := false
	loginWait = func(ctx context.Context, d time.Duration) error {
		if !approved {
			approved = true
			approveDevice(t, &prompt)
		}
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { loginWait = nil })
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if code := run(ctx, []string{"login", "--server", server}, &out, &prompt); code != exitOK || !strings.Contains(out.String(), "as bob ("+bob+")") {
		t.Fatalf("login: exit %d, %s%s", code, out.String(), prompt.String())
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitOK {
		t.Fatalf("runs after login: exit %d, %s", code, stderr)
	}
	path, err := client.CredentialsPath()
	if err != nil {
		t.Fatal(err)
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
	renewed, _ := client.LoadCredential(path, server)
	if renewed == nil || renewed.RefreshToken == used {
		t.Fatalf("not renewed: %+v", renewed)
	}

	// MCP: the CLI's token from Dex, and nothing else.
	send := teamRequests(t, server)
	ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}
	for token, want := range map[string]int{renewed.IDToken: http.StatusOK, "not-a-token": http.StatusUnauthorized} {
		resp := send(http.MethodPost, "/mcp", ping, map[string]string{"Accept": "application/json, text/event-stream", "Authorization": "Bearer " + token})
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("MCP: %d, want %d", resp.StatusCode, want)
		}
	}
}

// startDex runs Dex until the test ends, with the debugger's web and CLI
// clients and the users alice and bob, and returns its issuer.
func startDex(t *testing.T, port, redirect string) string {
	t.Helper()
	issuer := "http://127.0.0.1:" + port + "/dex"
	config := `issuer: ` + issuer + `
storage:
  type: memory
web:
  http: 0.0.0.0:5556
oauth2:
  skipApprovalScreen: true
enablePasswordDB: true
staticClients:
  - id: web
    name: web
    secret: web-secret
    redirectURIs: ["` + redirect + `"]
  - id: cli
    name: cli
    public: true
    redirectURIs: ["/device/callback"]
staticPasswords:
  - {email: alice@example.com, username: alice, userID: alice-id, hash: "` + dexPassword + `"}
  - {email: bob@example.com, username: bob, userID: bob-id, hash: "` + dexPassword + `"}
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := "linea-dex-" + hex.EncodeToString(b[:])
	docker := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v\n%s", args[0], err, out)
		}
	}
	docker("create", "--name", name, "-p", "127.0.0.1:"+port+":5556", dexImage, "dex", "serve", "/etc/dex/gate.yaml")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	docker("cp", path, name+":/etc/dex/gate.yaml")
	docker("start", name)
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(issuer + "/.well-known/openid-configuration")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return issuer
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("Dex did not start: %v\n%s", err, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dexSubject is the subject Dex gives a static password user: its user ID
// and connector, protobuf-encoded.
func dexSubject(userID string) string {
	b := append([]byte{0x0a, byte(len(userID))}, userID...)
	b = append(append(b, 0x12, byte(len("local"))), "local"...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

var formAction = regexp.MustCompile(`<form[^>]*action="([^"]+)"`)

// submitLogin fills in the Dex login form on page with the user's email.
func submitLogin(t *testing.T, c *http.Client, page *http.Response, email string) *http.Response {
	t.Helper()
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	m := formAction.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no login form at %s: %s", page.Request.URL, body)
	}
	action, err := page.Request.URL.Parse(html.UnescapeString(string(m[1])))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.PostForm(action.String(), url.Values{"login": {email}, "password": {"password"}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// browserSignIn signs a browser in through the login form and returns it,
// signed in, with its session.
func browserSignIn(t *testing.T, server, email string) (*http.Client, api.Session) {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	page, err := c.Get(server + "/auth/login?return_to=/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	resp := submitLogin(t, c, page, email)
	defer resp.Body.Close()
	var s api.Session
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/api/v1/session" {
		t.Fatalf("%s signs in: %d at %s, %v", email, resp.StatusCode, resp.Request.URL, err)
	}
	return c, s
}

func browserSend(t *testing.T, c *http.Client, method, target string, body any, csrf string) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set(platform.CSRFHeader, csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

var verificationURL = regexp.MustCompile(`open (\S+) and enter the code`)

// approveDevice approves the code login printed, signing in as bob.
func approveDevice(t *testing.T, prompt *syncBuffer) {
	t.Helper()
	m := verificationURL.FindStringSubmatch(prompt.String())
	if m == nil {
		t.Fatalf("login printed no verification URL: %s", prompt.String())
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	page, err := c.Get(m[1])
	if err != nil {
		t.Fatal(err)
	}
	verify := submitCode(t, c, page)
	resp := submitLogin(t, c, verify, "bob@example.com")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Login Successful") {
		t.Fatalf("device approval: %d %s", resp.StatusCode, body)
	}
}

// submitCode submits Dex's device page, its user code filled in.
func submitCode(t *testing.T, c *http.Client, page *http.Response) *http.Response {
	t.Helper()
	code := page.Request.URL.Query().Get("user_code")
	page.Body.Close()
	action, _ := page.Request.URL.Parse("device/auth/verify_code")
	resp, err := c.PostForm(action.String(), url.Values{"user_code": {code}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// syncBuffer is a bytes.Buffer safe to read while a command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
