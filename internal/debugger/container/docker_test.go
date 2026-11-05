package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// dockerTests enables the tests that build runtimes in Docker. They compile
// the worker with every built-in component (minutes, and gigabytes of
// module and build cache on the first run), so go test ./... skips them.
const dockerTests = "LINEA_DOCKER_TESTS"

func requireDocker(t *testing.T) *Docker {
	t.Helper()
	if os.Getenv(dockerTests) != "1" {
		t.Skipf("set %s=1 to build runtimes in Docker", dockerTests)
	}
	d := &Docker{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.Version(ctx); err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	return d
}

func packedSource(t *testing.T) fstest.MapFS {
	t.Helper()
	raw, err := workersrc.Pack("../../..")
	if err != nil {
		t.Fatal(err)
	}
	src, err := workersrc.Unpack(raw)
	if err != nil {
		t.Fatal(err)
	}
	return src.(fstest.MapFS)
}

func build(t *testing.T, d *Docker, req BuildRequest) (*Runtime, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	req.Log = func(line string) { t.Log(line) }
	// Kept small for developer machines; the built-in components fit.
	req.Memory, req.CPUs = 3<<30, 2
	rt, err := d.Build(ctx, req)
	if rt != nil {
		t.Cleanup(func() { _ = d.Remove(context.Background(), rt.Ref) })
	}
	return rt, err
}

func sample(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "samples"}, parts...)...)
}

// TestABuiltRuntimeRunsTheSamplesAsTheBuiltIn builds the built-in manifest
// in Docker and runs every sample, quick and scenario, on its worker
// container: the normalized reports equal the in-process engine's. The
// container has no network, a read-only root, a non-root user and the
// memory limit; a rebuild from the warm cache needs no network.
func TestABuiltRuntimeRunsTheSamplesAsTheBuiltIn(t *testing.T) {
	d := requireDocker(t)
	src := packedSource(t)
	rt, err := build(t, d, BuildRequest{Name: "test-built-in", Manifest: builtInManifest(t), Source: src, Set: set162(t)})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Labels[LabelCollectorVersion] != "0.162.0" || rt.Labels[LabelVerified] != "true" || rt.Labels[LabelManifest] != string(builtInManifest(t)) {
		t.Fatalf("labels %v", rt.Labels)
	}
	img, err := d.Inspect(context.Background(), rt.Ref)
	if err != nil || img.ID != rt.ImageID || img.Name() != "test-built-in" {
		t.Fatalf("inspect: %+v, %v", img, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	const memory = 1 << 30
	w, err := d.Start(ctx, rt.Ref, rt.Name, RunOptions{Memory: memory})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	out, err := d.output(ctx, nil, "container", "inspect", w.Container())
	if err != nil {
		t.Fatal(err)
	}
	var info []struct {
		Config     struct{ User string }
		HostConfig struct {
			NetworkMode    string
			Memory         int64
			ReadonlyRootfs bool
		}
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info) != 1 {
		t.Fatalf("container inspect: %v", err)
	}
	if c := info[0]; c.HostConfig.NetworkMode != "none" || c.HostConfig.Memory != memory || !c.HostConfig.ReadonlyRootfs || c.Config.User != "65534:65534" {
		t.Fatalf("container settings %+v", c)
	}

	entries, err := os.ReadDir(sample())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, mode := range []string{"quick", "scenario"} {
			t.Run(e.Name()+"/"+mode, func(t *testing.T) {
				req := engine.Request{Config: sample(e.Name(), "config.yaml"), Normalize: true}
				if mode == "quick" {
					req.Fixture = sample(e.Name(), "input.json")
				} else {
					req.Scenario = sample(e.Name(), "scenario.yaml")
				}
				prepared, err := engine.Prepare(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				want, err := engine.Execute(ctx, prepared)
				if err != nil {
					t.Fatal(err)
				}
				got, err := w.Execute(ctx, prepared)
				if err != nil {
					t.Fatal(err)
				}
				var a, b bytes.Buffer
				if err := engine.Encode(&a, want); err != nil {
					t.Fatal(err)
				}
				if err := engine.Encode(&b, got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(a.Bytes(), b.Bytes()) {
					t.Fatalf("container report differs from the in-process report\nin process: %.2000s\ncontainer:  %.2000s", a.Bytes(), b.Bytes())
				}
			})
		}
	}

	// The container has no network: an otlp exporter to a public address
	// still completes, its output captured.
	t.Run("public exporter", func(t *testing.T) {
		raw, err := os.ReadFile(sample("logs-cleanup", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := strings.Replace(string(raw), "backend.example.invalid:4317", "1.1.1.1:4317", 1)
		if cfg == string(raw) {
			t.Fatal("the sample's exporter endpoint changed")
		}
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		prepared, err := engine.Prepare(ctx, engine.Request{Config: path, Fixture: sample("logs-cleanup", "input.json")})
		if err != nil {
			t.Fatal(err)
		}
		got, err := w.Execute(ctx, prepared)
		if err != nil {
			t.Fatal(err)
		}
		if got.Result.Status != "completed" {
			t.Fatalf("status %s: %s", got.Result.Status, got.Result.StatusReason)
		}
		for _, o := range got.Result.Outputs {
			if o.Exporter == "otlp/backend" && o.Batches > 0 {
				return
			}
		}
		t.Fatalf("otlp/backend captured nothing: %+v", got.Result.Outputs)
	})

	t.Run("offline rebuild", func(t *testing.T) {
		if _, err := build(t, d, BuildRequest{Name: "test-built-in-offline", Manifest: builtInManifest(t), Source: src, Set: set162(t), Offline: true}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestABuildWhoseWorkerPanicsAtStartFails builds a worker that panics while
// it starts, as asapauthextension's does under Go 1.27: the build
// fails with the panic and leaves no image.
func TestABuildWhoseWorkerPanicsAtStartFails(t *testing.T) {
	d := requireDocker(t)
	src := packedSource(t)
	src["cmd/linea-worker/panic.go"] = &fstest.MapFile{Data: []byte("package main\n\nfunc init() { panic(\"component failed to register\") }\n")}
	_, err := build(t, d, BuildRequest{Name: "test-panics", Manifest: builtInManifest(t), Source: src})
	var be *BuildError
	if !errors.As(err, &be) || be.Stage != "starting the worker" || !strings.Contains(err.Error(), "component failed to register") {
		t.Fatalf("got %v, want a start failure with the panic", err)
	}
	if _, err := d.Inspect(context.Background(), Ref("test-panics")); err == nil {
		t.Fatal("the image of a failed build was kept")
	}
}
