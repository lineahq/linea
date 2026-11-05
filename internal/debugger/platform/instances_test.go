package platform

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/oidctest"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// Instance tests run two shared services on one in-memory workspace, as two
// team server instances on one PostgreSQL database.

// waitFor polls cond, which reads another instance's state through the change
// feed, until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstancesReadAndCancelEachOthersRuns(t *testing.T) {
	data := memory.NewData()
	started := make(chan engine.Request, 1)
	a := openPlatform(t, data, Options{Shared: true, Execute: blockingExecute(started)})
	run := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	// b has no executor slot to spare, so a keeps the run.
	b := openPlatform(t, data, Options{Shared: true, Execute: blockingExecute(make(chan engine.Request, 1))})
	ctx := context.Background()

	// A read on b catches up first, so it sees a's run at once.
	v, err := b.GetRun(ctx, testCaller, run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v.State != StateRunning || v.Attempt != 1 {
		t.Fatalf("b sees %s attempt %d", v.State, v.Attempt)
	}
	runs, err := allRuns(ctx, b, testCaller, RunFilter{})
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID() {
		t.Fatalf("b lists %+v, %v", runs, err)
	}

	// Cancellation on b reaches the executing instance through the queue.
	c, err := b.CancelRun(ctx, testCaller, run.ID())
	if err != nil || !c.Effective {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if v := waitRun(t, run); v.State != StateError || !errors.Is(run.Err(), context.Canceled) {
		t.Fatalf("a's run = %s, %v", v.State, run.Err())
	}
	remote, err := b.Get(run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, remote); v.State != StateError {
		t.Fatalf("b's copy = %s", v.State)
	}
}

func TestInstanceAdoptsQueuedRunsOfAnyInstance(t *testing.T) {
	data := memory.NewData()
	// a's executor never frees, so its second run waits in the queue.
	held := make(chan engine.Request, 1)
	a := openPlatform(t, data, Options{Shared: true, Execute: blockingExecute(held)})
	submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, held)
	waiting := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})

	executed := make(chan engine.Request, 1)
	b := openPlatform(t, data, Options{Shared: true, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return report(req, "completed"), nil
	}})
	if req := receive(t, executed); req.RunID != waiting.ID() || !req.Prepared() {
		t.Fatalf("b executed %q, prepared %v", req.RunID, req.Prepared())
	}
	// a learns the outcome b published.
	if v := waitRun(t, waiting); v.State != StateDone {
		t.Fatalf("a's copy = %s, %v", v.State, waiting.Err())
	}
	own, err := b.Get(waiting.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, own); v.State != StateDone || v.Report == nil {
		t.Fatalf("b's run = %s", v.State)
	}
}

func TestInstanceRequeuesRunsOfAKilledInstance(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	started := make(chan engine.Request, 1)
	a, kill := openKillable(t, data, Options{Shared: true, Now: clock.Now, Execute: blockingExecute(started)})
	run := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)

	executed := make(chan engine.Request, 1)
	b := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return report(req, "completed"), nil
	}})
	kill()

	b.heartbeat(clock.Advance(DefaultLease + time.Second))
	if req := receive(t, executed); req.RunID != run.ID() {
		t.Fatalf("b executed %q", req.RunID)
	}
	adopted, err := b.Get(run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, adopted); v.State != StateDone || v.Attempt != 2 {
		t.Fatalf("adopted run = %s attempt %d, %v", v.State, v.Attempt, adopted.Err())
	}
}

// TestADrainedInstanceHandsItsRunBack stops an instance mid-run as SIGTERM
// does: another instance runs it, and the handed-back attempt does not count,
// so a later lease that ends still requeues it.
func TestADrainedInstanceHandsItsRunBack(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	started := make(chan engine.Request, 1)
	a := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: blockingExecute(started)})
	run := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Drain(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("drain of a running instance = %v", err)
	}

	taken := make(chan engine.Request, 1)
	_, kill := openKillable(t, data, Options{Shared: true, Now: clock.Now, Execute: blockingExecute(taken)})
	a.Close()
	if req := receive(t, taken); req.RunID != run.ID() {
		t.Fatalf("b executed %q", req.RunID)
	}
	kill()

	executed := make(chan engine.Request, 1)
	c := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return report(req, "completed"), nil
	}})
	c.heartbeat(clock.Advance(DefaultLease + time.Second))
	receive(t, executed)
	adopted, err := c.Get(run.ID())
	if err != nil {
		t.Fatal(err)
	}
	if v := waitRun(t, adopted); v.State != StateDone || v.Attempt != 3 {
		t.Fatalf("run = %s attempt %d, %v", v.State, v.Attempt, adopted.Err())
	}
	if err := c.Drain(context.Background()); err != nil {
		t.Errorf("drain of an idle instance = %v", err)
	}
}

