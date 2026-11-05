package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestServerConfigComposesTheServer reads every setting from the server config,
// resolving relative paths against its directory, and refuses a file that
// leaves a team server unauthenticated, unbound, in process or ambiguous.
func TestServerConfigComposesTheServer(t *testing.T) {
	tm, err := loadConfig(writeTeam(t, teamYAML()))
	if err != nil {
		t.Fatal(err)
	}
	if tm.store.DatabaseURL != "postgres://linea@127.0.0.1/linea" || tm.store.S3.Bucket != "linea" || tm.store.S3.SecretKey != "secret" || !tm.store.S3.UsePathStyle || tm.store.Limits.StorageBytes != 64<<20 || tm.limits.storage != 64<<20 || tm.limits.run != defaultRunLimit ||
		tm.headers.Proxies[0].String() != "127.0.0.1/32" {
		t.Fatalf("team = %+v", tm)
	}
	if tm.queue != 64 || tm.principals != (platform.PrincipalLimits{Runs: 32, Captures: 1}) {
		t.Fatalf("default limits: queue %d, %+v", tm.queue, tm.principals)
	}
	if tm.retention != (platform.RetentionPolicy{Runs: 10_000, Age: 30 * 24 * time.Hour}) {
		t.Fatalf("default retention: %+v", tm.retention)
	}
	for yaml, want := range map[string]platform.RetentionPolicy{
		"retention: {runs: 500, age: 7d}\n": {Runs: 500, Age: 7 * 24 * time.Hour},
		"retention: {age: 36h}\n":           {Runs: 10_000, Age: 36 * time.Hour},
	} {
		if tm, err := loadConfig(writeTeam(t, teamYAML()+yaml)); err != nil || tm.retention != want {
			t.Fatalf("%s: %+v, %v", yaml, tm, err)
		}
	}
	limited := teamYAML() + "limits: {queue: 8, runs_per_principal: 4, captures_per_principal: 2}\n"
	if tm, err := loadConfig(writeTeam(t, limited)); err != nil || tm.queue != 8 || tm.principals != (platform.PrincipalLimits{Runs: 4, Captures: 2}) {
		t.Fatalf("limits: %+v, %v", tm, err)
	}
	if tm, err := loadConfig(writeTeam(t, teamYAML())); err != nil || tm.connections != defaultTeamConnections || tm.gitClone != defaultGitCloneSize {
		t.Fatalf("default connection and clone limits: %+v, %v", tm, err)
	}
	bounded := teamYAML() + "limits: {connections: 64, git_clone_size: 32MiB}\n"
	if tm, err := loadConfig(writeTeam(t, bounded)); err != nil || tm.connections != 64 || tm.gitClone != 32<<20 {
		t.Fatalf("connection and clone limits: %+v, %v", tm, err)
	}

	valid := teamYAML()
	for name, tc := range map[string]struct{ old, new, want string }{
		"version":          {"version: 1", "version: 2", "version 2"},
		"unknown key":      {"version: 1", "version: 1\noidc: {}", "field oidc not found"},
		"no database":      {"  database_url_file: db-url\n", "", "store needs database_url_file"},
		"missing secret":   {"secret_key_file: s3-secret", "secret_key_file: missing", "store.objects.secret_key_file"},
		"bad endpoint":     {"endpoint: http://127.0.0.1:8333", "endpoint: s3.example.com", "store.objects.endpoint"},
		"bad size":         {"64MiB", "lots", "store.storage_limit"},
		"no retained runs": {"version: 1", "version: 1\nretention: {runs: 0}", "retention.runs 0"},
		"short retention":  {"version: 1", "version: 1\nretention: {age: 10m}", "retention.age"},
		"bad retention":    {"version: 1", "version: 1\nretention: {age: 1w}", "retention.age"},
		"no sign-in":       {"authentication:\n  trusted_headers:\n    proxies: [127.0.0.1]\n    issuer: proxy\n    user: X-Forwarded-User\n    groups: X-Forwarded-Groups\n", "", "needs a sign-in method"},
		"no proxies":       {"proxies: [127.0.0.1]", "proxies: []", "needs proxies, issuer and user"},
		"bad proxy":        {"proxies: [127.0.0.1]", "proxies: [proxy.example.com]", "not an address or CIDR prefix"},
		"no bindings":      {"bindings:\n  - group: platform\n    role: viewer\n    scope: \"*\"\n", "", "bindings are required"},
		"no scope":         {"    scope: \"*\"\n", "", "bindings[0]: scope"},
		"unknown role":     {"role: viewer", "role: superuser", "bindings:"},
		"host paths":       {"version: 1", "version: 1\npaths:\n  allow: [scenarios]", "field paths not found"},
		"host environment": {"version: 1", "version: 1\nenvironment:\n  import: [HOME]", "field environment not found"},
		"run above store":  {"storage_limit: 64MiB", "storage_limit: 64MiB\n  run_limit: 128MiB", "exceeds"},
		"remote runtimes":  {"version: 1", "version: 1\nremote_runtimes: runtimes.json", "field remote_runtimes not found"},
		"listed workers":   {"version: 1", "version: 1\nworkers: []", "field workers not found"},
		"default runtime":  {"version: 1", "version: 1\nruntime: edge", "field runtime not found"},
		"duplicate key":    {"version: 1", "version: 1\nversion: 1", "already defined"},
		"two subjects":     {"  - group: platform\n", "  - group: platform\n    principal: user:x\n", "exactly one principal or group"},
		"scope wrong kind": {"scope: \"*\"", "scope: team:x", "bindings[0]"},
		"oidc incomplete":  {"authentication:\n", "authentication:\n  oidc:\n    issuer: https://idp.example.com\n", "authentication.oidc: issuer, client_id and redirect_url are required"},
		"oidc plain http": {"authentication:\n", "authentication:\n  oidc: {issuer: http://idp.example.com, client_id: web, redirect_url: https://d.example.com/auth/callback}\n",
			"issuer \"http://idp.example.com\": give an https URL"},
		"oidc redirect": {"authentication:\n", "authentication:\n  oidc: {issuer: https://idp.example.com, client_id: web, redirect_url: https://d.example.com/callback}\n",
			"ending in /auth/callback"},
		"oidc secret": {"authentication:\n", "authentication:\n  oidc: {issuer: https://idp.example.com, client_id: web, redirect_url: https://d.example.com/auth/callback, client_secret_file: missing}\n",
			"client_secret_file"},
		"limit zero":      {"version: 1", "version: 1\nlimits: {captures_per_principal: 0}", "limits.captures_per_principal 0: give a number from 1 to 1024"},
		"limit too high":  {"version: 1", "version: 1\nlimits: {queue: 4096}", "limits.queue 4096"},
		"runs over queue": {"version: 1", "version: 1\nlimits: {queue: 8}", "limits.runs_per_principal 32 exceeds limits.queue 8"},
		"few connections": {"version: 1", "version: 1\nlimits: {connections: 4}", "limits.connections 4: give a number from 16 to 65536"},
		"clone size":      {"version: 1", "version: 1\nlimits: {git_clone_size: lots}", "limits.git_clone_size: invalid size"},
		"oidc session": {"authentication:\n", "authentication:\n  oidc: {issuer: https://idp.example.com, client_id: web, redirect_url: https://d.example.com/auth/callback, session_ttl: 30d}\n",
			"session_ttl"},
	} {
		text := strings.Replace(valid, tc.old, tc.new, 1)
		if text == valid {
			t.Fatalf("%s: the replacement does not apply", name)
		}
		if _, err := loadConfig(writeTeam(t, text)); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "server config ") {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestServeRefusesWhatTheServerConfigDoesNotCompose: serve is composed by its team
// file alone, so the flags of the removed local mode are unknown, and it
// serves off loopback only over TLS unless --allow-http.
func TestServeRefusesWhatTheServerConfigDoesNotCompose(t *testing.T) {
	path := writeTeam(t, teamYAML())
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--config", path, "--storage-limit", "1GiB"}, "flag provided but not defined: -storage-limit"},
		{[]string{"--config", path, "--run-limit", "4MiB"}, "flag provided but not defined: -run-limit"},
		{[]string{"--config", path, "--allow-path", "."}, "flag provided but not defined: -allow-path"},
		{[]string{"--config", path, "--import-env", "HOME"}, "flag provided but not defined: -import-env"},
		{[]string{"--team", path}, "flag provided but not defined: -team"},
		{[]string{"--config", path, "--scenario", "s.yaml"}, "flag provided but not defined: -scenario"},
		{[]string{"--config", path, "--tls-cert-file", "cert.pem"}, "go together"},
		{[]string{"--config", path, "--tls-cert-file", "c", "--tls-key-file", "k", "--allow-http"}, "--allow-http cannot be used with --tls-cert-file"},
		{nil, noConfig},
		{[]string{"--allow-http"}, noConfig},
		{[]string{"--addr", "0.0.0.0:0"}, noConfig},
		{[]string{"--mcp", "--addr", "127.0.0.1:0"}, noConfig},
		{[]string{"--config", path, "--addr", "0.0.0.0:0"}, "--allow-http"},
		{[]string{"--config", path, "--tls-cert-file", "missing.pem", "--tls-key-file", "missing.key"}, "--tls-cert-file and --tls-key-file:"},
		{[]string{"--config", writeTeam(t, "version: 1\n")}, "store needs database_url_file"},
	} {
		code, _, stderr := runServer(t, tc.args...)
		if code != exitError || !strings.Contains(stderr, tc.want) {
			t.Errorf("linea-server %v: exit %d, %s", tc.args, code, stderr)
		}
	}
}

// TestServerConfigHoldsTheWorkerRegistrationToken reads the token connecting
// workers present from the file the server config names, and refuses a token
// too short to guess safely.
func TestServerConfigHoldsTheWorkerRegistrationToken(t *testing.T) {
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	token := strings.Repeat("r", minWorkerToken)
	for name, v := range map[string]string{"registration": token + "\n", "short": "short\n", "spaced": token + " " + token + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tm, err := loadConfig(writeTeamIn(t, dir, "registration", teamYAML()+"worker_token_file: registration\n"))
	if err != nil || tm.workerToken != token {
		t.Fatalf("team = %+v, %v", tm, err)
	}
	if tm, err := loadConfig(writeTeamIn(t, dir, "none", teamYAML())); err != nil || tm.workerToken != "" {
		t.Fatalf("a server config without a registration token = %+v, %v", tm, err)
	}
	for _, file := range []string{"short", "spaced", "missing"} {
		if _, err := loadConfig(writeTeamIn(t, dir, file, teamYAML()+"worker_token_file: "+file+"\n")); err == nil || !strings.Contains(err.Error(), "worker_token_file") {
			t.Fatalf("worker_token_file %s = %v", file, err)
		}
	}
}

// TestServerConfigHoldsTheBuilderToken reads the token builders present from
// builds.token_file, and refuses a short one or the workers' token.
func TestServerConfigHoldsTheBuilderToken(t *testing.T) {
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	workers, builders := strings.Repeat("r", minWorkerToken), strings.Repeat("b", minWorkerToken)
	for name, v := range map[string]string{"registration": workers + "\n", "builders": builders + "\n", "short": "short\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tm, err := loadConfig(writeTeamIn(t, dir, "builds", teamYAML()+"worker_token_file: registration\nbuilds:\n  token_file: builders\n"))
	if err != nil || tm.buildToken != builders || tm.workerToken != workers {
		t.Fatalf("team = %+v, %v", tm, err)
	}
	for _, file := range []string{"short", "missing", "registration"} {
		if _, err := loadConfig(writeTeamIn(t, dir, "bad", teamYAML()+"worker_token_file: registration\nbuilds:\n  token_file: "+file+"\n")); err == nil || !strings.Contains(err.Error(), "builds.token_file") {
			t.Fatalf("builds.token_file %s = %v", file, err)
		}
	}
}
