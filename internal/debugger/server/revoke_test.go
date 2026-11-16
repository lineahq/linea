package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/store/pg"
)

// TestRevokeAdminEndsTheAdminsTokensAndSessions: the subcommand reads the
// server config, ends what the admin account holds in the server's storage
// and prints counts, with or without the account enabled, and again finds
// nothing.
func TestRevokeAdminEndsTheAdminsTokensAndSessions(t *testing.T) {
	for name, text := range map[string]string{"admin on": adminYAML, "admin off": teamYAML()} {
		t.Run(name, func(t *testing.T) {
			data := memory.NewData()
			previous := OpenStore
			OpenStore = func(_ context.Context, c pg.Config) (artifact.Repository, error) {
				return memory.Open(data, c.Limits), nil
			}
			t.Cleanup(func() { OpenStore = previous })

			ctx := context.Background()
			seed := memory.Open(data, artifact.Limits{})
			defer seed.Close()
			now := time.Now().UTC()
			for n, owner := range []string{platform.AdminPrincipalID, "user:alice"} {
				if err := seed.SaveToken(ctx, artifact.TokenRecord{ID: fmt.Sprintf("%032x", n+1), Hash: fmt.Sprintf("%064x", n+1), Principal: owner, Kind: artifact.TokenUser,
					Label: "token", CreatedBy: owner, Created: now.Add(-time.Hour), Expires: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				if err := seed.SaveSession(ctx, artifact.SessionRecord{Hash: fmt.Sprintf("%064x", n+10), Principal: owner, Created: now.Add(-time.Hour), Expires: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
			}

			dir := t.TempDir()
			writeTeamSecrets(t, dir)
			if err := os.WriteFile(filepath.Join(dir, "admin-password"), []byte(adminPassword+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := writeTeamIn(t, dir, "team", text)
			for run, want := range []string{"revoked 1 API tokens and ended 1 web sessions", "revoked 0 API tokens and ended 0 web sessions"} {
				var stdout, stderr bytes.Buffer
				if code := Main(ctx, []string{"revoke-admin", "--config", path}, &stdout, &stderr); code != exitOK {
					t.Fatalf("run %d exit = %d: %s", run+1, code, stderr.String())
				}
				if got := strings.TrimSpace(stdout.String()); !strings.HasPrefix(got, want) || strings.Count(got, "\n") != 0 {
					t.Errorf("run %d printed %q, want one line starting %q", run+1, got, want)
				}
			}
			if _, err := seed.SessionByHash(ctx, fmt.Sprintf("%064x", 11)); err != nil {
				t.Errorf("another principal's session = %v", err)
			}
			if _, err := seed.SessionByHash(ctx, fmt.Sprintf("%064x", 10)); err == nil {
				t.Error("the admin's session survived")
			}
		})
	}
}

func TestRevokeAdminRefusesBadArguments(t *testing.T) {
	path := writeTeam(t, teamYAML())
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"extra argument": {[]string{"revoke-admin", "--config", path, "now"}, `unexpected argument "now"`},
		"unknown flag":   {[]string{"revoke-admin", "--all"}, "flag provided but not defined"},
		"no config":      {[]string{"revoke-admin"}, noConfig},
		"missing file":   {[]string{"revoke-admin", "--config", path + ".missing"}, "server config"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Main(context.Background(), tc.args, &stdout, &stderr); code != exitError {
				t.Fatalf("exit = %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want it to name %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q", stdout.String())
			}
		})
	}
}
