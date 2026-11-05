package worker_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// These tests drive workers through the platform, which imports this
// package (through its Docker runtime store), so they are external tests.
// TestMain of the internal tests still serves the worker side.

var cli = platform.Local("cli")

func startWorker(t *testing.T, mode string) *worker.Client {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "LINEA_TEST_WORKER="+mode)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := worker.Start(ctx, "test-worker", cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func samplePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "samples"}, parts...)...)
}

// TestABundleForAnotherRuntimeIsAnUnsupportedBuild classifies a worker's
// refusal the way a platform run does, through a service whose executor
// returns it.
func TestABundleForAnotherRuntimeIsAnUnsupportedBuild(t *testing.T) {
	c := startWorker(t, "serve")
	req, err := engine.Prepare(context.Background(), engine.Request{Config: samplePath("logs-cleanup", "config.yaml"), Fixture: samplePath("logs-cleanup", "input.json")})
	if err != nil {
		t.Fatal(err)
	}
	req.Runtime = &engine.RuntimeTarget{ID: "sha256:another", Name: "another"}
	_, err = c.Execute(context.Background(), req)
	if !errors.Is(err, engine.ErrDistribution) || !strings.Contains(err.Error(), "another") {
		t.Fatalf("bundle for another runtime = %v", err)
	}
	s := platform.New(platform.Options{
		Prepare: func(_ context.Context, r engine.Request) (engine.Request, error) { return r, nil },
		Execute: func(context.Context, engine.Request) (*engine.Report, error) { return nil, err },
	})
	defer s.Close()
	_, perr := s.ExecuteRun(context.Background(), cli, platform.RunRequest{
		Mode: platform.ModeQuick, Config: platform.Source{Text: "x"}, Quick: &platform.QuickInput{Fixture: platform.Source{Text: "{}"}},
	})
	if platform.ErrorCode(perr) != platform.CodeUnsupportedBuild {
		t.Fatalf("code = %q", platform.ErrorCode(perr))
	}
}
