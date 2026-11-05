package platform

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/release"
)

const minimalManifest = "dist:\n  otelcol_version: 0.162.0\n"

func waitBuild(t *testing.T, s *Service, id string) *RuntimeBuild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	v, err := s.WaitRuntimeBuild(ctx, testCaller, id, 0)
	if err != nil || !v.Terminal() {
		t.Fatalf("build = %+v, %v", v, err)
	}
	return v
}

func wantDiagnostic(t *testing.T, err error, code, text string) {
	t.Helper()
	wantCode(t, err, CodeValidationFailed)
	var e *Error
	errors.As(err, &e)
	for _, d := range e.Diagnostics {
		if d.Code == code && strings.Contains(d.Message, text) {
			return
		}
	}
	t.Fatalf("no %s diagnostic saying %q: %+v", code, text, e.Diagnostics)
}

// TestBuildsFollowTheSupportTable refuses a release before the oldest
// supported one, naming it; builds a verified release with its dependency
// set and a release outside the table as unverified.
func TestBuildsFollowTheSupportTable(t *testing.T) {
	ctx := context.Background()
	store := newFakeRuntimes()
	s := New(Options{Runtimes: store})
	defer s.Close()
	_, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "old", Manifest: Source{Text: "dist:\n  otelcol_version: 0.159.0\n"}})
	wantDiagnostic(t, err, "unsupported_collector_version", "older than 0.160.0, the oldest release the debugger builds")

	b, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "verified", Manifest: Source{Text: minimalManifest + "  output_path: ./out\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Verified || b.CollectorVersion != "0.162.0" || len(b.Warnings) != 5 || b.Warnings[0] != "dist.output_path only steers OCB's own build and is ignored" ||
		b.Warnings[1] != "the manifest has no processor/transform, so the runtime lacks the ottl_replay feature and its reports say so" {
		t.Fatalf("build = %+v", b)
	}
	if v := waitBuild(t, s, b.ID); v.State != BuildDone || !strings.Contains(strings.Join(v.Log, "\n"), "warning: dist.output_path") {
		t.Fatalf("build = %+v", v)
	}
	if r := store.specs[0].Release; r == nil || r.Version != "0.162.0" || len(r.GoMod) == 0 {
		t.Fatalf("a verified build has the release's dependency set: %+v", r)
	}

	unverified := New(Options{Runtimes: store, Versions: versions.NewTable()})
	defer unverified.Close()
	b, err = unverified.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "newer", Manifest: Source{Text: minimalManifest}})
	if err != nil || b.Verified {
		t.Fatalf("build = %+v, %v", b, err)
	}
	waitBuild(t, unverified, b.ID)
	if store.specs[1].Release != nil {
		t.Fatal("an unverified build has no dependency set")
	}
}