// TestARequeuedRunAnotherInstanceClaimedIsRestored: b adopts a's queued run
// but a claims it and dies; b's requeue restores the request from the
// bundle rather than running the empty request of its unclaimed job.
func TestARequeuedRunAnotherInstanceClaimedIsRestored(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	ctx := context.Background()
	onA, onB := make(chan engine.Request, 1), make(chan engine.Request, 1)
	a, kill := openKillable(t, data, Options{Shared: true, Now: clock.Now, Execute: blockingExecute(onA)})
	first := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, onA)
	b := openPlatform(t, data, Options{Shared: true, Now: clock.Now, Execute: blockingExecute(onB)})
	busy := submit(t, b, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if req := receive(t, onB); req.RunID != busy.ID() {
		t.Fatalf("b executed %q", req.RunID)
	}

	queued := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	waitFor(t, "b to adopt the queued run", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.jobs[queued.ID()] != nil
	})
	if _, err := a.CancelRun(ctx, testCaller, first.ID()); err != nil {
		t.Fatal(err)
	}
	if req := receive(t, onA); req.RunID != queued.ID() {
		t.Fatalf("a executed %q", req.RunID)
	}
	kill()

	b.heartbeat(clock.Advance(DefaultLease + time.Second))
	if _, err := b.CancelRun(ctx, testCaller, busy.ID()); err != nil {
		t.Fatal(err)
	}
	if req := receive(t, onB); req.RunID != queued.ID() || !req.Prepared() {
		t.Fatalf("b executed %q, prepared %v", req.RunID, req.Prepared())
	}
}

func TestInstancesSeeEachOthersLiveEventsAndSuites(t *testing.T) {
	data := memory.NewData()
	started := make(chan engine.Request, 1)
	a := openPlatform(t, data, Options{Shared: true, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		req.Observer(instrument.Event{ID: req.RunID + ":1", RunID: req.RunID, Seq: 1, Kind: instrument.KindRunStarted, Time: time.Now().UTC()})
		started <- req
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}})
	run := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	b := openPlatform(t, data, Options{Shared: true, Execute: blockingExecute(make(chan engine.Request, 1))})

	ctx := context.Background()
	waitFor(t, "a's live event on b", func() bool {
		w, err := b.WatchRun(ctx, testCaller, run.ID(), EventCursor{})
		return err == nil && len(w.Events) == 1
	})
	if err := run.Cancel(); err != nil {
		t.Fatal(err)
	}
	waitRun(t, run)

	// Deletion on one instance removes the run from the other.
	if err := b.DeleteRun(ctx, testCaller, run.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetRun(ctx, testCaller, run.ID()); ErrorCode(err) != CodeNotFound {
		t.Fatalf("a reads a deleted run: %v", err)
	}
}

// TestAnInspectionReachesAWorkerOfAnotherInstance connects the runtime's only
// worker to one instance and inspects on the other: the request goes as a
// job row and the worker's instance answers it.
func TestAnInspectionReachesAWorkerOfAnotherInstance(t *testing.T) {
	data := memory.NewData()
	opts := Options{Shared: true, Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken}
	a := openPlatform(t, data, opts)
	b := openPlatform(t, data, opts)
	connectPool(t, a, "team", 1)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ins, err := b.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Path: logsCleanup("config.yaml")}, Runtime: "team"})
	if err != nil || !ins.Valid {
		t.Fatalf("inspection = %+v, %v", ins, err)
	}
	// Answered jobs keep no history.
	b.mu.Lock()
	waiters := len(b.jobWaiters)
	b.mu.Unlock()
	if waiters != 0 {
		t.Errorf("%d requests still wait", waiters)
	}
	changes, _, err := b.repository.Changes(ctx, artifact.ChangeCursor{}, artifact.MaxChanges)
	if err != nil {
		t.Fatal(err)
	}
	jobs := 0
	for _, c := range changes {
		if c.Kind == artifact.ChangeJob {
			jobs++
		}
	}
	if jobs != 2 {
		t.Errorf("%d job changes, want the posting and the answer", jobs)
	}
}

