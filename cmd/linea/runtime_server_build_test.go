package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
)

// serverBuildManifest builds example.com/proc from ./proc beside it.
const serverBuildManifest = "dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\nreplaces:\n  - example.com/util => ../util\n"

// moduleDir writes a module's files under dir.
func moduleDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// untar lists an archive's entries as name to content, directories with a
// trailing slash.
func untar(t *testing.T, archive []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	r := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg {
			t.Fatalf("entry %q has type %c", h.Name, h.Typeflag)
		}
		body, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(body)
	}
}

// TestPackModulePacksRegularFilesOnly packs a module directory into an
// archive of relative forward-slash paths with go.mod at its root, leaving
// out version control and dependency directories and links, and packs the
// same bytes again for the same directory.
func TestPackModulePacksRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	moduleDir(t, dir, map[string]string{
		"go.mod":                "module example.com/proc\n",
		"proc.go":               "package proc\n",
		"internal/deep/deep.go": "package deep\n",
		".git/config":           "[core]\n",
		"node_modules/x/y.js":   "y\n",
	})
	linked := os.Symlink(filepath.Join(dir, "proc.go"), filepath.Join(dir, "link.go")) == nil

	archive, files, err := packModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := untar(t, archive)
	want := map[string]string{"go.mod": "module example.com/proc\n", "proc.go": "package proc\n", "internal/": "", "internal/deep/": "", "internal/deep/deep.go": "package deep\n"}
	if files != 3 || len(got) != len(want) {
		t.Fatalf("packed %d files, entries %v (symbolic link made: %t)", files, got, linked)
	}
	for name, content := range want {
		if body, ok := got[name]; !ok || body != content {
			t.Errorf("entry %q = %q, %v", name, body, ok)
		}
	}
	again, _, err := packModule(dir)
	if err != nil || !bytes.Equal(archive, again) {
		t.Fatalf("a second packing differs: %v", err)
	}

	// A change to a file changes the archive; a directory without go.mod,
	// or a file, is refused.
	moduleDir(t, dir, map[string]string{"proc.go": "package proc // changed\n"})
	if changed, _, err := packModule(dir); err != nil || bytes.Equal(archive, changed) {
		t.Fatalf("a changed file packed the same bytes: %v", err)
	}
	if _, _, err := packModule(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no go.mod") {
		t.Fatalf("a directory without go.mod = %v", err)
	}
	if _, _, err := packModule(filepath.Join(dir, "proc.go")); err == nil {
		t.Fatal("a file was packed")
	}
	if _, _, err := packModule(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing directory was packed")
	}
}