func writeModule(t *testing.T, dir, module string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"go.mod": "module " + module + "\n", "factory.go": "package custom\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestBuildSourcesComeFromDirectoriesAndGit reads a manifest file's local
// directories relative to it and a supplied directory under the path
// policy, clones a git source at a ref when the build starts and records
// its commit; refused paths and relative directories of an inline manifest
// are diagnostics.
func TestBuildSourcesComeFromDirectoriesAndGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	writeModule(t, filepath.Join(root, "manifests", "local"), "example.com/local")
	writeModule(t, filepath.Join(root, "manifests", "replaced"), "example.com/replaced")
	writeModule(t, filepath.Join(root, "supplied"), "example.com/supplied")

	// A repository whose module is in a subdirectory, at a tag on a branch
	// that is not the default.
	work := filepath.Join(root, "work")
	writeModule(t, filepath.Join(work, "mod"), "example.com/fromgit")
	runGit(t, work, "init", "--quiet")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "first")
	runGit(t, work, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "mod", "second.go"), []byte("package custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "second")
	commit := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "checkout", "--quiet", "main")
	bare := filepath.Join(t.TempDir(), "repo.git")
	runGit(t, root, "clone", "--quiet", "--bare", work, bare)

	manifestPath := filepath.Join(root, "manifests", "manifest.yaml")
	manifestText := minimalManifest + `processors:
  - gomod: example.com/local v0.1.0
    path: ./local
  - gomod: example.com/supplied v0.1.0
  - gomod: example.com/fromgit v0.1.0
replaces:
  - example.com/replaced => ./replaced
`
	if err := os.WriteFile(manifestPath, []byte(manifestText), 0o644); err != nil {
		t.Fatal(err)
	}
	// The CLI's own builds read any host file, local repositories included.
	store := newFakeRuntimes()
	s := New(Options{Runtimes: store, Paths: PathPolicy{Unrestricted: true}})
	defer s.Close()
	b, err := s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "custom", Manifest: Source{Path: manifestPath}, Sources: []SourceOverride{
		{Module: "example.com/supplied", Dir: filepath.Join(root, "supplied")},
		{Module: "example.com/fromgit", Git: bare, Ref: "feature", Subdir: "mod"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := waitBuild(t, s, b.ID); v.State != BuildDone {
		t.Fatalf("build = %+v", v)
	}
	got := map[string]BuildSource{}
	for _, src := range store.specs[0].Sources {
		got[src.Module] = src
	}
	for module, origin := range map[string]string{
		"example.com/local":    filepath.Join(root, "manifests", "local"),
		"example.com/replaced": filepath.Join(root, "manifests", "replaced"),
		"example.com/supplied": filepath.Join(root, "supplied"),
		"example.com/fromgit":  bare + "@feature#mod",
	} {
		src, ok := got[module]
		if !ok || src.Origin != origin || src.Files == nil {
			t.Fatalf("source of %s = %+v, want origin %s", module, src, origin)
		}
		mod, err := fs.ReadFile(src.Files, "go.mod")
		if err != nil || string(mod) != "module "+module+"\n" {
			t.Fatalf("go.mod of %s = %q, %v", module, mod, err)
		}
	}
	if src := got["example.com/fromgit"]; src.Commit != commit {
		t.Fatalf("git commit %q, want %s", src.Commit, commit)
	} else if _, err := fs.ReadFile(src.Files, "second.go"); err != nil {
		t.Fatal("the git source is not at its ref")
	}

	_, err = s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "inline", Manifest: Source{Text: manifestText}, Sources: []SourceOverride{
		{Module: "example.com/supplied", Dir: t.TempDir()},
	}})
	wantDiagnostic(t, err, "manifest_sources", "the manifest's directory ./local for example.com/local is relative to a manifest file")
	wantDiagnostic(t, err, "manifest_sources", "the source directory for example.com/supplied")

	// An inline manifest's directories resolve against ManifestDir.
	b, err = s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "inline", ManifestDir: filepath.Join(root, "manifests"),
		Manifest: Source{Text: minimalManifest + "processors:\n  - gomod: example.com/local v0.1.0\n    path: ./local\n"}})
	if err != nil {
		t.Fatal(err)
	}
	waitBuild(t, s, b.ID)
	if src := store.specs[len(store.specs)-1].Sources; len(src) != 1 || src[0].Origin != filepath.Join(root, "manifests", "local") {
		t.Fatalf("sources = %+v", src)
	}

	_, err = s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "both", Manifest: Source{Text: minimalManifest}, Sources: []SourceOverride{
		{Module: "example.com/x", Dir: root, Git: bare},
	}})
	wantDiagnostic(t, err, "manifest_sources", "needs a directory or a git repository, not both")

	b, err = s.BuildRuntime(ctx, testCaller, BuildRuntimeRequest{Name: "badref", Manifest: Source{Text: minimalManifest}, Sources: []SourceOverride{
		{Module: "example.com/fromgit", Git: bare, Ref: "missing", Subdir: "mod"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if v := waitBuild(t, s, b.ID); v.State != BuildFailed || !strings.Contains(v.Error, "the repository has no ref missing") {
		t.Fatalf("build = %+v", v)
	}
}

// TestManifestsAreComposedFromTheCatalog writes a manifest from the
// components a configuration uses and refuses a release without a catalog.
func TestManifestsAreComposedFromTheCatalog(t *testing.T) {
	ctx := context.Background()
	s := New(Options{})
	defer s.Close()
	support, err := s.GetCollectorSupport(ctx, testCaller)
	if err != nil || support.Oldest != "0.160.0" || len(support.Verified) == 0 {
		t.Fatalf("support = %+v, %v", support, err)
	}
	cat, err := s.GetCollectorCatalog(ctx, testCaller, "")
	if err != nil || cat.CollectorVersion != support.Verified[len(support.Verified)-1] || len(cat.Components) == 0 {
		t.Fatalf("catalog = %v, %v", cat, err)
	}
	_, err = s.GetCollectorCatalog(ctx, testCaller, "0.150.0")
	wantCode(t, err, CodeNotFound)

	config := "receivers:\n  otlp:\nprocessors:\n  transform/a:\n  mystery:\nexporters:\n  debug:\nservice:\n  pipelines:\n    logs: {receivers: [otlp], processors: [transform/a], exporters: [debug]}\n"
	m, err := s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Configuration: &Source{Text: config}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"receiver/otlpreceiver", "processor/transformprocessor", "exporter/debugexporter"} {
		if !strings.Contains(m.Manifest, want) {
			t.Errorf("manifest lacks %s:\n%s", want, m.Manifest)
		}
	}
	if len(m.Unknown) != 1 || m.Unknown[0] != "processors/mystery" {
		t.Fatalf("unknown = %q", m.Unknown)
	}
	_, err = s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Preset: "everything"})
	wantCode(t, err, CodeInvalidRequest)
}

