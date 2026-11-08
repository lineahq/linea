package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/server"
)

// teamStoreYAML is the store block of a test server config; writeTeam writes the
// secret files it names, and TestMain opens memory storage in its place.
const teamStoreYAML = `store:
  database_url_file: db-url
  objects:
    endpoint: http://127.0.0.1:8333
    bucket: linea
    path_style: true
    access_key_file: s3-access
    secret_key_file: s3-secret
`

// teamYAML is a valid server config; tests replace lines.
func teamYAML() string {
	return `version: 1
` + teamStoreYAML + `  storage_limit: 64MiB
authentication:
  trusted_headers:
    proxies: [127.0.0.1]
    issuer: proxy
    user: X-Forwarded-User
    groups: X-Forwarded-Groups
bindings:
  - group: platform
    role: viewer
    scope: "*"
`
}

// writeTeamSecrets writes the secret files teamStoreYAML names into dir.
func writeTeamSecrets(t *testing.T, dir string) {
	t.Helper()
	for name, v := range map[string]string{"db-url": "postgres://linea@127.0.0.1/linea\n", "s3-access": "access\n", "s3-secret": "secret\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTeam(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	path := filepath.Join(dir, "team.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeTeamIn writes a server config into dir, beside files it names.
func writeTeamIn(t *testing.T, dir, name, text string) string {
	t.Helper()
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTeamServerAuthenticatesEveryRequest serves a server config: requests
// without a credential are refused, identity headers from the proxy sign a
// user in, the user's API token works from the CLI and on MCP, and any Host
// is answered.
func TestTeamServerAuthenticatesEveryRequest(t *testing.T) {
	server := startTeam(t, writeTeam(t, teamYAML()))
	send := teamRequests(t, server)
	status := func(resp *http.Response) int {
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := status(send(http.MethodGet, "/api/v1/runs", nil, nil)); code != http.StatusUnauthorized {

		t.Fatalf("no credential: %d", code)
	}
	if code := status(send(http.MethodGet, "/api/v1/runs", nil, map[string]string{"X-Forwarded-User": "mallory"})); code != http.StatusForbidden {
		t.Fatalf("an unbound user: %d", code)
	}
	alice := map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "platform"}
	resp := send(http.MethodPost, "/api/v1/tokens", api.TokenRequest{Label: "laptop"}, alice)
	var created api.CreatedToken
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token: %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	t.Setenv(serverTokenEnv, created.Secret)
	code, out, stderr := runCLI(t, "token", "list", "--server", server)
	if code != exitOK || !strings.Contains(out, created.Token.ID) || !strings.Contains(out, "ACTIVE") {
		t.Fatalf("token list: exit %d, %s%s", code, out, stderr)
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitOK {
		t.Fatalf("runs with a token: exit %d, %s", code, stderr)
	}
	mcpCall := send(http.MethodPost, "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"},
		map[string]string{"Accept": "application/json, text/event-stream"})
	if code := status(mcpCall); code != http.StatusUnauthorized {
		t.Fatalf("MCP without a token: %d", code)
	}
	if code, out, stderr := runCLI(t, "token", "revoke", "--server", server, "--token", created.Token.ID); code != exitOK || !strings.Contains(out, "revoked") {
		t.Fatalf("token revoke: exit %d, %s%s", code, out, stderr)
	}
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitError || !strings.Contains(stderr, "invalid, expired or revoked") {
		t.Fatalf("runs with a revoked token: exit %d, %s", code, stderr)
	}
}

// minWorkerToken and defaultRunLimit are the team server's shortest worker
// token and default run limit.
const (
	minWorkerToken  = 32
	defaultRunLimit = 16 << 20
)

// startTeam serves the server config with MCP on a loopback port until the test
// ends, and returns its URL.
func startTeam(t *testing.T, path string) string {
	t.Helper()
	return startTeamAt(t, path, "127.0.0.1:0")
}

// startTeamAt is startTeam on the address given.
func startTeamAt(t *testing.T, path, addr string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 16)
	done := make(chan int, 1)
	go func() {
		done <- server.Main(ctx, []string{"--config", path, "--addr", addr, "--mcp"}, io.Discard, notices)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("serve exit = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	select {
	case notice := <-notices:
		const prefix = "serving API on "
		if !strings.HasPrefix(notice, prefix) {
			t.Fatalf("unexpected startup: %s", notice)
		}
		return strings.TrimSuffix(strings.Fields(strings.TrimPrefix(notice, prefix))[0], "/api/v1")
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	return ""
}

// teamRequests sends requests to a team server under another Host name,
// with a JSON body and the headers and cookies given.
func teamRequests(t *testing.T, server string) func(method, path string, body any, header map[string]string, cookies ...*http.Cookie) *http.Response {
	return func(method, path string, body any, header map[string]string, cookies ...*http.Cookie) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		req, err := http.NewRequest(method, server+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "debugger.example.com"
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		for _, c := range cookies {
			req.AddCookie(c)
		}
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
}
