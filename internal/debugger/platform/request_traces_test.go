package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// spanFeed records spans and announces each started span's name, so a test
// waits for a span to open without polling.
type spanFeed struct {
	*tracetest.SpanRecorder
	started chan string
}

func (f spanFeed) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	f.SpanRecorder.OnStart(ctx, s)
	select {
	case f.started <- s.Name():
	default:
	}
}

// requestTracing returns a feed, its provider and a request span in a context
// the test ends before reading the recorded spans.
func requestTracing(t *testing.T) (spanFeed, trace.TracerProvider, context.Context, trace.Span) {
	t.Helper()
	feed := spanFeed{SpanRecorder: tracetest.NewSpanRecorder(), started: make(chan string, 256)}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(feed))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "request")
	return feed, provider, ctx, parent
}

// awaitStarted blocks until a span named name starts.
func awaitStarted(t *testing.T, feed spanFeed, name string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case got := <-feed.started:
			if got == name {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for span %q", name)
		}
	}
}

// endedNamed returns the ended spans named name.
func endedNamed(feed spanFeed, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, sp := range feed.Ended() {
		if sp.Name() == name {
			out = append(out, sp)
		}
	}
	return out
}

// wantAttributes fails unless sp holds exactly the attributes in want.
func wantAttributes(t *testing.T, sp sdktrace.ReadOnlySpan, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	for _, kv := range sp.Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if len(got) != len(want) {
		t.Errorf("%s attributes = %v, want %v", sp.Name(), got, want)
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s %s = %q, want %q", sp.Name(), k, got[k], v)
		}
	}
}

// wantChildOf fails unless sp is an internal child of parent that ended
// without error.
func wantChildOf(t *testing.T, sp sdktrace.ReadOnlySpan, parent trace.Span) {
	t.Helper()
	if sp.Parent().SpanID() != parent.SpanContext().SpanID() || sp.Parent().TraceID() != parent.SpanContext().TraceID() {
		t.Errorf("%s parent = %v, want the request span", sp.Name(), sp.Parent())
	}
	if sp.Status().Code == codes.Error || sp.SpanKind() != trace.SpanKindInternal {
		t.Errorf("%s status %+v kind %v", sp.Name(), sp.Status(), sp.SpanKind())
	}
}

// wantNoPrivateText fails if any recorded span holds configuration text or
// the name of a runtime.
func wantNoPrivateText(t *testing.T, feed spanFeed) {
	t.Helper()
	for _, sp := range feed.Ended() {
		var b strings.Builder
		fmt.Fprintf(&b, "%s|%s", sp.Status().Code, sp.Status().Description)
		for _, kv := range sp.Attributes() {
			fmt.Fprintf(&b, "|%s=%s", kv.Key, kv.Value.Emit())
		}
		for _, e := range sp.Events() {
			fmt.Fprintf(&b, "|%s", e.Name)
			for _, kv := range e.Attributes {
				fmt.Fprintf(&b, "|%s=%s", kv.Key, kv.Value.Emit())
			}
		}
		for _, private := range []string{"receivers", "exporters", "example.invalid", BuiltInRuntime} {
			if strings.Contains(b.String(), private) {
				t.Errorf("span %s holds %q: %s", sp.Name(), private, b.String())
			}
		}
	}
}

// TestRunRequestSpansCoverPreparationAndWaiting: submitting a run under a
// request span adds a linea.run.prepare child, waiting for its report a
// linea.run.wait child with the run's ID, and waiting for a run that already
// ended adds none.
func TestRunRequestSpansCoverPreparationAndWaiting(t *testing.T) {
	feed, provider, ctx, parent := requestTracing(t)
	release := make(chan struct{})
	s := New(Options{TracerProvider: provider, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)

	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig},
		Quick: &QuickInput{Fixture: Source{Text: `{"resourceSpans":[]}`}}})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		rep *Report
		err error
	}
	waited := make(chan result, 1)
	go func() {
		rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
		waited <- result{rep, err}
	}()
	awaitStarted(t, feed, spanRunWait)
	close(release)
	if got := receive(t, waited); got.err != nil || got.rep == nil {
		t.Fatalf("report = %+v, %v", got.rep, got.err)
	}
	// The run ended: waiting again returns at once and records nothing.
	if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	parent.End()

	prepares, waits := endedNamed(feed, "linea.run.prepare"), endedNamed(feed, "linea.run.wait")
	if len(prepares) != 1 || len(waits) != 1 {
		t.Fatalf("prepare spans %d, wait spans %d, want one each", len(prepares), len(waits))
	}
	wantChildOf(t, prepares[0], parent)
	wantAttributes(t, prepares[0], map[string]string{"linea.runtime.remote": "false"})
	wantChildOf(t, waits[0], parent)
	wantAttributes(t, waits[0], map[string]string{"linea.run.id": run.ID})
	wantNoPrivateText(t, feed)
}

