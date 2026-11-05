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
