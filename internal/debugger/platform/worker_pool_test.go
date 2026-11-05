package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/worker"
)

const poolToken = "registration-token-for-the-pool-tests"

// connectPool runs a worker in this process that dials s under name with
// capacity, waits until s serves it, registers name with its hello and
// returns a function that stops it.
func connectPool(t *testing.T, s *Service, name string, capacity int) (disconnect func()) {
	t.Helper()
	disconnect = connectPoolUnregistered(t, s, name, capacity)
	registerPool(t, s, name)
	return disconnect
}

// connectPoolUnregistered connects a worker as connectPool does, leaving
// the catalog as it is.
func connectPoolUnregistered(t *testing.T, s *Service, name string, capacity int) (disconnect func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.ConnectWorker))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- worker.Connect(ctx, worker.ConnectOptions{URL: srv.URL, Token: poolToken, Name: name, Capacity: capacity})
	}()
	stopped := false
	disconnect = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Connect = %v", err)
		}
	}
	t.Cleanup(disconnect)
	until(t, func() bool { return s.connectedSlots(name) == capacity })
	return disconnect
}

// registerPool registers name in the catalog with the hello of a worker
// connected under it, as linea runtime register reads it from the image.
func registerPool(t *testing.T, s *Service, name string) *RuntimeInfo {
	t.Helper()
	info, err := s.RegisterRuntime(context.Background(), testCaller, RegisterRuntimeRequest{
		Name: name, Image: "registry.example/linea-runtime:" + name, Hello: poolHello(t, s, name)})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// poolHello is the hello of a worker connected under name, encoded as
// linea runtime register sends it.
func poolHello(t *testing.T, s *Service, name string) json.RawMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.executors {
		if e.conn != nil && e.conn.hello.Name == name {
			return rawHello(t, e.conn.hello)
		}
	}
	t.Fatalf("no worker is connected under %s", name)
	return nil
}

func rawHello(t *testing.T, h worker.Hello) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// connectedSlots counts the slots of the workers connected under name.
func (s *Service) connectedSlots(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.executors {
		if e.conn != nil && e.conn.hello.Name == name {
			n += e.capacity
		}
	}
	return n
}

// until waits for a condition another goroutine makes true: a worker's
// connection is accepted in a handler that does not return while it lasts.
func until(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the pool")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// slowPoolRun is a scenario run on runtime that waits two minutes between
// its inputs, so it is still running when a test stops it.
func slowPoolRun(t *testing.T, runtime string) RunRequest {
	t.Helper()
	input, err := filepath.Abs(logsCleanup("input.json"))
	if err != nil {
		t.Fatal(err)
	}
	input = filepath.ToSlash(input)
	scenario := fmt.Sprintf("version: 1\nname: slow\ninputs:\n  - receiver: otlp\n    file: %q\n  - after: 2m\n    receiver: otlp\n    file: %q\n", input, input)
	return RunRequest{Mode: ModeScenario, Runtime: runtime, Config: Source{Path: logsCleanup("config.yaml")}, Scenario: &ScenarioInput{Scenario: Source{Text: scenario}}}
}

// submitPool submits a run and returns its handle.
func submitPool(t *testing.T, s *Service, req RunRequest) *Handle {
	t.Helper()
	run, err := s.SubmitRun(context.Background(), testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// executing waits until the run's Collector has recorded an event.
func executing(t *testing.T, s *Service, h *Handle) {
	t.Helper()
	executingAttempt(t, s, h, 1)
}

// executingAttempt waits until the run's attempt has recorded an event.
func executingAttempt(t *testing.T, s *Service, h *Handle, attempt int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		w, err := s.WatchRun(ctx, testCaller, h.ID(), EventCursor{})
		if err != nil {
			t.Fatal(err)
		}
		if w.Attempt == attempt && len(w.Events) > 0 {
			return
		}
		if w.Terminal {
			t.Fatalf("run %s ended before it executed: %v", h.ID(), h.Err())
		}
		select {
		case <-w.Changed:
		case <-ctx.Done():
			t.Fatalf("run %s did not execute", h.ID())
		}
	}
}

func cancelRun(t *testing.T, s *Service, h *Handle) {
	t.Helper()
	if _, err := s.CancelRun(context.Background(), testCaller, h.ID()); err != nil {
		t.Fatal(err)
	}
}

func waitPoolRun(t *testing.T, h *Handle) Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("run %s: %v", h.ID(), err)
	}
	return h.View(false)
}

