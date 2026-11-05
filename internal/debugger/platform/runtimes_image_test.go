package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/container/registrytest"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// exportedRuntime pushes a runtime image whose /worker is this test binary
// to a registry, as linea-builder exports a build, and returns its
// reference. The Docker client's config in DOCKER_CONFIG holds the
// registry's credentials, and the image is accepted on this host, which
// runs the binary as a worker (TestMain).
func exportedRuntime(t *testing.T, collectorVersion string) string {
	t.Helper()
	reg := registrytest.New(t, "team/runtimes", "ci", "pw")
	auth, _ := json.Marshal(map[string]any{"auths": map[string]any{reg.Host(): map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("ci:pw"))}}})
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfg)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{container.LabelRuntime: "edge", container.LabelProtocol: strconv.Itoa(worker.ProtocolVersion), container.LabelCollectorVersion: collectorVersion}
	e := &container.ImageExport{Auth: auth, PlainHTTP: []string{reg.Host()}, WorkDir: t.TempDir()}
	repo := reg.Host() + "/team/runtimes"
	if _, err := e.Push(context.Background(), repo, "edge", self, labels); err != nil {
		t.Fatal(err)
	}

	savedPlatform, savedCommand, savedEnv := imagePlatform, binaryCommand, binaryEnv
	imagePlatform = "linux/" + runtime.GOARCH
	binaryCommand = func(path string, args ...string) *exec.Cmd {
		if runtime.GOOS == "windows" {
			// Windows runs only a file with an executable extension.
			exe := path + ".exe"
			if _, err := os.Stat(exe); err != nil {
				b, _ := os.ReadFile(path)
				_ = os.WriteFile(exe, b, 0o755)
			}
			path = exe
		}
		return exec.Command(path, args...)
	}
	binaryEnv = func() []string { return append(savedEnv(), poolChild+"=serve") }
	t.Cleanup(func() { imagePlatform, binaryCommand, binaryEnv = savedPlatform, savedCommand, savedEnv })
	return repo + ":edge"
}

// TestADirectRunOnAnImageReferenceRunsItsBinary: a direct command that
// names a runtime image runs the image's binary as a child process, and the
// report names the image by digest. A second run uses the
// cached binary.
func TestADirectRunOnAnImageReferenceRunsItsBinary(t *testing.T) {
	own, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	ref := exportedRuntime(t, own.CollectorVersion)
	cache := t.TempDir()
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: DockerRuntimes(DockerOptions{ImageCache: cache})})
	t.Cleanup(s.Close)
	ctx := context.Background()
	quick := RunRequest{Mode: ModeQuick, Runtime: ref, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	for range 2 {
		rep, err := s.ExecuteRun(ctx, testCaller, quick)
		if err != nil {
			t.Fatal(err)
		}
		rt := rep.Runtime
		if rt == nil || rt.Kind != engine.RuntimeImage || !strings.HasPrefix(rt.Name, strings.TrimSuffix(ref, ":edge")+"@sha256:") ||
			rt.ImageID != strings.TrimPrefix(rt.Name, strings.TrimSuffix(ref, ":edge")+"@") || rt.CollectorVersion != own.CollectorVersion {
			t.Fatalf("report runtime = %+v", rt)
		}
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 1 {
		t.Fatalf("cache = %v", entries)
	}
	s.Close() // Stops the worker, whose binary a prune removes.
	if n, err := PruneImageCache(cache); n != 1 || err != nil {
		t.Fatalf("pruned %d, %v", n, err)
	}
}

// TestAnImageWhoseCollectorIsNotItsLabelIsRefused: the binary's hello must
// report the Collector release the image's labels name.
func TestAnImageWhoseCollectorIsNotItsLabelIsRefused(t *testing.T) {
	ref := exportedRuntime(t, "0.1.0")
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Runtimes: DockerRuntimes(DockerOptions{ImageCache: t.TempDir()})})
	t.Cleanup(s.Close)
	quick := RunRequest{Mode: ModeQuick, Runtime: ref, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	if _, err := s.ExecuteRun(context.Background(), testCaller, quick); err == nil || !strings.Contains(err.Error(), "0.1.0") {
		t.Fatalf("run = %v", err)
	}
}

// TestATeamServerRefusesAnImageReference: a team server runs the runtimes
// of its catalog, never an image a request names.
func TestATeamServerRefusesAnImageReference(t *testing.T) {
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken})
	t.Cleanup(s.Close)
	quick := RunRequest{Mode: ModeQuick, Runtime: "registry.example.com/team/runtimes:edge", Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}}
	if _, err := s.ExecuteRun(context.Background(), testCaller, quick); ErrorCode(err) != CodeInvalidRequest || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("run = %v (%s)", err, ErrorCode(err))
	}
}
