package server

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// serveStartup runs serve with a context that is already over and returns
// what it wrote at startup.
func serveStartup(t *testing.T, configFile string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := Main(ctx, []string{"--config", configFile, "--addr", "127.0.0.1:0", "--mcp"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	return stderr.String()
}

// TestServePrintsTheServerConfigsPolicy: startup states what the server
// config composes: the endpoints, the sign-in methods and the storage, and
// that requests read no host file or host environment variable.
func TestServePrintsTheServerConfigsPolicy(t *testing.T) {
	startup := serveStartup(t, writeTeam(t, teamYAML()))
	for _, want := range []string{"serving API on http://127.0.0.1:", "/api/v1 (contract: /api/v1/openapi.json)", "serving MCP on http://127.0.0.1:",
		"team mode: requests are authenticated by API token, identity headers from 127.0.0.1/32", "storage: PostgreSQL, payloads in object storage bucket linea",
		"requests read no host file or host environment variable"} {
		if !strings.Contains(startup, want) {
			t.Errorf("startup lacks %q:\n%s", want, startup)
		}
	}
	if strings.Contains(startup, "in memory") || strings.Contains(startup, "host imports") || strings.Contains(startup, "only under") {
		t.Errorf("startup describes a mode that is gone:\n%s", startup)
	}
}

// TestServeHasNoLocalMode: serve without a server config is refused, whatever the
// address, and says where a server runs.
func TestServeHasNoLocalMode(t *testing.T) {
	for _, args := range [][]string{nil, {"--addr", "127.0.0.1:0"}, {"--addr", "0.0.0.0:0"}} {
		var stdout, stderr bytes.Buffer
		if code := Main(context.Background(), args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), noConfig) {
			t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
		}
	}
}

// TestServeRefusesTheRemovedWorkspaceFlag: the file workspace is gone, so
// --workspace is an unknown flag instead of a silently ignored one.
func TestServeRefusesTheRemovedWorkspaceFlag(t *testing.T) {
	code, _, stderr := runServer(t, "--addr", "127.0.0.1:0", "--workspace", t.TempDir())
	if code != exitError || !strings.Contains(stderr, "flag provided but not defined: -workspace") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
