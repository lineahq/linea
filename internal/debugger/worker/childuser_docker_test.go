package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// poolBaseImage is the base of the linea-worker image
// (deploy/docker/linea-worker.Dockerfile).
const poolBaseImage = "gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"

// TestAPoolContainersChildCannotReachItsSecrets runs
// TestAChildUserCannotReadTheParentsSecrets in a container set up as the
// Compose file and the chart set up a pool: root with only CAP_SETUID and
// CAP_SETGID, no new privileges and a read-only root filesystem, on the
// worker image's base. It cross-compiles this package's tests for the
// Docker host's architecture; LINEA_DOCKER_TESTS=1 enables it.
func TestAPoolContainersChildCannotReachItsSecrets(t *testing.T) {
	if os.Getenv("LINEA_DOCKER_TESTS") != "1" {
		t.Skip("set LINEA_DOCKER_TESTS=1 to run the child-user test in a pool container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	build := exec.CommandContext(ctx, "go", "test", "-c", "-o", filepath.Join(dir, "worker.test"), ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the test binary: %v: %s", err, out)
	}
	run := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none",
		"--user", "0:0", "--cap-drop", "ALL", "--cap-add", "SETUID", "--cap-add", "SETGID",
		"--security-opt", "no-new-privileges", "--read-only", "--tmpfs", "/tmp:exec,mode=1777",
		"-v", dir+":/t:ro", "--entrypoint", "/t/worker.test", poolBaseImage,
		"-test.run", "^TestAChildUserCannotReadTheParentsSecrets$", "-test.v", "-test.count=1")
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("the test in the container failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestAChildUserCannotReadTheParentsSecrets") {
		t.Fatalf("the test in the container did not pass:\n%s", out)
	}
	t.Logf("%s", out)
}
