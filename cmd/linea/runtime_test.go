package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

const sampleConfig = "../../samples/logs-cleanup/config.yaml"
const sampleScenario = "../../samples/logs-cleanup/scenario.yaml"

// TestRuntimeFlagReachesTheRequest runs on a runtime the machine does not
// have: the platform refuses it by name (or for lack of Docker), which only
// happens when --runtime reached the request.
func TestRuntimeFlagReachesTheRequest(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--config", sampleConfig, "--scenario", sampleScenario, "--runtime", "missing-rt"},
		{"inspect", "--config", sampleConfig, "--runtime", "missing-rt"},
		{"test", "--config", sampleConfig, "--scenario", sampleScenario, "--runtime", "missing-rt"},
		{"diff", "--before", sampleConfig, "--after", sampleConfig, "--scenario", sampleScenario, "--after-runtime", "missing-rt"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitError {
			t.Errorf("%v: exit %d, want %d; stdout %s stderr %s", args, code, exitError, stdout.String(), stderr.String())
			continue
		}
		if msg := strings.ToLower(stderr.String()); !strings.Contains(msg, "missing-rt") && !strings.Contains(msg, "docker") {
			t.Errorf("%v: stderr %q names neither the runtime nor Docker", args, stderr.String())
		}
	}
}

func TestRuntimeCommandRefusesBadUsage(t *testing.T) {
	for _, args := range [][]string{
		{"runtime"},
		{"runtime", "bogus"},
		{"runtime", "build", "--name", "x"},
		{"runtime", "rm"},
		{"runtime", "push", "only-name"},
		{"runtime", "import"},
		{"runtime", "list", "--server", "http://example.com:7070"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != exitError {
			t.Errorf("%v: exit %d, want %d (stderr %s)", args, code, exitError, stderr.String())
		}
	}
}

func TestRuntimeTableShortensImageIDs(t *testing.T) {
	var out bytes.Buffer
	err := writeRuntimes(&out, []api.Runtime{{Name: "v0162", CollectorVersion: "0.162.0", Verified: true, ImageID: "sha256:0123456789abcdef0123"},
		{Name: "next", CollectorVersion: "0.162.0", Verified: true}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || strings.Fields(lines[0])[0] != "NAME" || strings.Join(strings.Fields(lines[1]), " ") != "v0162 0.162.0 true 0123456789ab" ||
		strings.Join(strings.Fields(lines[2]), " ") != "next 0.162.0 true -" {
		t.Fatalf("table:\n%s", out.String())
	}
}

// TestStorageAndPruneSayWhatCanGo prints each image's state and size, the
// caches and what a prune removed and kept with its reason.
func TestStorageAndPruneSayWhatCanGo(t *testing.T) {
	var out bytes.Buffer
	err := writeStorage(&out, &api.RuntimeStorage{
		Runtimes: []api.StoredRuntime{
			{Runtime: api.Runtime{Name: "v0162", CollectorVersion: "0.162.0", ImageID: "sha256:0123456789abcdef", Size: 69_400_000}, State: "replaced"},
			{Runtime: api.Runtime{Name: "v0162", CollectorVersion: "0.162.0", ImageID: "sha256:fedcba9876543210", Size: 70_000_000}, State: "named", InUse: true},
		},
		Caches:      []api.CacheVolume{{Name: "linea-gomod", Size: 5_200_000_000}, {Name: "linea-gocache", Size: -1}},
		Reclaimable: 69_400_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"replaced 69.4MB 0123456789ab", "named (in use) 70.0MB", "linea-gomod 5.2GB", "linea-gocache unknown", "runtime prune: 69.4MB"} {
		if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), want) {
			t.Fatalf("storage lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	err = writePrune(&out, &api.RuntimePruneResult{
		Removed:   []api.PrunedItem{{Kind: "runtime", Name: "v0162", ImageID: "sha256:0123456789abcdef", Size: 69_400_000}, {Kind: "container", Name: "linea-build-x"}},
		Kept:      []api.PrunedItem{{Kind: "cache", Name: "linea-gomod", Reason: "volume is in use"}},
		Reclaimed: 69_400_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "removed runtime v0162 (image 0123456789ab)\nremoved container linea-build-x\nkept cache linea-gomod: volume is in use\nreclaimed 69.4MB\n"
	if out.String() != want {
		t.Fatalf("prune:\n%s\nwant:\n%s", out.String(), want)
	}
}

// fakeBuilds is a build reader whose log grows by one line per read.
type fakeBuilds struct {
	mu        sync.Mutex
	lines     []string
	reads     int
	cancelled bool
	failWith  string
	runtime   *platform.RuntimeInfo
}

func (f *fakeBuilds) GetBuild(_ context.Context, _ string, from int) (*platform.RuntimeBuild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	shown := min(f.reads, len(f.lines))
	b := &platform.RuntimeBuild{ID: "b1", Name: "rt", State: platform.BuildRunning, LogFrom: from, Log: []string{}}
	b.Log = append(b.Log, f.lines[min(from, shown):shown]...)
	switch {
	case f.cancelled:
		b.State = platform.BuildCancelled
	case f.reads > len(f.lines) && f.failWith != "":
		b.State, b.Error = platform.BuildFailed, f.failWith
	case f.reads > len(f.lines):
		b.State, b.Runtime = platform.BuildDone, f.runtime
	}
	return b, nil
}

func (f *fakeBuilds) CancelBuild(context.Context, string) error {
	f.mu.Lock()
	f.cancelled = true
	f.mu.Unlock()
	return nil
}

func TestFollowBuildPrintsLogLinesOnce(t *testing.T) {
	old := buildPollInterval
	buildPollInterval = time.Millisecond
	t.Cleanup(func() { buildPollInterval = old })

	f := &fakeBuilds{lines: []string{"step 1", "step 2", "step 3"}, runtime: &platform.RuntimeInfo{Name: "rt", ImageID: "sha256:abcdef0123456789", CollectorVersion: "0.162.0", Verified: true,
		CustomModules: []platform.CustomModule{{Module: "example.com/stamp", Source: "repo@v1", Commit: "abc123"}}}}
	var stdout, stderr bytes.Buffer
	if code := followBuild(context.Background(), f, &platform.RuntimeBuild{ID: "b1"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	want := "step 1\nstep 2\nstep 3\nbuilt runtime rt (image abcdef012345, Collector 0.162.0)\n  custom module example.com/stamp from repo@v1 at commit abc123\n"
	if stdout.String() != want {
		t.Fatalf("stdout %q, want %q", stdout.String(), want)
	}

	f = &fakeBuilds{lines: []string{"boom"}, failWith: "compile failed"}
	stdout.Reset()
	stderr.Reset()
	if code := followBuild(context.Background(), f, &platform.RuntimeBuild{ID: "b1"}, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "compile failed") {
		t.Fatalf("failed build: exit %d, stderr %s", code, stderr.String())
	}
}

func TestFollowBuildCancelsOnInterrupt(t *testing.T) {
	old := buildPollInterval
	buildPollInterval = time.Millisecond
	t.Cleanup(func() { buildPollInterval = old })

	f := &fakeBuilds{lines: make([]string, 1_000_000)}
	ctx, interrupt := context.WithCancel(context.Background())
	interrupt()
	var stdout, stderr bytes.Buffer
	if code := followBuild(ctx, f, &platform.RuntimeBuild{ID: "b1"}, &stdout, &stderr); code != exitError {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if !f.cancelled || !strings.Contains(stderr.String(), "cancelled") {
		t.Fatalf("cancelled %v, stderr %s", f.cancelled, stderr.String())
	}
}

// TestSourceFlagsBecomeRequestSources makes directories absolute and splits
// a git source into its repository, ref and subdirectory.
func TestSourceFlagsBecomeRequestSources(t *testing.T) {
	var sources sourceFlag
	git := gitFlag{&sources}
	for _, v := range []string{"example.com/a=git@github.com:org/repo.git", "example.com/b=https://h/r.git#feature/x:mods/b", `example.com/c=C:epos\c#:sub`} {
		if err := git.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := sources.Set("example.com/d=rel/dir"); err != nil {
		t.Fatal(err)
	}
	want := []api.RuntimeSource{
		{Module: "example.com/a", Git: "git@github.com:org/repo.git"},
		{Module: "example.com/b", Git: "https://h/r.git", Ref: "feature/x", Subdir: "mods/b"},
		{Module: "example.com/c", Git: `C:epos\c`, Subdir: "sub"},
	}
	for i, w := range want {
		if sources[i] != w {
			t.Errorf("source %d = %+v, want %+v", i, sources[i], w)
		}
	}
	if d := sources[3]; d.Module != "example.com/d" || !filepath.IsAbs(d.Dir) {
		t.Errorf("directory source = %+v", d)
	}
	for _, bad := range []string{"example.com/a", "=dir", "example.com/a="} {
		if sources.Set(bad) == nil || git.Set(bad) == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// TestRuntimeManifestWritesACatalogManifest lists the support table and
// writes a manifest from a configuration into a new file, never over one,
// and adds a custom source's components.
func TestRuntimeManifestWritesACatalogManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"runtime", "versions"}, &stdout, &stderr); code != exitOK || !strings.Contains(stdout.String(), "oldest release: 0.160.0") {
		t.Fatalf("versions: exit %d, stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	out := filepath.Join(t.TempDir(), "manifest.yaml")
	stdout.Reset()
	stderr.Reset()
	args := []string{"runtime", "manifest", "--config", sampleConfig, "--component", "receivers/mystery", "--out", out}
	if code := run(context.Background(), args, &stdout, &stderr); code != exitOK || !strings.Contains(stderr.String(), "receivers/mystery is not in the catalog") {
		t.Fatalf("manifest: exit %d, stderr %s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), "otelcol_version:") {
		t.Fatalf("manifest file = %q, %v", data, err)
	}
	stderr.Reset()
	if code := run(context.Background(), args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "never overwrite") {
		t.Fatalf("second write: exit %d, stderr %s", code, stderr.String())
	}

	// A custom source's components are added.
	stdout.Reset()
	stderr.Reset()
	stamp := filepath.Join("..", "..", "testdata", "runtimes", "stampprocessor")
	args = []string{"runtime", "manifest", "--component", "receivers/otlp", "--source", "example.com/linea/stampprocessor=" + stamp}
	if code := run(context.Background(), args, &stdout, &stderr); code != exitOK ||
		!strings.Contains(stdout.String(), "gomod: example.com/linea/stampprocessor v0.0.0") ||
		!strings.Contains(stderr.String(), "added processors/example.com/linea/stampprocessor from its source") {
		t.Fatalf("with a source: exit %d, stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
}

// TestPruneBinariesEmptiesTheRuntimeCache: runtime prune --binaries
// empties the cache of runtime images that direct commands keep, and takes
// none of the flags that prune Docker or a server.
func TestPruneBinariesEmptiesTheRuntimeCache(t *testing.T) {
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, strings.Repeat("a", 64)), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "runtime", "prune", "--binaries", "--runtime-cache", cache)
	if code != exitOK || out != "removed 1 runtime images from "+cache+"\n" {
		t.Fatalf("prune = %d, %q, %q", code, out, errOut)
	}
	if code, out, _ := runCLI(t, "runtime", "prune", "--binaries", "--runtime-cache", filepath.Join(cache, "none"), "--format", "json"); code != exitOK || !strings.Contains(out, `"removed": 0`) {
		t.Fatalf("prune of no cache = %d, %q", code, out)
	}
	for _, flag := range []string{"--server=http://127.0.0.1:1", "--stale", "--caches"} {
		if code, _, _ := runCLI(t, "runtime", "prune", "--binaries", flag); code != exitError {
			t.Errorf("--binaries %s = %d", flag, code)
		}
	}
}

// TestAFinishedBuildNamesItsExportedImage: a done build prints the image
// the server exported by digest, and a failed export only warns.
func TestAFinishedBuildNamesItsExportedImage(t *testing.T) {
	const image = "registry.example.com/team/runtimes@sha256:0123"
	for _, tc := range []struct {
		build       platform.RuntimeBuild
		out, errOut string
	}{
		{platform.RuntimeBuild{Name: "edge", State: platform.BuildDone, CollectorVersion: "0.162.0", Verified: true, Image: image},
			"built runtime edge (Collector 0.162.0)\nexported image " + image + "\n", ""},
		{platform.RuntimeBuild{Name: "edge", State: platform.BuildDone, CollectorVersion: "0.162.0", Verified: true, ExportError: "denied"},
			"built runtime edge (Collector 0.162.0)\n", "warning: the runtime is built, but exporting its image failed: denied\n"},
	} {
		var stdout, stderr bytes.Buffer
		if code := finishBuild(&tc.build, &stdout, &stderr); code != exitOK || stdout.String() != tc.out || stderr.String() != tc.errOut {
			t.Errorf("finishBuild = %d, %q, %q", code, stdout.String(), stderr.String())
		}
	}
}