// TestOneConnectedWorkerRunsEightQueuedRunsAtOnce covers the
// platform: eight queued runs execute together on one worker of capacity
// eight, a ninth waits for a free slot, and a run completes as the
// in-process engine completes it.
func TestOneConnectedWorkerRunsEightQueuedRunsAtOnce(t *testing.T) {
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken})
	defer s.Close()
	connectPool(t, s, "team", 8)
	var runs []*Handle
	for range 8 {
		runs = append(runs, submitPool(t, s, slowPoolRun(t, "team")))
	}
	for _, h := range runs {
		executing(t, s, h)
	}
	ninth := submitPool(t, s, slowPoolRun(t, "team"))
	if v := ninth.View(false); v.State != StateQueued {
		t.Fatalf("ninth run = %s while eight run", v.State)
	}
	cancelRun(t, s, runs[0])
	waitPoolRun(t, runs[0])
	executing(t, s, ninth)
	for _, h := range append(runs[1:], ninth) {
		cancelRun(t, s, h)
	}
	for _, h := range append(runs, ninth) {
		waitPoolRun(t, h)
		if v := h.View(true); v.State != StateDone || v.Report.Result.Status != runresult.StatusCancelled {
			t.Fatalf("cancelled run %s = %s, %v", h.ID(), v.State, h.Err())
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := RunRequest{Mode: ModeQuick, Config: Source{Path: logsCleanup("config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}, Normalize: true}
	builtIn, err := s.ExecuteRun(ctx, testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	req.Runtime = "team"
	rep, err := s.ExecuteRun(ctx, testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Runtime == nil || rep.Runtime.Name != "team" {
		t.Fatalf("runtime = %+v", rep.Runtime)
	}
	a, b := *rep, *builtIn
	a.Runtime, b.Runtime = nil, nil
	if ja, jb := mustJSON(t, a), mustJSON(t, b); ja != jb {
		t.Fatal("the connected worker's report differs from the built-in run's beyond its runtime")
	}
}

// TestInteractiveRequestsGoAheadOfQueuedRuns fills a worker's one slot,
// queues a second run and waits for an inspection: when the slot frees,
// the inspection takes it, not the queued run.
func TestInteractiveRequestsGoAheadOfQueuedRuns(t *testing.T) {
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken})
	defer s.Close()
	connectPool(t, s, "team", 1)
	first := submitPool(t, s, slowPoolRun(t, "team"))
	executing(t, s, first)
	second := submitPool(t, s, slowPoolRun(t, "team"))

	type outcome struct {
		ins *Inspection
		err error
	}
	inspected := make(chan outcome, 1)
	go func() {
		ins, err := s.InspectConfiguration(context.Background(), testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "team"})
		inspected <- outcome{ins, err}
	}()
	until(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.waiting) == 1
	})
	cancelRun(t, s, first)
	// Had the queued run taken the slot, the inspection would wait two
	// minutes for it.
	select {
	case o := <-inspected:
		if o.err != nil || !o.ins.Valid {
			t.Fatalf("inspection = %+v, %v", o.ins, o.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the queued run took the freed slot before the waiting inspection")
	}
	executing(t, s, second)
	cancelRun(t, s, second)
	waitPoolRun(t, second)
}

// TestARunOnALostWorkerIsRequeued disconnects the worker running a run:
// the run stays running until its lease ends, then runs again on the
// worker that connected since. A watcher that followed the first attempt
// is restarted at the second attempt's first event.
func TestARunOnALostWorkerIsRequeued(t *testing.T) {
	clock := newTestClock()
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken, Now: clock.Now})
	defer s.Close()
	disconnect := connectPool(t, s, "team", 1)
	h := submitPool(t, s, slowPoolRun(t, "team"))
	executing(t, s, h)
	disconnect()
	w, err := s.WatchRun(context.Background(), testCaller, h.ID(), EventCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Attempt != 1 || w.Restarted {
		t.Fatalf("first attempt watch = attempt %d, restarted %v", w.Attempt, w.Restarted)
	}
	cursor := EventCursor{Attempt: 1, Seq: w.Events[len(w.Events)-1].Seq}
	until(t, func() bool { return s.connectedSlots("team") == 0 })
	if v := h.View(false); v.State != StateRunning || v.Attempt != 1 {
		t.Fatalf("run on the lost worker = %s attempt %d", v.State, v.Attempt)
	}

	connectPool(t, s, "team", 1)
	s.heartbeat(clock.Now())
	if v := h.View(false); v.Attempt != 1 {
		t.Fatalf("the run was requeued before its lease ended: attempt %d", v.Attempt)
	}
	s.heartbeat(clock.Advance(DefaultLease + time.Second))
	until(t, func() bool { return h.View(false).Attempt == 2 })
	executingAttempt(t, s, h, 2)
	w, err = s.WatchRun(context.Background(), testCaller, h.ID(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Restarted || w.Attempt != 2 || len(w.Events) == 0 || w.Events[0].Seq != 1 {
		t.Fatalf("watch from the first attempt = attempt %d, restarted %v, %d events", w.Attempt, w.Restarted, len(w.Events))
	}
	for i, e := range w.Events {
		if e.Seq != i+1 {
			t.Fatalf("event %d has seq %d: the first attempt's events were kept", i, e.Seq)
		}
	}
	cancelRun(t, s, h)
	if v := waitPoolRun(t, h); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("requeued run = %s attempt %d, %v", v.State, v.Attempt, h.Err())
	}
}

// TestAWorkerIsRefusedWithoutTheRegistrationToken checks that a service
// without a token accepts no worker, and one with a token only workers
// that present it.
func TestAWorkerIsRefusedWithoutTheRegistrationToken(t *testing.T) {
	for _, tc := range []struct {
		name, serverToken, presented string
		want                         int
	}{
		{"no token configured", "", poolToken, http.StatusNotFound},
		{"no token presented", poolToken, "", http.StatusUnauthorized},
		{"another token", poolToken, "another-registration-token-value", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{WorkerToken: tc.serverToken})
			defer s.Close()
			r := httptest.NewRequest(http.MethodGet, worker.ConnectPath, nil)
			if tc.presented != "" {
				r.Header.Set("Authorization", "Bearer "+tc.presented)
			}
			w := httptest.NewRecorder()
			s.ConnectWorker(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if s.connectedSlots("team") != 0 {
				t.Fatal("a refused worker was added")
			}
		})
	}
}

// TestAConnectedWorkerReceivesOnlyAdmittedEnvironmentValues inspects a
// configuration reading a host variable under a service that reads the
// host: the built-in runtime uses the host value, a connected worker's
// runtime a stand-in unless the service imports the name.
func TestAConnectedWorkerReceivesOnlyAdmittedEnvironmentValues(t *testing.T) {
	const name = "LINEA_POOL_TEST_SECRET"
	t.Setenv(name, "host-secret")
	cfg := Source{Text: `receivers:
  otlp:
    protocols:
      grpc: {}
processors:
  transform:
    log_statements:
      - set(log.attributes["k"], "${env:` + name + `}")
exporters:
  debug: {}
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [transform]
      exporters: [debug]
`}
	ctx := context.Background()
	source := func(env config.EnvironmentPolicy, rt string) string {
		t.Helper()
		s := New(Options{Environment: env, WorkerToken: poolToken})
		defer s.Close()
		if rt != "" {
			connectPool(t, s, rt, 1)
		}
		ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: cfg, Runtime: rt})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range ins.References {
			if r.Target == name {
				return r.Source
			}
		}
		t.Fatalf("no reference to %s in %+v", name, ins.References)
		return ""
	}
	if got := source(config.EnvironmentPolicy{LegacyHost: true}, ""); got != config.SourceHost {
		t.Fatalf("built-in source = %s", got)
	}
	if got := source(config.EnvironmentPolicy{LegacyHost: true}, "lab"); got != config.SourceStandIn {
		t.Fatalf("connected worker source with a host-reading service = %s, want a stand-in", got)
	}
	if got := source(config.EnvironmentPolicy{Host: []string{name}}, "lab"); got != config.SourceHost {
		t.Fatalf("connected worker source of an imported name = %s", got)
	}
}

