package pg

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// testClock is the clock the instances' leases are measured on.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestPlatformRequeuesARunItsInstanceAbandoned runs the platform on
// PostgreSQL with fake workers: one instance dies while executing a run, and
// another, once the lease ends, requeues the run and executes it from its
// bundle.
func TestPlatformRequeuesARunItsInstanceAbandoned(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	clock := &testClock{now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	caller := platform.Local("test")
	const lease = 300 * time.Millisecond

	dead, err := Open(ctx, f.config(artifact.Limits{}))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 1)
	first, err := platform.Open(dead, platform.Options{Now: clock.Now, Lease: lease, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.RunID
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	run, err := first.SubmitRun(ctx, caller, platform.RunRequest{Mode: platform.ModeQuick,
		Config: platform.Source{Text: "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"},
		Quick:  &platform.QuickInput{Fixture: platform.Source{Text: `{"resourceSpans":[]}`}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first instance never started the run")
	}
	dead.Close() // the instance's process dies: it renews and publishes nothing more

	executed := make(chan engine.Request, 1)
	second, err := platform.Open(f.open(t, artifact.Limits{}), platform.Options{Now: clock.Now, Lease: lease, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		executed <- req
		return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: "completed"}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if v, err := second.GetRun(ctx, caller, run.ID); err != nil || v.State != platform.StateRunning || v.Attempt != 1 {
		t.Fatalf("recovered run = %+v, %v", v, err)
	}
	clock.advance(time.Minute)
	select {
	case req := <-executed:
		if req.RunID != run.ID || !req.Prepared() {
			t.Fatalf("executed %q, prepared %v", req.RunID, req.Prepared())
		}
	case <-ctx.Done():
		t.Fatal("second instance never requeued the run")
	}
	if _, err := second.GetRunReport(ctx, caller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if v, err := second.GetRun(ctx, caller, run.ID); err != nil || v.State != platform.StateDone || v.Attempt != 2 {
		t.Fatalf("finished run = %+v, %v", v, err)
	}
}