// TestBuildLocalModulesResolveBesideTheManifest finds the directories of
// path: entries and local replaces relative to the manifest file, lets a
// --source directory replace them and leaves out versioned replaces.
func TestBuildLocalModulesResolveBesideTheManifest(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "build", "ocb.yaml")
	text := serverBuildManifest + "  - example.com/remote v1.0.0 => example.com/fork v1.2.3\n" + "  - example.com/abs => " + filepath.ToSlash(filepath.Join(root, "abs")) + "\n"
	modules, err := buildLocalModules([]byte(text), manifestPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range modules {
		got = append(got, m.Module+"="+m.Dir)
	}
	want := []string{
		"example.com/abs=" + filepath.Join(root, "abs"),
		"example.com/proc=" + filepath.Join(root, "build", "proc"),
		"example.com/util=" + filepath.Join(root, "util"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("modules = %v, want %v", got, want)
	}

	over := filepath.Join(root, "elsewhere")
	modules, err = buildLocalModules([]byte(text), manifestPath, map[string]string{"example.com/proc": over, "example.com/new": over})
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, m := range modules {
		dirs[m.Module] = m.Dir
	}
	if len(dirs) != 4 || dirs["example.com/proc"] != over || dirs["example.com/new"] != over {
		t.Fatalf("with overrides = %v", dirs)
	}

	// Without local directories nothing is uploaded.
	if modules, err := buildLocalModules([]byte("dist:\n  otelcol_version: 0.162.0\n"), manifestPath, nil); err != nil || len(modules) != 0 {
		t.Fatalf("plain manifest = %v, %v", modules, err)
	}
	if _, err := buildLocalModules([]byte("replaces:\n  - not a replace\n"), manifestPath, nil); err == nil {
		t.Fatal("a malformed replace was accepted")
	}
}

// TestRuntimeBuildOnAServerRefusesLocalOnlySettings refuses settings a
// team server's builders own and git sources, before reading anything.
func TestRuntimeBuildOnAServerRefusesLocalOnlySettings(t *testing.T) {
	base := []string{"runtime", "build", "--name", "x", "--manifest", "missing.yaml", "--server", "http://127.0.0.1:1"}
	for flag, want := range map[string][]string{
		"--go-image":     {"--go-image", "golang:1"},
		"--module-proxy": {"--module-proxy", "https://proxy.example"},
		"--offline":      {"--offline"},
		"--git":          {"--git", "example.com/m=https://example.com/m.git"},
	} {
		code, _, stderr := runCLI(t, append(slices.Clone(base), want...)...)
		if code != exitError || !strings.Contains(stderr, flag) || !strings.Contains(stderr, "cannot be used with --server") {
			t.Errorf("%s: exit %d: %s", flag, code, stderr)
		}
	}
	if code, _, stderr := runCLI(t, base...); code != exitError || !strings.Contains(stderr, "missing.yaml") {
		t.Errorf("a missing manifest: exit %d: %s", code, stderr)
	}
}

// followed is a CLI command running in the background.
type followed struct {
	done   chan struct{}
	code   int
	stderr syncBuffer
}

func startFollowed(ctx context.Context, args ...string) *followed {
	f := &followed{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		f.code = run(ctx, args, io.Discard, &f.stderr)
	}()
	return f
}

// waitQueued waits until the command has printed that its build is queued,
// and returns the build's ID.
func (f *followed) waitQueued(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, line := range strings.Split(f.stderr.String(), "\n") {
			if rest, ok := strings.CutSuffix(strings.TrimPrefix(line, "build "), " queued"); ok && strings.HasPrefix(line, "build ") {
				return rest
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the build was not queued: %s", f.stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitBuilds waits until the server lists want builds.
func waitBuilds(t *testing.T, server string, want int) []api.RuntimeBuild {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, out, errOut := runCLI(t, "runtime", "builds", "--all", "--format", "json", "--server", server)
		var page api.RuntimeBuildPage
		if code != exitOK || json.Unmarshal([]byte(out), &page) != nil {
			t.Fatalf("runtime builds: exit %d: %s%s", code, out, errOut)
		}
		if len(page.Builds) >= want {
			return page.Builds
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server lists %d builds, want %d", len(page.Builds), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRuntimeBuildOnAServerUploadsAndFollows packs the manifest's directory,
// queues the build on a team server, follows it until it is cancelled by
// another client and by an interrupt, and lists the builds page by page.
func TestRuntimeBuildOnAServerUploadsAndFollows(t *testing.T) {
	old := buildPollInterval
	buildPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { buildPollInterval = old })
	lab := startLab(t)
	ctx := context.Background()

	root := t.TempDir()
	moduleDir(t, filepath.Join(root, "proc"), map[string]string{"go.mod": "module example.com/proc\n", "proc.go": "package proc\n"})
	manifest := filepath.Join(root, "ocb.yaml")
	if err := os.WriteFile(manifest, []byte("dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./proc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"runtime", "build", "--name", "edge", "--manifest", manifest, "--server", lab.URL}

	// Cancelled by another client: the command ends with the build.
	first := startFollowed(ctx, args...)
	firstID := first.waitQueued(t)
	c, err := connect(lab.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CancelRuntimeBuild(ctx, firstID); err != nil {
		t.Fatal(err)
	}
	<-first.done
	code, errOut := first.code, first.stderr.String()
	if code != exitError || !strings.Contains(errOut, "packed example.com/proc from ") ||
		!strings.Contains(errOut, "build of runtime edge was cancelled") {
		t.Fatalf("exit %d: %s", code, errOut)
	}

	// An interrupt cancels the build it follows.
	interrupted, interrupt := context.WithCancel(ctx)
	second := startFollowed(interrupted, args...)
	secondID := second.waitQueued(t)
	interrupt()
	<-second.done
	code, errOut = second.code, second.stderr.String()
	if code != exitError || !strings.Contains(errOut, "was cancelled") {
		t.Fatalf("interrupted: exit %d: %s", code, errOut)
	}
	if got, err := c.GetRuntimeBuild(ctx, secondID, 0); err != nil || got.State != api.BuildCancelled {
		t.Fatalf("interrupted build = %+v, %v", got, err)
	}

	// A manifest directory that is missing is refused before anything is
	// queued; a directory of another module is refused by the server.
	broken := filepath.Join(root, "broken.yaml")
	if err := os.WriteFile(broken, []byte("dist:\n  otelcol_version: 0.162.0\nprocessors:\n  - gomod: example.com/proc v0.0.0\n    path: ./nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI(t, "runtime", "build", "--name", "edge", "--manifest", broken, "--server", lab.URL); code != exitError || !strings.Contains(errOut, "the source of example.com/proc") {
		t.Fatalf("missing directory: exit %d: %s", code, errOut)
	}
	other := filepath.Join(root, "other")
	moduleDir(t, other, map[string]string{"go.mod": "module example.com/other\n"})
	if code, _, errOut := runCLI(t, "runtime", "build", "--name", "edge", "--manifest", manifest, "--source", "example.com/proc="+other, "--server", lab.URL); code != exitError || !strings.Contains(errOut, "example.com/other") {
		t.Fatalf("another module's directory: exit %d: %s", code, errOut)
	}
	builds := waitBuilds(t, lab.URL, 2)
	if len(builds) != 2 || builds[0].ID != secondID || builds[1].ID != firstID {
		t.Fatalf("builds after the refusals = %+v", builds)
	}

	// Listing: the CLI prints what HTTP returns, page by page.
	code, out, errOut := runCLI(t, "runtime", "builds", "--limit", "1", "--server", lab.URL)
	if code != exitOK || !strings.HasPrefix(out, "BUILD") || !strings.Contains(out, builds[0].ID) || strings.Contains(out, builds[1].ID) ||
		!strings.Contains(errOut, "more builds: pass --cursor ") {
		t.Fatalf("first page: exit %d: %s%s", code, out, errOut)
	}
	cli := parityCLI(t, ctx, []string{"runtime", "builds", "--all", "--format", "json", "--server", lab.URL})
	httpJSON := parityHTTP(t, ctx, http.MethodGet, lab.URL+"/api/v1/runtime-builds", nil, http.StatusOK)
	assertParityBytes(t, "builds", bytes.TrimSpace(reindent(t, httpJSON)), bytes.TrimSpace(cli))
	if code, _, errOut := runCLI(t, "runtime", "builds", "--all", "--cursor", "x", "--server", lab.URL); code != exitError || !strings.Contains(errOut, "cannot be combined") {
		t.Fatalf("--all with --cursor: exit %d: %s", code, errOut)
	}
	if code, _, errOut := runCLI(t, "runtime", "builds"); code != exitError || !strings.Contains(errOut, "--server") {
		t.Fatalf("builds without a server: exit %d: %s", code, errOut)
	}
}
