package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/store/pg"
)

// TestMain keeps the team servers tests start in memory instead of
// PostgreSQL.
func TestMain(m *testing.M) {
	OpenStore = func(_ context.Context, c pg.Config) (artifact.Repository, error) { return memory.New(c.Limits), nil }
	os.Exit(m.Run())
}

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

// writeTeam writes a server config beside the secret files it names.
func writeTeam(t *testing.T, text string) string {
	t.Helper()
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	return writeTeamIn(t, dir, "team", text)
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

// Startup writes synchronize tests with the real listener without polling or
// concurrent reads of a bytes.Buffer. Only startup notices reach this writer.
type serveNotices chan string

func (n serveNotices) Write(p []byte) (int, error) {
	n <- string(p)
	return len(p), nil
}

// runServer runs Main with args, as the linea-server binary does.
func runServer(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}
