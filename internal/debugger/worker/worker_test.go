package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// The test binary is its own worker: started with workerMode set, TestMain
// serves the protocol instead of running tests. Its registry is the
// built-in one, generated from the default manifest.
const workerMode = "LINEA_TEST_WORKER"

func TestMain(m *testing.M) {
	switch os.Getenv(workerMode) {
	case "":
		os.Exit(m.Run())
	case "serve":
		if err := Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "protocol-99":
		h, _ := Identity()
		h.Protocol = 99
		_ = writeFrame(os.Stdout, &message{Type: typeHello, Hello: &h})
		_, _ = os.Stdin.Read(make([]byte, 1))
		os.Exit(0)
	case "die":
		fmt.Fprintln(os.Stderr, "worker failed to load its registry")
		os.Exit(3)
	}
}

func startWorker(t *testing.T, mode string) *Client {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerMode+"="+mode)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := Start(ctx, "test-worker", cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func samplePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "samples"}, parts...)...)
}

func samples(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(samplePath())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		t.Fatal("no samples")
	}
	return out
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := engine.Encode(&b, v); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestWorkerReportsEqualTheInProcessEngine runs every sample in quick and
// scenario mode in process and on a worker built from the same manifest:
// the normalized reports are byte-identical, and every event reached the
// observer.
func TestWorkerReportsEqualTheInProcessEngine(t *testing.T) {
	c := startWorker(t, "serve")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range samples(t) {
		for _, mode := range []string{"quick", "scenario"} {
			t.Run(id+"/"+mode, func(t *testing.T) {
				req := engine.Request{Config: samplePath(id, "config.yaml"), Normalize: true}
				if mode == "quick" {
					req.Fixture = samplePath(id, "input.json")
				} else {
					req.Scenario = samplePath(id, "scenario.yaml")
				}
				prepared, err := engine.Prepare(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				want, err := engine.Execute(ctx, prepared)
				if err != nil {
					t.Fatal(err)
				}
				var events atomic.Int64
				prepared.Observer = func(instrument.Event) { events.Add(1) }
				got, err := c.Execute(ctx, prepared)
				if err != nil {
					t.Fatal(err)
				}
				if a, b := encode(t, want), encode(t, got); !bytes.Equal(a, b) {
					t.Fatalf("worker report differs from the in-process report\nin process: %.2000s\nworker:     %.2000s", a, b)
				}
				if n := int(events.Load()); n != len(got.Result.Events) || n == 0 {
					t.Fatalf("observer saw %d events, the report has %d", n, len(got.Result.Events))
				}
			})
		}
	}
}

// TestWorkerInspectsAndPreviewsAsTheInProcessEngine compares inspections
// and an OTTL preview completed by a worker with the in-process results.
func TestWorkerInspectsAndPreviewsAsTheInProcessEngine(t *testing.T) {
	c := startWorker(t, "serve")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pol := engine.Policy{Paths: hostfs.Policy{Unrestricted: true}}
	for _, req := range []engine.InspectRequest{
		{Config: samplePath("logs-cleanup", "config.yaml")},
		{Config: samplePath("container-logs", "config.yaml"), Scenario: samplePath("container-logs", "scenario.yaml")},
		{ConfigYAML: "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], exporters: [nope]}}}\n"},
		{ConfigYAML: "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {mystery: {}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [mystery], exporters: [nop]}}}\n"},
	} {
		want, err := engine.InspectWithPolicy(ctx, req, pol)
		if err != nil {
			t.Fatal(err)
		}
		in, done, err := engine.CaptureInspection(ctx, req, pol)
		if err != nil {
			t.Fatal(err)
		}
		got := done
		if in != nil {
			if got, err = c.Inspect(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		if a, b := encode(t, want), encode(t, got); !bytes.Equal(a, b) {
			t.Fatalf("inspection differs\nin process: %s\nworker:     %s", a, b)
		}
	}

	input, err := os.ReadFile(samplePath("logs-cleanup", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	preview := engine.OTTLRequest{Config: samplePath("logs-cleanup", "config.yaml"), Processor: "transform/severity", Input: input}
	want, err := engine.EvaluateOTTLWithPolicy(ctx, preview, pol)
	if err != nil {
		t.Fatal(err)
	}
	in, done, err := engine.CaptureOTTL(ctx, preview, pol)
	if err != nil || done != nil {
		t.Fatalf("capture = %v, %v", done, err)
	}
	got, err := c.EvaluateOTTL(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := encode(t, want), encode(t, got); !bytes.Equal(a, b) {
		t.Fatalf("preview differs\nin process: %s\nworker:     %s", a, b)
	}

	// A request error keeps its kind across the boundary.
	in, _, err = engine.CaptureOTTL(ctx, engine.OTTLRequest{Config: preview.Config, Processor: "transform/absent", Input: input}, pol)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.EvaluateOTTL(ctx, in); !errors.Is(err, engine.ErrInvalidRequest) || !strings.Contains(err.Error(), "transform/absent") {
		t.Fatalf("absent processor = %v", err)
	}
}

// slowRun waits two minutes between its inputs, so it is still running
// when a test cancels it or stops its worker.
func slowRun(t *testing.T) engine.Request {
	t.Helper()
	input, err := filepath.Abs(samplePath("logs-cleanup", "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	input = filepath.ToSlash(input)
	scenario := fmt.Sprintf("version: 1\nname: slow\ninputs:\n  - receiver: otlp\n    file: %q\n  - after: 2m\n    receiver: otlp\n    file: %q\n", input, input)
	req, err := engine.Prepare(context.Background(), engine.Request{Config: samplePath("logs-cleanup", "config.yaml"), ScenarioYAML: scenario})
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// observed returns an observer and a channel closed at the first event.
func observed() (func(instrument.Event), <-chan struct{}) {
	started := make(chan struct{})
	var once sync.Once
	return func(instrument.Event) { once.Do(func() { close(started) }) }, started
}

func TestCancellationReachesTheWorker(t *testing.T) {
	c := startWorker(t, "serve")
	req := slowRun(t)
	var started <-chan struct{}
	req.Observer, started = observed()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		rep *engine.Report
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		rep, err := c.Execute(ctx, req)
		done <- outcome{rep, err}
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the run did not start")
	}

	// The worker answers an inspection while the run executes.
	in, _, err := engine.CaptureInspection(context.Background(), engine.InspectRequest{Config: samplePath("logs-cleanup", "config.yaml")}, engine.Policy{Paths: hostfs.Policy{Unrestricted: true}})
	if err != nil {
		t.Fatal(err)
	}
	if ins, err := c.Inspect(context.Background(), in); err != nil || !ins.Valid {
		t.Fatalf("inspection during a run = %+v, %v", ins, err)
	}

	cancelled := time.Now()
	cancel()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("cancelled run = %v", o.err)
		}
		if o.rep.Result.Status != runresult.StatusCancelled {
			t.Fatalf("status = %q, want cancelled", o.rep.Result.Status)
		}
		if waited := time.Since(cancelled); waited > 30*time.Second {
			t.Fatalf("cancellation took %v", waited)
		}
	case <-time.After(time.Minute):
		t.Fatal("cancellation did not reach the worker")
	}
}

func TestAWorkerThatExitsMidRunFailsTheRunNamingIt(t *testing.T) {
	c := startWorker(t, "serve")
	req := slowRun(t)
	var started <-chan struct{}
	req.Observer, started = observed()
	done := make(chan error, 1)
	go func() {
		_, err := c.Execute(context.Background(), req)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Minute):
		t.Fatal("the run did not start")
	}
	c.kill()
	select {
	case err := <-done:
		if !errors.Is(err, ErrWorkerExited) || !errors.Is(err, engine.ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "test-worker") {
			t.Fatalf("run error = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the run did not fail")
	}
	// Later requests fail the same way rather than hang.
	if _, err := c.Execute(context.Background(), slowRun(t)); !errors.Is(err, ErrWorkerExited) {
		t.Fatalf("request after exit = %v", err)
	}
}

func TestAWorkerRefusesABundleForAnotherRuntime(t *testing.T) {
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
}

func TestStartRefusesAnotherProtocolAndReportsAWorkerThatDies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerMode+"=protocol-99")
	if _, err := Start(ctx, "old-worker", cmd); err == nil || !strings.Contains(err.Error(), "protocol 99") {
		t.Fatalf("protocol mismatch = %v", err)
	}
	cmd = exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), workerMode+"=die")
	_, err := Start(ctx, "dead-worker", cmd)
	if !errors.Is(err, engine.ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "failed to load its registry") {
		t.Fatalf("dying worker = %v", err)
	}
}

func TestErrorsKeepTheirKindAcrossTheBoundary(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: bad", engine.ErrInvalidRequest),
		fmt.Errorf("%w: other build", engine.ErrDistribution),
		engine.ErrBundleSchema,
		fmt.Errorf("run: %w", context.DeadlineExceeded),
		context.Canceled,
		errors.New("plain"),
		suspectError{fmt.Errorf("%w: bad binary", engine.ErrRuntimeUnavailable)},
	} {
		got := decodeError(roundTrip(t, encodeError(err)))
		if got.Error() != err.Error() {
			t.Fatalf("message %q became %q", err, got)
		}
		for _, k := range kinds {
			if errors.Is(err, k.err) != errors.Is(got, k.err) {
				t.Fatalf("%v: errors.Is(%v) changed", err, k.err)
			}
		}
		if errors.Is(err, ErrSuspectBinary) != errors.Is(got, ErrSuspectBinary) {
			t.Fatalf("%v: errors.Is(ErrSuspectBinary) changed", err)
		}
	}
}

func roundTrip(t *testing.T, w *wireError) *wireError {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var out wireError
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// TestTheWorkerCommandServesTheBuiltInRegistry builds cmd/linea-worker
// and checks its hello and one run against the in-process engine.
func TestTheWorkerCommandServesTheBuiltInRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the worker binary")
	}
	bin := filepath.Join(t.TempDir(), "linea-worker")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/linea-worker")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := Start(ctx, "linea-worker", exec.Command(bin))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	self, err := Identity()
	if err != nil {
		t.Fatal(err)
	}
	h := c.Hello()
	if h.Protocol != ProtocolVersion || h.CollectorVersion != self.CollectorVersion || !h.Verified ||
		strings.Join(h.Modules, "\n") != strings.Join(self.Modules, "\n") || !strings.HasPrefix(h.Runtime, "sha256:") {
		t.Fatalf("hello = %+v", h)
	}
	req, err := engine.Prepare(ctx, engine.Request{Config: samplePath("logs-cleanup", "config.yaml"), Scenario: samplePath("logs-cleanup", "scenario.yaml"), Normalize: true})
	if err != nil {
		t.Fatal(err)
	}
	want, err := engine.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(t, want), encode(t, got)) {
		t.Fatal("the worker command's report differs from the in-process report")
	}
}

// A worker returns the run's redacted Collector log tail with its result,
// as the in-process engine keeps it.
func TestTheWorkerReturnsTheRunLog(t *testing.T) {
	c := startWorker(t, "serve")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := samples(t)[0]
	prepared, err := engine.Prepare(ctx, engine.Request{Config: samplePath(id, "config.yaml"), Scenario: samplePath(id, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	prepared.Log = &engine.LogTail{}
	if _, err := c.Execute(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	log, truncated := prepared.Log.Tail()
	if truncated || !strings.Contains(log, "\tINFO\t") {
		t.Fatalf("worker log (truncated %t):\n%s", truncated, log)
	}
}