// TestACatalogRuntimeQueuesUntilAWorkerOfItsIdentityConnects covers
// the platform: a runtime registered from a hello takes runs while no
// worker of it is connected, refuses inspections until one is, and the
// first connected worker of its identity, whatever its name, confirms the
// entry and runs the queued run.
func TestACatalogRuntimeQueuesUntilAWorkerOfItsIdentityConnects(t *testing.T) {
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken})
	defer s.Close()
	// The worker's hello, as linea runtime register reads it from an image.
	probe := connectPool(t, s, "probe", 1)
	raw := poolHello(t, s, "probe")
	var hello worker.Hello
	if err := json.Unmarshal(raw, &hello); err != nil {
		t.Fatal(err)
	}
	probe()
	until(t, func() bool { return s.connectedSlots("probe") == 0 })
	if err := s.DeleteRuntime(context.Background(), testCaller, "probe"); err != nil {
		t.Fatal(err)
	}

	info, err := s.RegisterRuntime(context.Background(), testCaller, RegisterRuntimeRequest{Name: "edge", Image: "registry.example/linea-runtime:edge", Digest: "sha256:" + strings.Repeat("a", 64), Hello: raw})
	if err != nil {
		t.Fatal(err)
	}
	if c := info.Catalog; c == nil || c.Identity != hello.Runtime || !c.Confirmed.IsZero() || c.Workers != 0 || info.CollectorVersion != hello.CollectorVersion {
		t.Fatalf("registered runtime = %+v, %+v", info, info.Catalog)
	}
	if _, err := s.InspectConfiguration(context.Background(), testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "edge"}); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("inspection with no worker connected = %v", err)
	}
	if _, err := s.SubmitRun(context.Background(), testCaller, slowPoolRun(t, "unregistered")); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("run on an unregistered runtime = %v", err)
	}
	h := submitPool(t, s, slowPoolRun(t, "edge"))
	if v := h.View(false); v.State != StateQueued {
		t.Fatalf("run with no worker connected = %s", v.State)
	}

	connectPoolUnregistered(t, s, "another-label", 1)
	executing(t, s, h)
	got, err := s.GetRuntime(context.Background(), testCaller, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Catalog; c == nil || c.Confirmed.IsZero() || c.Workers != 1 || c.Slots != 1 || c.Image != "registry.example/linea-runtime:edge" {
		t.Fatalf("runtime after a worker connected = %+v", got.Catalog)
	}
	if err := s.DeleteRuntime(context.Background(), testCaller, "edge"); ErrorCode(err) != CodeConflict {
		t.Fatalf("deleting a runtime with a running run = %v", err)
	}
	cancelRun(t, s, h)
	if v := waitPoolRun(t, h); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, h.Err())
	}
	if rep := h.View(true).Report; rep == nil || rep.Runtime == nil || rep.Runtime.Name != "edge" {
		t.Fatalf("report runtime = %+v", rep)
	}
}

