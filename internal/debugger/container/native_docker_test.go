package container

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// nativeTests enables the tests that build runtimes with the host's Go
// toolchain; like the Docker tests they download modules and compile the
// worker.
const nativeTests = "LINEA_NATIVE_TESTS"

// smallManifest is a runtime of two components: it compiles the worker and
// the Collector core, without the hundreds of packages of the built-in
// manifest.
const smallManifest = `dist:
  name: linea-small
  otelcol_version: 0.162.0
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0
`

// TestANativeBuildIsTheBinaryTheDockerBuildMakes builds a small runtime with
// the host's go command (a linux host with the Go release of GoImage,
// LINEA_NATIVE_TESTS=1) and, when Docker is available too
// (LINEA_DOCKER_TESTS=1), in the Go image, and checks that the binaries are
// identical. Without Docker it logs the native digest, which the same test
// run elsewhere can be compared with. Both runs use cold caches unless
// LINEA_NATIVE_WORKDIR names a directory to keep them in.
func TestANativeBuildIsTheBinaryTheDockerBuildMakes(t *testing.T) {
	if os.Getenv(nativeTests) != "1" {
		t.Skipf("set %s=1 to build a runtime with this host's Go toolchain", nativeTests)
	}
	if runtime.GOOS != "linux" {
		t.Skip("the native builder builds and starts linux binaries")
	}
	work := os.Getenv("LINEA_NATIVE_WORKDIR")
	if work == "" {
		work = t.TempDir()
	}
	n := &Native{WorkDir: work}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	if v, err := n.Toolchain(ctx); err != nil || v != GoVersion {
		t.Skipf("the go command is %q (%v), the builds need %s", v, err, GoVersion)
	}
	src := packedSource(t)
	req := BuildRequest{Name: "test-small", Manifest: []byte(smallManifest), Source: src, Set: set162(t), Log: func(line string) { t.Log(line) }}
	rt, err := n.Build(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	t.Logf("native binary %s, %d bytes", rt.Digest, rt.Size)
	if rt.Hello.CollectorVersion != "0.162.0" || rt.Hello.Runtime == "" {
		t.Fatalf("hello %+v", rt.Hello)
	}

	if os.Getenv(dockerTests) != "1" {
		t.Logf("%s is not set: the Docker build was not compared", dockerTests)
		return
	}
	d := requireDocker(t)
	built, err := build(t, d, req)
	if err != nil {
		t.Fatal(err)
	}
	id, err := d.output(ctx, nil, "create", built.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer d.removeContainer(id)
	out := filepath.Join(t.TempDir(), "worker")
	if _, err := d.output(ctx, nil, "cp", id+":/worker", out); err != nil {
		t.Fatal(err)
	}
	digest, size, err := hashFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if digest != rt.Digest || size != rt.Size {
		t.Fatalf("the Docker build made %s (%d bytes), the native build %s (%d bytes)", digest, size, rt.Digest, rt.Size)
	}
	if built.Hello.Runtime != rt.Hello.Runtime {
		t.Errorf("runtime identity %s on Docker, %s native", built.Hello.Runtime, rt.Hello.Runtime)
	}
}