// TestExecuteRunWaitsUnderASpan: ExecuteRun waits through the same span.
func TestExecuteRunWaitsUnderASpan(t *testing.T) {
	feed, provider, ctx, parent := requestTracing(t)
	release := make(chan struct{})
	s := New(Options{TracerProvider: provider, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	done := make(chan error, 1)
	go func() {
		_, err := s.ExecuteRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig},
			Quick: &QuickInput{Fixture: Source{Text: "{}"}}})
		done <- err
	}()
	awaitStarted(t, feed, spanRunWait)
	close(release)
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	parent.End()
	waits := endedNamed(feed, "linea.run.wait")
	if len(waits) != 1 {
		t.Fatalf("wait spans = %d, want 1", len(waits))
	}
	wantChildOf(t, waits[0], parent)
	if id := attrOf(waits[0], "linea.run.id"); id == "" {
		t.Errorf("wait span has no run ID: %v", waits[0].Attributes())
	}
}

// TestSuiteWaitIsOneSpanOverThePendingRuns: waiting for a suite's report is
// one linea.suite.wait span with the suite's ID and the number of runs still
// pending; waiting for a suite that ended records none.
func TestSuiteWaitIsOneSpanOverThePendingRuns(t *testing.T) {
	feed, provider, ctx, parent := requestTracing(t)
	started, release := make(chan engine.Request, 4), make(chan struct{})
	s := New(Options{TracerProvider: provider, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
		}
		rep := report(req, runresult.StatusCompleted)
		rep.Scenario = req.Scenario
		rep.Assertions = &assertion.Report{Outcome: assertion.Pass}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("first", "second"))
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		rep *SuiteReport
		err error
	}
	waited := make(chan result, 1)
	go func() {
		rep, err := s.GetSuiteReport(ctx, testCaller, suite.ID, true)
		waited <- result{rep, err}
	}()
	awaitStarted(t, feed, spanSuiteWait)
	close(release)
	if got := receive(t, waited); got.err != nil || got.rep == nil {
		t.Fatalf("suite report = %+v, %v", got.rep, got.err)
	}
	if _, err := s.GetSuiteReport(ctx, testCaller, suite.ID, true); err != nil {
		t.Fatal(err)
	}
	parent.End()

	waits := endedNamed(feed, "linea.suite.wait")
	if len(waits) != 1 {
		t.Fatalf("suite wait spans = %d, want 1", len(waits))
	}
	wantChildOf(t, waits[0], parent)
	wantAttributes(t, waits[0], map[string]string{"linea.suite.id": suite.ID, "linea.suite.runs": "2"})
	if n := len(endedNamed(feed, "linea.run.wait")); n != 0 {
		t.Errorf("a suite wait recorded %d run wait spans", n)
	}
	wantNoPrivateText(t, feed)
}

