package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lineahq/linea/internal/debugger/manifest"
	"github.com/lineahq/linea/internal/debugger/release"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// fakeGoMarker starts the test binary as a stand-in for the go command: the
// native builder's seam runs it with the go arguments after the marker, and
// TestFakeGo plays the part the file "fake-go" in the build module names.
const fakeGoMarker = "linea-fake-go"

func fakeGoArgs() ([]string, bool) {
	for i, a := range os.Args {
		if a == fakeGoMarker {
			return os.Args[i+1:], true
		}
	}
	return nil, false
}

// TestFakeGo is the fake go command; it does nothing in a normal test run.
func TestFakeGo(t *testing.T) {
	args, ok := fakeGoArgs()
	if !ok {
		t.Skip("only runs as the native builder's stand-in for go")
	}
	mode, _ := os.ReadFile("fake-go")
	cwd, _ := os.Getwd()
	switch args[0] {
	case "env":
		fmt.Println(GoVersion)
	case "list":
		if args[1] == "-e" {
			fmt.Println(missingMarker + "example.com/missing/pkg")
			break
		}
		fmt.Println("packages that the resolve step must not log")
		if string(mode) == "fail" {
			fmt.Fprintln(os.Stderr, "go: boom")
			os.Exit(1)
		}
		if err := os.WriteFile("go.sum", []byte("resolved go.sum\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "run":
		fmt.Fprintln(os.Stderr, "gen ran")
	case "build":
		fmt.Fprintln(os.Stderr, "args "+strings.Join(args, " "))
		for _, kv := range os.Environ() {
			fmt.Fprintln(os.Stderr, "env "+kv)
		}
		sum, _ := os.ReadFile("go.sum")
		fmt.Fprintf(os.Stderr, "go.sum %q\n", strings.TrimSpace(string(sum)))
		info, err := os.Stat("go.mod")
		if err == nil {
			fmt.Fprintf(os.Stderr, "go.mod age %s\n", time.Since(info.ModTime()).Truncate(time.Hour))
		}
		fmt.Fprintln(os.Stderr, "dir "+filepath.Base(filepath.Dir(cwd)))
		switch string(mode) {
		case "fail":
			fmt.Fprintln(os.Stderr, "compile: boom")
			os.Exit(1)
		case "hang":
			fmt.Fprintln(os.Stderr, "hanging")
			time.Sleep(time.Minute)
		}
		out := args[slicesIndex(args, "-o")+1]
		if err := os.WriteFile(out, []byte("fake worker binary"), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func slicesIndex(args []string, s string) int {
	for i, a := range args {
		if a == s {
			return i
		}
	}
	return -1
}

// fakeNative is a Native whose go command is the test binary and whose
// worker says what the manifest names.
func fakeNative(t *testing.T, m *manifest.Manifest) *Native {
	t.Helper()
	if _, err := os.Stat(os.Args[0]); err != nil {
		t.Skip(err)
	}
	return &Native{
		WorkDir: t.TempDir(),
		command: func(ctx context.Context, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestFakeGo$", fakeGoMarker}, args...)...)
		},
		hello: func(ctx context.Context, name, binary string) (worker.Hello, error) {
			return worker.Hello{CollectorVersion: m.CollectorVersion(), Modules: m.Modules(), Verified: true}, nil
		},
	}
}

func fakeSource(t *testing.T, mode string) fstest.MapFS {
	t.Helper()
	src := repoSource(t)
	src["fake-go"] = &fstest.MapFile{Data: []byte(mode)}
	return src
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

func (l *logLines) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestGoVersionIsTheGoImagesRelease(t *testing.T) {
	tag, _, _ := strings.Cut(strings.TrimPrefix(GoImage, "golang:"), "@")
	version, _, _ := strings.Cut(tag, "-")
	if want := "go" + version; GoVersion != want {
		t.Fatalf("GoVersion = %q, GoImage %q is %q", GoVersion, GoImage, want)
	}
}

// TestBuildersShareTheGoCommands pins the commands both builders run: the
// Docker script's text is what it was before the native builder shared its
// arguments, so the Docker build's output did not change.
func TestBuildersShareTheGoCommands(t *testing.T) {
	for name, tc := range map[string]struct{ got, want string }{
		"resolve": {shellJoin(resolveArgs), "list -mod=mod -deps ./cmd/linea-worker"},
		"gates":   {shellJoin(genArgs), `run -mod=readonly ./internal/debugger/registry/gen -gates internal/debugger/registry/gateowners.go -source 'the runtime'\''s manifest'`},
		"compile": {shellJoin(compileArgs("/tmp/worker", true)), "build -mod=readonly -p 2 -trimpath -buildvcs=false '-ldflags=-s -w -X " + release.VersionVariable + "=" + release.ProductVersion + " -X github.com/lineahq/linea/internal/debugger/worker.custom=true' " +
			"-gcflags=github.com/DataDog/datadog-api-client-go/v2/api/datadogV2=-c=1 -o /tmp/worker ./cmd/linea-worker"},
		"compile unverified": {shellJoin(compileArgs("/tmp/worker", false)), "build -mod=readonly -p 2 -trimpath -buildvcs=false '-ldflags=-s -w -X " + release.VersionVariable + "=" + release.ProductVersion +
			" -X github.com/lineahq/linea/internal/debugger/worker.custom=true -X github.com/lineahq/linea/internal/debugger/worker.unverified=true' " +
			"-gcflags=github.com/DataDog/datadog-api-client-go/v2/api/datadogV2=-c=1 -o /tmp/worker ./cmd/linea-worker"},
		"missing": {shellJoin(missingArgs), "list -e -deps -f '{{if .Error}}linea-missing {{.ImportPath}}{{end}}' ./cmd/linea-worker"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s step: %s, want %s", name, tc.got, tc.want)
		}
	}
}

// TestNativeBuildRunsTheGoStepsInTheContainersEnvironment compiles with a
// stand-in go command: the build module is extracted fresh under the work
// directory, the environment is the container's with nothing else of the
// host's, the binary is hashed from its file, and the build directory is gone
// afterwards.
func TestNativeBuildRunsTheGoStepsInTheContainersEnvironment(t *testing.T) {
	t.Setenv("LINEA_SECRET", "must-not-leak")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOPROXY", "https://proxy.example.com")
	t.Setenv("NETRC", "/etc/linea/modules/netrc")
	t.Setenv("SSL_CERT_DIR", "/etc/linea/modules/ca")
	m := parse(t, builtInManifest(t))
	n := fakeNative(t, m)
	var log logLines
	rt, err := n.Build(context.Background(), BuildRequest{Name: "edge", Manifest: builtInManifest(t), Source: fakeSource(t, "ok"), Set: set162(t), Log: log.add})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.text())
	}
	sum := sha256.Sum256([]byte("fake worker binary"))
	if want := "sha256:" + hex.EncodeToString(sum[:]); rt.Digest != want || rt.Size != int64(len("fake worker binary")) {
		t.Fatalf("digest %s size %d, want %s", rt.Digest, rt.Size, want)
	}
	if rt.Hello.CollectorVersion != "0.162.0" || rt.Name != "edge" {
		t.Fatalf("runtime %+v", rt)
	}
	text := log.text()
	for _, want := range []string{
		"args build -mod=readonly -p 2 -trimpath -buildvcs=false -ldflags=-s -w -X " + release.VersionVariable + "=" + release.ProductVersion + " -X github.com/lineahq/linea/internal/debugger/worker.custom=true -gcflags=github.com/DataDog/datadog-api-client-go/v2/api/datadogV2=-c=1 -o " + rt.Path + " ./cmd/linea-worker",
		"gen ran",
		"env CGO_ENABLED=0", "env GOOS=linux", "env GOARCH=" + runtime.GOARCH, "env GOTOOLCHAIN=local", "env GOGC=25", "env GOENV=off",
		"env GOPROXY=https://proxy.example.com",
		"env NETRC=/etc/linea/modules/netrc", "env SSL_CERT_DIR=/etc/linea/modules/ca",
		"env GOMODCACHE=" + filepath.Join(n.WorkDir, "gomod"), "env GOCACHE=" + filepath.Join(n.WorkDir, "gocache"),
		"go.mod age 0s", "dir build-",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %q:\n%s", want, text)
		}
	}
	for _, leak := range []string{"LINEA_SECRET", "GOFLAGS", "must-not-leak"} {
		if strings.Contains(text, leak) {
			t.Errorf("the host's %s reached the go command:\n%s", leak, text)
		}
	}
	if strings.Contains(text, "must not log") || strings.Contains(text, "resolving modules") {
		t.Errorf("a verified build resolved, or the resolve output was logged:\n%s", text)
	}
	if got := entries(t, filepath.Join(n.WorkDir, "builds")); len(got) != 0 {
		t.Errorf("build directories left behind: %v", got)
	}
	if got := entries(t, filepath.Join(n.WorkDir, "bin")); len(got) != 1 || filepath.Join(n.WorkDir, "bin", got[0]) != rt.Path {
		t.Errorf("binaries %v, want only %s", got, rt.Path)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(rt.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Close left the binary: %v", err)
	}
}

// TestNativeBuildOfAnUnverifiedReleaseResolvesFirst resolves the modules,
// discards the resolve step's package list, and compiles the resolved
// go.sum.
func TestNativeBuildOfAnUnverifiedReleaseResolvesFirst(t *testing.T) {
	raw := strings.NewReplacer("0.162.0", "0.163.0", "v1.68.0", "v1.69.0").Replace(string(builtInManifest(t)))
	n := fakeNative(t, parse(t, []byte(raw)))
	var log logLines
	rt, err := n.Build(context.Background(), BuildRequest{Name: "edge", Manifest: []byte(raw), Source: fakeSource(t, "ok"), Log: log.add})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.text())
	}
	defer rt.Close()
	text := log.text()
	if !strings.Contains(text, "building an unverified runtime") || !strings.Contains(text, "resolving modules") ||
		!strings.Contains(text, `go.sum "resolved go.sum"`) || strings.Contains(text, "must not log") {
		t.Fatalf("log:\n%s", text)
	}
}

// TestNativeBuildReportsAFailedStepAndItsMissingModules keeps Docker's
// BuildError: the stage, the output and the packages go could not load.
func TestNativeBuildReportsAFailedStepAndItsMissingModules(t *testing.T) {
	m := parse(t, builtInManifest(t))
	n := fakeNative(t, m)
	_, err := n.Build(context.Background(), BuildRequest{Name: "edge", Manifest: builtInManifest(t), Source: fakeSource(t, "fail"), Set: set162(t)})
	var be *BuildError
	if !errors.As(err, &be) || be.Stage != "compiling" || !strings.Contains(be.Output, "compile: boom") {
		t.Fatalf("error %v", err)
	}
	if len(be.Missing) != 1 || be.Missing[0] != "example.com/missing/pkg" {
		t.Fatalf("missing %v", be.Missing)
	}
	if got := entries(t, filepath.Join(n.WorkDir, "builds")); len(got) != 0 {
		t.Errorf("build directories left behind: %v", got)
	}
	if got := entries(t, filepath.Join(n.WorkDir, "bin")); len(got) != 0 {
		t.Errorf("a failed build left a binary: %v", got)
	}
}

// TestNativeBuildStopsWhenItsContextEnds kills the go command of a build
// that is cancelled and reports the cause.
func TestNativeBuildStopsWhenItsContextEnds(t *testing.T) {
	m := parse(t, builtInManifest(t))
	n := fakeNative(t, m)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	errCancelled := errors.New("cancelled by the test")
	done := make(chan error, 1)
	go func() {
		_, err := n.Build(ctx, BuildRequest{Name: "edge", Manifest: builtInManifest(t), Source: fakeSource(t, "hang"), Set: set162(t), Log: func(line string) {
			if line == "hanging" {
				cancel(errCancelled)
			}
		}})
		done <- err
	}()
	select {
	case err := <-done:
		var be *BuildError
		if !errors.As(err, &be) || be.Stage != "compiling" || !errors.Is(err, errCancelled) {
			t.Fatalf("error %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the cancelled build did not stop")
	}
	if got := entries(t, filepath.Join(n.WorkDir, "builds")); len(got) != 0 {
		t.Errorf("build directories left behind: %v", got)
	}
}

// TestNativeBuildChecksTheWorkersHello refuses a worker that announces
// another Collector or other modules than the manifest names, and a worker
// that does not start, removing the binary.
func TestNativeBuildChecksTheWorkersHello(t *testing.T) {
	m := parse(t, builtInManifest(t))
	for name, hello := range map[string]func(context.Context, string, string) (worker.Hello, error){
		"another Collector": func(context.Context, string, string) (worker.Hello, error) {
			return worker.Hello{CollectorVersion: "0.161.0", Modules: m.Modules()}, nil
		},
		"other modules": func(context.Context, string, string) (worker.Hello, error) {
			return worker.Hello{CollectorVersion: m.CollectorVersion(), Modules: m.Modules()[1:]}, nil
		},
		"no start": func(context.Context, string, string) (worker.Hello, error) {
			return worker.Hello{}, errors.New("panic: boom")
		},
	} {
		n := fakeNative(t, m)
		n.hello = hello
		_, err := n.Build(context.Background(), BuildRequest{Name: "edge", Manifest: builtInManifest(t), Source: fakeSource(t, "ok"), Set: set162(t)})
		var be *BuildError
		if !errors.As(err, &be) || be.Stage != "starting the worker" {
			t.Errorf("%s: error %v", name, err)
		}
		if got := entries(t, filepath.Join(n.WorkDir, "bin")); len(got) != 0 {
			t.Errorf("%s: left a binary: %v", name, got)
		}
	}
}

func TestNativeBuildNeedsAWorkDirAndAValidName(t *testing.T) {
	if _, err := (&Native{}).Build(context.Background(), BuildRequest{Name: "edge", Manifest: builtInManifest(t), Source: repoSource(t), Set: set162(t)}); err == nil || !strings.Contains(err.Error(), "work directory") {
		t.Errorf("no work directory: %v", err)
	}
	if _, err := (&Native{WorkDir: t.TempDir()}).Build(context.Background(), BuildRequest{Name: "Edge", Manifest: builtInManifest(t), Source: repoSource(t), Set: set162(t)}); err == nil || !strings.Contains(err.Error(), "runtime name") {
		t.Errorf("bad name: %v", err)
	}
}

func TestNativeToolchainReportsTheHostsGo(t *testing.T) {
	n := fakeNative(t, parse(t, builtInManifest(t)))
	got, err := n.Toolchain(context.Background())
	if err != nil || got != GoVersion {
		t.Fatalf("Toolchain = %q, %v", got, err)
	}
}

// TestExtractModuleRefusesUnsafeEntries unpacks only relative regular files
// and directories.
func TestExtractModuleRefusesUnsafeEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../escape.go", "a\\b.go", "/abs.go"} {
		archive := tarGz(t, map[string]string{name: "x"})
		if err := extractModule(archive, dir); err == nil {
			t.Errorf("%q was extracted", name)
		}
	}
	if err := extractModule(tarGz(t, map[string]string{"a/b/c.go": "package c\n"}), dir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "a", "b", "c.go")); err != nil || string(data) != "package c\n" {
		t.Fatalf("%q, %v", data, err)
	}
}