// TestComposedManifestsBuildTheSourcesComponents adds an entry for each
// component package of a directory or git source to a written manifest,
// and refuses a source without components or declaring another module.
func TestComposedManifestsBuildTheSourcesComponents(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stamp := filepath.Join(root, "stamp")
	if err := os.CopyFS(stamp, os.DirFS(filepath.Join("..", "..", "..", "testdata", "runtimes", "stampprocessor"))); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty")
	writeModule(t, empty, "example.com/empty")
	s := New(Options{Paths: hostfs.Policy{Roots: []string{root}}})
	defer s.Close()

	m, err := s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Components: []string{"receivers/otlp", "exporters/debug"},
		Sources: []SourceOverride{{Module: stampModule, Dir: stamp}}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "processors:\n  - gomod: " + stampModule + " v0.0.0\n"; !strings.Contains(m.Manifest, want) {
		t.Fatalf("manifest lacks %q:\n%s", want, m.Manifest)
	}
	if len(m.Custom) != 1 || m.Custom[0] != "processors/"+stampModule {
		t.Fatalf("custom = %q", m.Custom)
	}
	parsed, err := manifest.Parse([]byte(m.Manifest))
	if err != nil {
		t.Fatal(err)
	}
	// The build takes the source the manifest now names.
	if _, err := s.buildSources(parsed, "", []SourceOverride{{Module: stampModule, Dir: stamp}}); err != nil {
		t.Fatal(err)
	}

	_, err = s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Preset: "all", Sources: []SourceOverride{{Module: "example.com/empty", Dir: empty}}})
	wantDiagnostic(t, err, "manifest_sources", "has no component")
	_, err = s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Preset: "all", Sources: []SourceOverride{{Module: "example.com/other", Dir: stamp}}})
	wantDiagnostic(t, err, "manifest_sources", "declares module "+stampModule)
	_, err = s.ComposeManifest(ctx, testCaller, ComposeManifestRequest{Preset: "all", Sources: []SourceOverride{{Module: stampModule, Dir: t.TempDir()}}})
	wantDiagnostic(t, err, "manifest_sources", "the source of "+stampModule)

	if _, err := exec.LookPath("git"); err != nil {
		return
	}
	runGit(t, stamp, "init", "--quiet")
	runGit(t, stamp, "add", ".")
	runGit(t, stamp, "commit", "--quiet", "-m", "stamp")
	// A service confined to roots clones no local repository: that would
	// read the host's files outside its policy.
	fromGit := ComposeManifestRequest{Version: release.CollectorVersion, Preset: "built-in", Sources: []SourceOverride{{Module: stampModule, Git: stamp}}}
	_, err = s.ComposeManifest(ctx, testCaller, fromGit)
	wantDiagnostic(t, err, "manifest_sources", "not allowed")
	local := New(Options{Paths: hostfs.Policy{Unrestricted: true}})
	defer local.Close()
	m, err = local.ComposeManifest(ctx, testCaller, fromGit)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Custom) != 1 || !strings.Contains(m.Manifest, stampModule+" v0.0.0") {
		t.Fatalf("from git: %q\n%s", m.Custom, m.Manifest)
	}
}