// TestWaitSpansNeedARunStillPending: waitRun and waitSuite record a span only
// for a run that has not ended, skip members without a run, and end the span
// when the wait is cancelled.
func TestWaitSpansNeedARunStillPending(t *testing.T) {
	ended := func() *Handle {
		h := &Handle{id: "ended", done: make(chan struct{})}
		close(h.done)
		return h
	}
	pending := func(id string) *Handle {
		return &Handle{id: id, done: make(chan struct{}), changed: make(chan struct{})}
	}
	t.Run("a run that ended", func(t *testing.T) {
		feed, provider, ctx, parent := requestTracing(t)
		s := &Service{}
		s.newServerTraces(provider)
		if err := s.waitRun(ctx, ended()); err != nil {
			t.Fatal(err)
		}
		parent.End()
		if n := len(endedNamed(feed, spanRunWait)); n != 0 {
			t.Errorf("wait spans = %d, want none", n)
		}
	})
	t.Run("a suite of ended and absent runs", func(t *testing.T) {
		feed, provider, ctx, parent := requestTracing(t)
		s := &Service{}
		s.newServerTraces(provider)
		if err := s.waitSuite(ctx, "suite-1", []*Handle{nil, ended(), nil}); err != nil {
			t.Fatal(err)
		}
		parent.End()
		if n := len(endedNamed(feed, spanSuiteWait)); n != 0 {
			t.Errorf("suite wait spans = %d, want none", n)
		}
	})
	t.Run("a cancelled wait", func(t *testing.T) {
		feed, provider, ctx, parent := requestTracing(t)
		s := &Service{}
		s.newServerTraces(provider)
		waitCtx, cancel := context.WithCancel(ctx)
		cancel()
		if err := s.waitRun(waitCtx, pending("run-1")); err == nil {
			t.Fatal("waitRun on a cancelled context succeeded")
		}
		if err := s.waitSuite(waitCtx, "suite-1", []*Handle{nil, pending("run-2"), pending("run-3")}); err == nil {
			t.Fatal("waitSuite on a cancelled context succeeded")
		}
		parent.End()
		runs, suites := endedNamed(feed, spanRunWait), endedNamed(feed, spanSuiteWait)
		if len(runs) != 1 || len(suites) != 1 {
			t.Fatalf("ended wait spans: %d run, %d suite, want one each", len(runs), len(suites))
		}
		wantAttributes(t, runs[0], map[string]string{"linea.run.id": "run-1"})
		wantAttributes(t, suites[0], map[string]string{"linea.suite.id": "suite-1", "linea.suite.runs": "2"})
	})
}

// TestInspectionSpanNamesTheWorkNotTheRuntime: inspecting a configuration
// under a request span adds one linea.inspect child saying only whether the
// work ran on a pooled worker.
func TestInspectionSpanNamesTheWorkNotTheRuntime(t *testing.T) {
	feed, provider, ctx, parent := requestTracing(t)
	s := New(Options{TracerProvider: provider})
	t.Cleanup(s.Close)
	if _, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Text: bundleConfig}, Runtime: BuiltInRuntime}); err != nil {
		t.Fatal(err)
	}
	parent.End()
	spans := endedNamed(feed, "linea.inspect")
	if len(spans) != 1 {
		t.Fatalf("inspect spans = %d, want 1", len(spans))
	}
	wantChildOf(t, spans[0], parent)
	wantAttributes(t, spans[0], map[string]string{"linea.runtime.remote": "false"})
	wantNoPrivateText(t, feed)
}

