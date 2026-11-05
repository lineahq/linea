package platform

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// The Docker tests here build small runtimes (the otlp receiver, the debug
// exporter and the custom stamp processor in testdata/runtimes/) and need
// LINEA_DOCKER_TESTS=1, like the container package's.

const stampModule = "example.com/linea/stampprocessor"

const stampManifest = `dist:
  otelcol_version: 0.162.0
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
processors:
  - gomod: ` + stampModule + ` v0.1.0
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0
`

const stampConfig = `receivers:
  otlp:
    protocols:
      grpc:
processors:
  stamp:
    value: from-the-test
exporters:
  debug:
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [stamp]
      exporters: [debug]
`

func dockerService(t *testing.T, table *versions.Table) *Service {
	t.Helper()
	if os.Getenv("LINEA_DOCKER_TESTS") != "1" {
		t.Skip("set LINEA_DOCKER_TESTS=1 to build runtimes in Docker")
	}
	raw, err := workersrc.Pack(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	src, err := workersrc.Unpack(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Kept small for developer machines.
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Versions: table,
		Runtimes: DockerRuntimes(DockerOptions{Source: src, BuildMemory: 3 << 30, BuildCPUs: 2})})
	t.Cleanup(s.Close)
	return s
}

// buildAndWait builds a runtime and waits for its end; a built runtime is
// removed when the test ends.
func buildAndWait(t *testing.T, s *Service, req BuildRuntimeRequest) *RuntimeBuild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	b, err := s.BuildRuntime(ctx, testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	if b, err = s.WaitRuntimeBuild(ctx, testCaller, b.ID, 0); err != nil {
		t.Fatal(err)
	}
	if b.State == BuildDone {
		t.Cleanup(func() { _ = s.DeleteRuntime(context.Background(), testCaller, req.Name) })
	} else {
		t.Logf("build log:\n%s", strings.Join(b.Log, "\n"))
	}
	return b
}

// copyStamp copies the stamp processor into dir, with go.mod's extra
// requirements.
func copyStamp(t *testing.T, dir string, require ...string) {
	t.Helper()
	from := filepath.Join("..", "..", "..", "testdata", "runtimes", "stampprocessor")
	if err := os.CopyFS(dir, os.DirFS(from)); err != nil {
		t.Fatal(err)
	}
	if len(require) == 0 {
		return
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod = append(mod, []byte("\nrequire (\n\t"+strings.Join(require, "\n\t")+"\n)\n")...)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0o644); err != nil {
		t.Fatal(err)
	}
}

// runStamped runs a log input on a runtime and returns the stamp stage's
// JSON and the report's runtime.
func runStamped(t *testing.T, s *Service, runtime string) (string, *Report) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: stampConfig},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range rep.Stages {
		if st.Type == "stamp" {
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw), rep
		}
	}
	t.Fatalf("no stamp stage: status %s %s", rep.Result.Status, rep.Result.StatusReason)
	return "", nil
}

// TestCustomProcessorsBuildFromDirectoriesAndGit builds the stamp processor
// from a directory and from a tag of a git repository: both runtimes run it
// with stage evidence, and the git runtime records the tag's commit in its
// reports.
func TestCustomProcessorsBuildFromDirectoriesAndGit(t *testing.T) {
	s := dockerService(t, nil)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	copyStamp(t, dir)
	b := buildAndWait(t, s, BuildRuntimeRequest{Name: "v6-stamp-dir", Manifest: Source{Text: stampManifest},
		Sources: []SourceOverride{{Module: stampModule, Dir: dir}}})
	if b.State != BuildDone || !b.Verified || len(b.Runtime.CustomModules) != 1 || b.Runtime.CustomModules[0].Source != dir {
		t.Fatalf("directory build = %+v (runtime %+v)", b.Error, b.Runtime)
	}
	stage, rep := runStamped(t, s, "v6-stamp-dir")
	if !strings.Contains(stage, "from-the-test") || rep.Runtime == nil || !rep.Runtime.Verified || len(rep.Runtime.CustomModules) != 1 {
		t.Fatalf("stage %s, runtime %+v", stage, rep.Runtime)
	}
	// The module's stampdebug provider is compiled in, named by
	// the runtime and its reports, and drives the run.
	if len(b.Runtime.Providers) != 1 || b.Runtime.Providers[0].Name != "stamp" || b.Runtime.Providers[0].Package != stampModule+"/stampdebug" {
		t.Fatalf("runtime providers %+v", b.Runtime.Providers)
	}
	if ps := rep.Runtime.Providers; len(ps) != 1 || ps[0].Name != "stamp" || !slices.Equal(ps[0].Components, []string{"processor/stamp"}) {
		t.Fatalf("report providers %+v", ps)
	}
	var declared []string
	for _, d := range rep.Result.Mocks {
		if d.Provider == "stamp" {
			declared = append(declared, d.Supplied...)
		}
	}
	if !slices.Equal(declared, []string{"value from-the-test, 1 inputs, note "}) {
		t.Fatalf("stamp provider declared %q in %+v", declared, rep.Result.Mocks)
	}

	work := filepath.Join(t.TempDir(), "work")
	copyStamp(t, filepath.Join(work, "processor"))
	runGit(t, work, "init", "--quiet")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "--quiet", "-m", "stamp")
	runGit(t, work, "tag", "v1")
	commit := runGit(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "stamp.git")
	runGit(t, work, "clone", "--quiet", "--bare", work, bare)
	b = buildAndWait(t, s, BuildRuntimeRequest{Name: "v6-stamp-git", Manifest: Source{Text: stampManifest},
		Sources: []SourceOverride{{Module: stampModule, Git: bare, Ref: "v1", Subdir: "processor"}}})
	if b.State != BuildDone {
		t.Fatalf("git build failed: %s", b.Error)
	}
	want := CustomModule{Module: stampModule, Source: bare + "@v1#processor", Commit: commit}
	if got := b.Runtime.CustomModules; len(got) != 1 || got[0] != want {
		t.Fatalf("custom modules %+v, want %+v", got, want)
	}
	stage, rep = runStamped(t, s, "v6-stamp-git")
	if !strings.Contains(stage, "from-the-test") || rep.Runtime == nil || len(rep.Runtime.CustomModules) != 1 || rep.Runtime.CustomModules[0] != want {
		t.Fatalf("stage %s, runtime %+v", stage, rep.Runtime)
	}
}