// TestRegisteringARuntimeChecksItsHello refuses registrations a worker of
// this debugger could not serve.
func TestRegisteringARuntimeChecksItsHello(t *testing.T) {
	s := New(Options{WorkerToken: poolToken})
	defer s.Close()
	identity := "sha256:" + strings.Repeat("b", 64)
	valid := rawHello(t, worker.Hello{Protocol: worker.ProtocolVersion, Runtime: identity})
	for _, tc := range []struct {
		name string
		req  RegisterRuntimeRequest
		want Code
	}{
		{"built-in name", RegisterRuntimeRequest{Name: BuiltInRuntime, Image: "x", Hello: valid}, CodeInvalidRequest},
		{"invalid name", RegisterRuntimeRequest{Name: "Edge", Image: "x", Hello: valid}, CodeInvalidRequest},
		{"not a hello", RegisterRuntimeRequest{Name: "edge", Image: "x", Hello: json.RawMessage(`[1]`)}, CodeInvalidRequest},
		{"no identity", RegisterRuntimeRequest{Name: "edge", Image: "x", Hello: rawHello(t, worker.Hello{Protocol: worker.ProtocolVersion})}, CodeInvalidRequest},
		{"another protocol", RegisterRuntimeRequest{Name: "edge", Image: "x", Hello: rawHello(t, worker.Hello{Protocol: worker.ProtocolVersion - 1, Runtime: identity})}, CodeUnsupportedBuild},
		{"no image", RegisterRuntimeRequest{Name: "edge", Hello: valid}, CodeInvalidRequest},
		{"invalid digest", RegisterRuntimeRequest{Name: "edge", Image: "x", Digest: "latest", Hello: valid}, CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.RegisterRuntime(context.Background(), testCaller, tc.req); ErrorCode(err) != tc.want {
				t.Fatalf("RegisterRuntime = %v, want %s", err, tc.want)
			}
		})
	}
	if _, err := s.GetRuntime(context.Background(), testCaller, "edge"); err == nil {
		t.Fatal("a refused registration was recorded")
	}
}
