package platform

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitCloneLimitStopsLargeRepositories clones a repository holding 2 MiB
// that does not compress: a 1 MiB limit refuses it, naming the limit, and
// no limit or a larger one clones it.
func TestGitCloneLimitStopsLargeRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	work := t.TempDir()
	writeModule(t, work, "example.com/large")
	blob := make([]byte, 2<<20)
	_, _ = rand.Read(blob)
	if err := os.WriteFile(filepath.Join(work, "data.bin"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "--quiet")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "large")
	source := SourceOverride{Module: "example.com/large", Git: work}

	ctx := context.Background()
	if _, _, err := fetchGit(ctx, source, true, 1<<20); err == nil || !strings.Contains(err.Error(), "exceeds the clone limit of 1048576 bytes") {
		t.Fatalf("a clone over the limit: %v", err)
	}
	for _, limit := range []int64{0, 16 << 20} {
		files, commit, err := fetchGit(ctx, source, true, limit)
		if err != nil || commit == "" || len(files["data.bin"]) != len(blob) {
			t.Fatalf("limit %d: %d files, commit %q, %v", limit, len(files), commit, err)
		}
	}
}

// TestServerClonesOverHTTPSOnly: a server's clone refuses every transport
// but https before it connects, and its git sees none of the server's
// other environment or git config.
func TestServerClonesOverHTTPSOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, url := range []string{"git://127.0.0.1:1/repo", "ssh://git@127.0.0.1:1/repo", "ext::sh -c touch% /tmp/pwned", t.TempDir()} {
		_, _, err := fetchGit(context.Background(), SourceOverride{Git: url}, false, 0)
		if err == nil || !strings.Contains(err.Error(), "not allowed") && !strings.Contains(err.Error(), "https:// repositories only") {
			t.Errorf("server clone of %q = %v, want refused as a transport", url, err)
		}
	}
	t.Setenv("LINEA_TEST_SECRET", "s3cret")
	env := strings.Join(gitEnv(false), "\n")
	if strings.Contains(env, "s3cret") || !strings.Contains(env, "GIT_CONFIG_NOSYSTEM=1") {
		t.Errorf("server git environment carries the host's: %q", env)
	}
	if !strings.Contains(strings.Join(gitEnv(true), "\n"), "s3cret") {
		t.Error("the CLI's git lost the user's environment")
	}
}