// fakeModuleProxy serves one module version from memory and 404 for every
// other path, so the go command falls back to the next proxy in GOPROXY.
func fakeModuleProxy(t *testing.T, module, version string, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(module + "@" + version + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	prefix := "/" + module + "/@v/" + version
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case prefix + ".info":
			w.Write([]byte(`{"Version":"` + version + `","Time":"2026-01-01T00:00:00Z"}`))
		case prefix + ".mod":
			w.Write([]byte(files["go.mod"]))
		case prefix + ".zip":
			w.Write(buf.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Docker Desktop forwards host.docker.internal to the host's loopback.
	return "http://host.docker.internal:" + u.Port()
}

// TestCustomModulesStayOnTheRelease refuses a custom module whose
// requirements move a Collector module off the release, naming it, and
// fails an offline build missing a module, naming the module.
func TestCustomModulesStayOnTheRelease(t *testing.T) {
	s := dockerService(t, nil)
	const featuregate = "go.opentelemetry.io/collector/featuregate"
	proxy := fakeModuleProxy(t, featuregate, "v1.99.0", map[string]string{
		"go.mod":         "module " + featuregate + "\n\ngo 1.24\n",
		"featuregate.go": "package featuregate\n",
	})
	t.Setenv("GONOSUMDB", featuregate)
	moved := t.TempDir()
	copyStamp(t, moved, featuregate+" v1.99.0")
	// The source's go.mod is checked when the build is requested, before
	// anything resolves, so the request itself is refused.
	_, err := s.BuildRuntime(context.Background(), testCaller, BuildRuntimeRequest{Name: "v6-moved", Manifest: Source{Text: stampManifest},
		Sources: []SourceOverride{{Module: stampModule, Dir: moved}}, ModuleProxy: proxy + ",https://proxy.golang.org"})
	if err == nil || !strings.Contains(err.Error(), "requires "+featuregate+" v1.99.0; Collector 0.162.0 has v1.68.0") {
		t.Fatalf("moved core module: %v", err)
	}

	const absent = "example.com/linea/absent"
	missing := t.TempDir()
	copyStamp(t, missing, absent+" v1.0.0")
	// A requirement nothing imports is never loaded (graph pruning).
	if err := os.WriteFile(filepath.Join(missing, "absent.go"), []byte("package stampprocessor\n\nimport _ \""+absent+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := buildAndWait(t, s, BuildRuntimeRequest{Name: "v6-offline", Manifest: Source{Text: stampManifest},
		Sources: []SourceOverride{{Module: stampModule, Dir: missing}}, Offline: true})
	if b.State != BuildFailed || !strings.Contains(b.Error, "GOPROXY=off") || !strings.Contains(b.Error, absent) {
		t.Fatalf("offline build missing a module: state %s, error %s", b.State, b.Error)
	}
}

// TestNewerReleasesBuildUnverified builds with a support table that does
// not know the manifest's release: the build resolves its modules through
// the given module proxy (here the host's module cache served locally),
// and the runtime and every report it produces are unverified.
func TestNewerReleasesBuildUnverified(t *testing.T) {
	s := dockerService(t, versions.NewTable())
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(strings.TrimSpace(string(out)), "cache", "download")
	if _, err := fs.Stat(os.DirFS(download), "."); err != nil {
		t.Skipf("no module cache: %v", err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(download)))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	// The served cache has no checksum database.
	t.Setenv("GOSUMDB", "off")
	dir := t.TempDir()
	copyStamp(t, dir)
	b := buildAndWait(t, s, BuildRuntimeRequest{Name: "v6-unverified", Manifest: Source{Text: stampManifest},
		Sources: []SourceOverride{{Module: stampModule, Dir: dir}}, ModuleProxy: "http://host.docker.internal:" + u.Port()})
	if b.State != BuildDone || b.Verified || b.Runtime.Verified {
		t.Fatalf("unverified build = %s %s (runtime %+v)", b.State, b.Error, b.Runtime)
	}
	stage, rep := runStamped(t, s, "v6-unverified")
	if !strings.Contains(stage, "from-the-test") || rep.Runtime == nil || rep.Runtime.Verified {
		t.Fatalf("stage %s, runtime %+v", stage, rep.Runtime)
	}
}