// TestIsPooledNeedsAPooledBinding: only a runtime bound to connected workers
// is remote.
func TestIsPooledNeedsAPooledBinding(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   Runtime
		want bool
	}{
		{"the built-in runtime", nil, false},
		{"a worker the service starts", &unbindOnFailure{b: &boundRuntime{}}, false},
		{"connected workers", &unbindOnFailure{b: &boundRuntime{pooled: true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPooled(tc.rt); got != tc.want {
				t.Errorf("isPooled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRequestScopeToleratesNone: a nil scope, which an operation outside a
// request has, never panics and remembers nothing.
func TestRequestScopeToleratesNone(t *testing.T) {
	var sc *requestScope
	sc.markCaughtUp()
	sc.keep("name", &boundRuntime{})
	if sc.caughtUpOnce() {
		t.Error("a nil scope has caught up")
	}
	if sc.boundRuntime("name") != nil {
		t.Error("a nil scope returned a runtime")
	}
	if scopeOf(context.Background()) != nil {
		t.Error("a context without a scope has one")
	}
}

// TestWithRequestScopeKeepsTheOneItHas: the first call installs a scope, and
// later calls on that context, or one derived from it, keep it.
func TestWithRequestScopeKeepsTheOneItHas(t *testing.T) {
	root := context.Background()
	first := withRequestScope(root)
	sc := scopeOf(first)
	if sc == nil {
		t.Fatal("no scope installed")
	}
	if again := withRequestScope(first); again != first || scopeOf(again) != sc {
		t.Error("a second call replaced the scope")
	}
	type key struct{}
	derived := context.WithValue(first, key{}, 1)
	if scopeOf(withRequestScope(derived)) != sc {
		t.Error("a derived context lost the scope")
	}
	if scopeOf(withRequestScope(root)) == sc {
		t.Error("two requests share a scope")
	}
}

// TestRequestScopeRemembersCatchUpAndBindings covers keep and boundRuntime.
func TestRequestScopeRemembersCatchUpAndBindings(t *testing.T) {
	sc := &requestScope{}
	if sc.caughtUpOnce() {
		t.Error("a new scope has caught up")
	}
	sc.markCaughtUp()
	if !sc.caughtUpOnce() {
		t.Error("the scope forgot its catch-up")
	}
	a, b := &boundRuntime{pooled: true}, &boundRuntime{}
	sc.keep("a", a)
	sc.keep("b", b)
	sc.keep("nil", nil)
	for _, tc := range []struct {
		name string
		want *boundRuntime
	}{{"a", a}, {"b", b}, {"nil", nil}, {"other", nil}} {
		if got := sc.boundRuntime(tc.name); got != tc.want {
			t.Errorf("boundRuntime(%q) = %p, want %p", tc.name, got, tc.want)
		}
	}
	sc.keep("a", b)
	if sc.boundRuntime("a") != b {
		t.Error("keep did not replace the binding")
	}
}

// TestRequestScopeIsSafeForConcurrentOperations: the operations of one request
// may run on several goroutines.
func TestRequestScopeIsSafeForConcurrentOperations(t *testing.T) {
	sc := &requestScope{}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc.markCaughtUp()
			sc.keep(fmt.Sprint(i%2), &boundRuntime{})
			_ = sc.caughtUpOnce()
			_ = sc.boundRuntime(fmt.Sprint(i % 2))
		}()
	}
	wg.Wait()
	if !sc.caughtUpOnce() || sc.boundRuntime("0") == nil || sc.boundRuntime("1") == nil {
		t.Error("the scope lost an update")
	}
}

// TestBindReusesARuntimeBoundInTheScope: within a request scope a runtime is
// bound once; a binding that failed is not remembered; without a scope every
// call binds.
func TestBindReusesARuntimeBoundInTheScope(t *testing.T) {
	s := New(Options{})
	t.Cleanup(s.Close)
	kept := &boundRuntime{}
	ctx := withRequestScope(context.Background())
	scopeOf(ctx).keep("kept", kept)
	if got, err := s.bind(ctx, "kept"); err != nil || got != kept {
		t.Fatalf("bind in the scope = %p, %v, want the kept binding", got, err)
	}
	// The same name outside the scope is looked up for real, and no runtime
	// has it.
	if _, err := s.bind(context.Background(), "kept"); err == nil {
		t.Fatal("bind outside the scope found a runtime nobody has")
	}
	if _, err := s.bind(ctx, "missing"); err == nil {
		t.Fatal("bind of a missing runtime succeeded")
	}
	if scopeOf(ctx).boundRuntime("missing") != nil {
		t.Error("a failed binding was kept")
	}
}

// countingStore counts the change feed reads whose context holds
// countedRequest, leaving out the service's own background catch-ups, and
// fails them while failing is set.
type countingStore struct {
	*memory.Store
	calls   atomic.Int64
	failing atomic.Bool
}

type countedRequest struct{}

func (c *countingStore) Changes(ctx context.Context, after artifact.ChangeCursor, limit int) ([]artifact.Change, artifact.ChangeCursor, error) {
	if ctx.Value(countedRequest{}) != nil {
		c.calls.Add(1)
		if c.failing.Load() {
			return nil, after, errors.New("change feed unavailable")
		}
	}
	return c.Store.Changes(ctx, after, limit)
}

func openCounting(t *testing.T, opts Options) (*Service, *countingStore) {
	t.Helper()
	store := &countingStore{Store: memory.Open(memory.NewData(), artifact.Limits{})}
	opts.Shared = true
	s, err := Open(store, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, store
}

// TestOperationsOfOneScopeCatchUpOnce: each read of a run begins with a
// catch-up on other instances' writes; under one request scope only the first
// does, and a catch-up that failed is tried again.
func TestOperationsOfOneScopeCatchUpOnce(t *testing.T) {
	s, store := openCounting(t, Options{Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	run := submit(t, s, request("quick"))
	waitRun(t, run)
	counted := context.WithValue(context.Background(), countedRequest{}, true)

	for _, tc := range []struct {
		name  string
		scope bool
		reads int
		want  int64
	}{
		{"without a scope", false, 3, 3},
		{"with a scope", true, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := counted
			if tc.scope {
				ctx = withRequestScope(ctx)
			}
			before := store.calls.Load()
			for range tc.reads {
				if _, err := s.GetRun(ctx, testCaller, run.ID()); err != nil {
					t.Fatal(err)
				}
			}
			if got := store.calls.Load() - before; got != tc.want {
				t.Errorf("%d reads made %d catch-ups, want %d", tc.reads, got, tc.want)
			}
		})
	}
	t.Run("a failed catch-up", func(t *testing.T) {
		ctx := withRequestScope(counted)
		before := store.calls.Load()
		store.failing.Store(true)
		if _, err := s.GetRun(ctx, testCaller, run.ID()); err == nil {
			t.Fatal("a read succeeded while the change feed failed")
		}
		store.failing.Store(false)
		for range 2 {
			if _, err := s.GetRun(ctx, testCaller, run.ID()); err != nil {
				t.Fatal(err)
			}
		}
		if got := store.calls.Load() - before; got != 2 {
			t.Errorf("catch-ups = %d, want the failed one and one retry", got)
		}
	})
}

// TestCompareRunsCatchesUpOncePerRequest: both lookups of a comparison share
// one catch-up.
func TestCompareRunsCatchesUpOncePerRequest(t *testing.T) {
	s, store := openCounting(t, Options{Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	ctx := context.Background()
	var ids [2]string
	for i := range ids {
		run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig},
			Quick: &QuickInput{Fixture: Source{Text: `{"resourceSpans":[]}`}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
			t.Fatal(err)
		}
		ids[i] = run.ID
	}
	counted := context.WithValue(ctx, countedRequest{}, true)
	before := store.calls.Load()
	if _, err := s.CompareRuns(counted, testCaller, CompareRequest{Before: ids[0], After: ids[1]}); err != nil {
		t.Fatal(err)
	}
	if got := store.calls.Load() - before; got != 1 {
		t.Errorf("CompareRuns made %d catch-ups, want 1", got)
	}
}

// TestRequestSpansNeedAParentSpan: work done with no span in its context, such
// as a poll or a caller that does not trace, records none of the request
// spans, though it still runs and waits.
func TestRequestSpansNeedAParentSpan(t *testing.T) {
	feed, provider, _, parent := requestTracing(t)
	parent.End()
	s := New(Options{TracerProvider: provider, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Scenario = req.Scenario
		rep.Assertions = &assertion.Report{Outcome: assertion.Pass}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()

	if _, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Text: bundleConfig}}); err != nil {
		t.Fatal(err)
	}
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig},
		Quick: &QuickInput{Fixture: Source{Text: `{"resourceSpans":[]}`}}})
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil || rep == nil {
		t.Fatalf("report = %+v, %v", rep, err)
	}
	// An unfinished run is waited for with no span too.
	pending := &Handle{id: "run-1", done: make(chan struct{}), changed: make(chan struct{})}
	waitCtx, cancel := context.WithCancel(ctx)
	cancel()
	local := &Service{}
	local.newServerTraces(provider)
	if err := local.waitRun(waitCtx, pending); err == nil {
		t.Fatal("waitRun on a cancelled context succeeded")
	}
	if err := local.waitSuite(waitCtx, "suite-1", []*Handle{pending}); err == nil {
		t.Fatal("waitSuite on a cancelled context succeeded")
	}
	for _, sp := range feed.Ended() {
		switch sp.Name() {
		case spanRunPrepare, spanRunWait, spanSuiteWait, spanInspect:
			t.Errorf("span %s recorded without a parent span", sp.Name())
		}
	}
	for _, name := range []string{spanRunPrepare, spanRunWait, spanSuiteWait, spanInspect} {
		if n := len(endedNamed(feed, name)); n != 0 {
			t.Errorf("%d %s spans", n, name)
		}
	}
}