// TestTheHeartbeatClaimsARunTheChangeFeedMissed queues a run on a for a
// runtime whose only worker is connected to b, and drops it from b's jobs as
// if b had never heard of it: b's next heartbeat still claims it for the
// worker, which runs it.
func TestTheHeartbeatClaimsARunTheChangeFeedMissed(t *testing.T) {
	data := memory.NewData()
	opts := Options{Shared: true, Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken}
	a := openPlatform(t, data, opts)
	b := openPlatform(t, data, opts)
	connectPool(t, b, "team", 1)()

	run := submitPool(t, a, RunRequest{Mode: ModeQuick, Runtime: "team", Config: Source{Path: logsCleanup("config.yaml")},
		Quick: &QuickInput{Fixture: Source{Path: logsCleanup("input.json")}}})
	waitFor(t, "b to adopt the queued run", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.jobs[run.ID()] != nil
	})
	b.mu.Lock()
	delete(b.jobs, run.ID())
	b.mu.Unlock()
	connectPoolUnregistered(t, b, "team", 1)

	b.heartbeat(time.Now())
	if v := waitPoolRun(t, run); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, run.Err())
	}
}

// TestInstancesCountEachOthersWorkers connects workers to other instances than
// the one that answers: the catalog counts them, and stops counting a
// worker that disconnects, the workers of an instance that closes, and those
// of a killed instance once its record expires.
func TestInstancesCountEachOthersWorkers(t *testing.T) {
	data, clock := memory.NewData(), newTestClock()
	opts := Options{Shared: true, Now: clock.Now, Paths: PathPolicy{Unrestricted: true}, WorkerToken: poolToken}
	a, kill := openKillable(t, data, opts)
	b := openPlatform(t, data, opts)
	c := openPlatform(t, data, opts)
	connectPoolUnregistered(t, a, "team", 3)
	disconnectSpare := connectPoolUnregistered(t, a, "spare", 1)
	connectPoolUnregistered(t, c, "team", 2)

	ctx := context.Background()
	counted := func(workers, slots int) func() bool {
		return func() bool {
			info, err := b.GetRuntime(ctx, testCaller, "team")
			return err == nil && info.Catalog.Workers == workers && info.Catalog.Slots == slots
		}
	}
	info, err := b.RegisterRuntime(ctx, testCaller, RegisterRuntimeRequest{
		Name: "team", Image: "registry.example/linea-runtime:team", Hello: poolHello(t, a, "team")})
	if err != nil {
		t.Fatal(err)
	}
	if info.Catalog.Confirmed.IsZero() {
		t.Error("a runtime whose workers are connected to other instances is unconfirmed")
	}
	waitFor(t, "three workers with six slots", counted(3, 6))
	disconnectSpare()
	waitFor(t, "the spare worker to drop out", counted(2, 5))
	c.Close()
	waitFor(t, "the closed instance's worker to drop out", counted(1, 3))
	kill()
	if !counted(1, 3)() {
		t.Fatal("the killed instance's worker dropped out before its record expired")
	}
	clock.Advance(DefaultLease + time.Second)
	if !counted(0, 0)() {
		t.Fatal("the killed instance's expired record still counts")
	}
}

// TestASignInCompletesOnAnotherInstance: the provider's callback may reach
// any instance behind the load balancer.
func TestASignInCompletesOnAnotherInstance(t *testing.T) {
	idp := oidctest.New(t)
	data := memory.NewData()
	opts := OIDCOptions{Issuer: idp.Issuer, ClientID: "web", ClientSecret: "web-secret", RedirectURL: "https://debugger.example.com/auth/callback", InsecureCookie: true}
	var instances [2]*OIDC
	for i := range instances {
		o, err := openPlatform(t, data, Options{Shared: true}).OIDC(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		instances[i] = o
	}
	cb := signInAcross(t, instances[0], instances[1], "/runs")
	if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/runs" || cookieNamed(cb, insecureSessionCookie) == nil {
		t.Fatalf("callback on the other instance = %d %s", cb.Code, cb.Header().Get("Location"))
	}
}
