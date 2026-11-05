package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// Scheduler tests use an engine boundary that blocks on explicit channels, not
// Collector timing. Real quick/scenario execution is covered by integration tests.
func unprepared(_ context.Context, req engine.Request) (engine.Request, error) {
	return req, nil
}

func request(label string) engine.Request {
	return engine.Request{ConfigYAML: "config", FixtureJSON: label}
}

func report(req engine.Request, status runresult.Status) *engine.Report {
	return &engine.Report{Schema: engine.ReportSchemaVersion, Result: &runresult.Result{RunID: req.RunID, Status: status}}
}

func waitRun(t *testing.T, r *Handle) Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return r.View(true)
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func submit(t *testing.T, s *Service, req engine.Request) *Handle {
	t.Helper()
	r, err := s.Submit(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunsExecuteInSubmissionOrderAcrossModes(t *testing.T) {
	started, release := make(chan engine.Request, 4), make(chan struct{}, 4)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	first := submit(t, s, request("first"))
	if got := receive(t, started); got.RunID != first.ID() {
		t.Fatalf("started %s, want %s", got.RunID, first.ID())
	}
	scenario := engine.Request{ConfigYAML: "config", ScenarioYAML: "scenario"}
	second := submit(t, s, scenario)
	third := submit(t, s, request("third"))
	if second.View(false).Mode != ModeScenario || third.View(false).Mode != ModeQuick {
		t.Fatal("input modes were not preserved")
	}
	if got := second.View(false); got.State != StateQueued || got.Started != nil {
		t.Fatalf("queued run = %+v", got)
	}
	release <- struct{}{}
	if got := receive(t, started); got.RunID != second.ID() {
		t.Fatalf("started %s, want second run %s", got.RunID, second.ID())
	}
	release <- struct{}{}
	if got := receive(t, started); got.RunID != third.ID() {
		t.Fatalf("started %s, want third run %s", got.RunID, third.ID())
	}
	release <- struct{}{}
	for _, r := range []*Handle{first, second, third} {
		v := waitRun(t, r)
		if v.State != StateDone || v.Report.Result.RunID != r.ID() {
			t.Fatalf("finished run = %+v", v)
		}
	}
}

func TestCancelledQueuedRunNeverExecutesAndFreesAdmission(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{}, 3)
	s := New(Options{QueueCapacity: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.FixtureJSON
		select {
		case <-release:
		case <-ctx.Done():
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	first := submit(t, s, request("first"))
	receive(t, started)
	cancelled := submit(t, s, request("cancelled"))
	if _, err := s.Submit(request("overflow")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("admission error = %v", err)
	}
	cancelled.Cancel()
	cancelled.Cancel()
	if got := waitRun(t, cancelled); got.State != StateCancelled || got.Started != nil || got.Report != nil {
		t.Fatalf("cancelled queued run = %+v", got)
	}
	last := submit(t, s, request("last"))
	release <- struct{}{}
	if got := receive(t, started); got != "last" {
		t.Fatalf("executed %q instead of skipping cancelled work", got)
	}
	release <- struct{}{}
	waitRun(t, first)
	waitRun(t, last)
	if len(s.List()) != 3 {
		t.Fatal("rejected submission created history")
	}
}

func TestObserverCancellationDoesNotCancelRun(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	r := submit(t, s, request("quick"))
	receive(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v", err)
	}
	if r.View(false).State != StateRunning {
		t.Fatal("disconnecting a watcher cancelled the run")
	}
	close(release)
	if v := waitRun(t, r); v.Report.Result.Status != runresult.StatusCompleted {
		t.Fatalf("run = %+v", v)
	}
}

func TestForegroundDeadlinePreservesPartialReport(t *testing.T) {
	started := make(chan context.Context, 1)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- ctx
		<-ctx.Done()
		status := runresult.StatusCancelled
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			status = runresult.StatusPartial
		}
		return report(req, status), nil
	}})
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	type result struct {
		report *engine.Report
		err    error
	}
	finished := make(chan result, 1)
	// Use a cancel cause to deterministically trigger deadline semantics without
	// timing a sleeping Collector; the actual deadline must also reach the worker.
	caller, stop := context.WithCancelCause(ctx)
	go func() {
		rep, err := s.Execute(caller, request("deadline"))
		finished <- result{rep, err}
	}()
	workerCtx := receive(t, started)
	wantDeadline, _ := caller.Deadline()
	if got, ok := workerCtx.Deadline(); !ok || !got.Equal(wantDeadline) {
		t.Fatalf("worker deadline = %v, %v; want %v", got, ok, wantDeadline)
	}
	stop(context.DeadlineExceeded)
	got := receive(t, finished)
	if got.err != nil || got.report == nil || got.report.Result.Status != runresult.StatusPartial {
		t.Fatalf("foreground result = %+v", got)
	}
}

func TestCloseCancelsActiveAndQueuedRunsAndRejectsSubmission(t *testing.T) {
	started := make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		close(started)
		<-ctx.Done()
		return report(req, runresult.StatusCancelled), nil
	}})
	t.Cleanup(s.Close)
	active := submit(t, s, request("active"))
	receive(t, started)
	queued := submit(t, s, request("queued"))
	var closers sync.WaitGroup
	for range 4 {
		closers.Go(s.Close)
	}
	closers.Wait()
	if v := waitRun(t, active); v.State != StateDone || v.Report.Result.Status != runresult.StatusCancelled {
		t.Fatalf("started cancellation lost report: %+v", v)
	}
	if v := waitRun(t, queued); v.State != StateCancelled || v.Started != nil {
		t.Fatalf("queued shutdown = %+v", v)
	}
	if _, err := s.Submit(request("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("submission after shutdown = %v", err)
	}
}

func TestConcurrentSubmissionAndClose(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	var workers sync.WaitGroup
	for i := range 40 {
		workers.Go(func() {
			r, err := s.Submit(request(fmt.Sprint(i)))
			if err != nil && !errors.Is(err, ErrClosed) && !errors.Is(err, ErrQueueFull) {
				t.Errorf("submit = %v", err)
			}
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := r.Wait(ctx); err != nil {
					t.Errorf("accepted run did not finish: %v", err)
				}
			}
		})
	}
	workers.Go(s.Close)
	workers.Wait()
}

func TestHistoryEvictsOnlyTerminalRunsAndRetainsHeldHandles(t *testing.T) {
	s := New(Options{HistoryLimit: 2, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	var runs []*Handle
	for i := range 5 {
		r := submit(t, s, request(fmt.Sprint(i)))
		waitRun(t, r)
		runs = append(runs, r)
	}
	// Completion is published before eviction; acquire the worker's next job to
	// establish that the preceding eviction completed, without polling/sleeping.
	barrier := submit(t, s, request("barrier"))
	waitRun(t, barrier)
	if _, err := s.Get(runs[0].ID()); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("old run lookup = %v", err)
	}
	if runs[0].View(true).Report == nil {
		t.Fatal("eviction invalidated a handle already being inspected")
	}
	list := s.List()
	if len(list) != 2 || list[0].ID != barrier.ID() || list[0].Report != nil {
		t.Fatalf("history = %+v", list)
	}
}

func TestEventResumptionAndViewsPreserveCanonicalSnapshots(t *testing.T) {
	started, release := make(chan engine.Request, 1), make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
		}
		rep := report(req, runresult.StatusCompleted)
		rep.Result.Snapshots = instrument.Snapshots{"snapshot": []byte(`{}`)}
		rep.Reference = &runresult.Result{Snapshots: instrument.Snapshots{"reference": []byte(`{}`)}}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	r := submit(t, s, request("events"))
	req := receive(t, started)
	changed := r.Since(EventCursor{}).Changed
	for i := 1; i <= 3; i++ {
		req.Observer(instrument.Event{RunID: r.ID(), Seq: i})
	}
	receive(t, changed)
	w := r.Since(EventCursor{Attempt: 1, Seq: 1})
	events, done := w.Events, w.Terminal
	if done || w.Restarted || len(events) != 2 || !slices.Equal([]int{events[0].Seq, events[1].Seq}, []int{2, 3}) {
		t.Fatalf("resumed events = %+v, terminal = %v", events, done)
	}
	close(release)
	waitRun(t, r)
	if v := r.View(false); v.Report.Result.Snapshots != nil || v.Report.Reference.Snapshots != nil {
		t.Fatal("ordinary view contains snapshots")
	}
	if v := r.View(true); len(v.Report.Result.Snapshots) != 1 || len(v.Report.Reference.Snapshots) != 1 {
		t.Fatal("ordinary view modified canonical evidence")
	}
	if e := r.End(); e.State != StateDone || e.Status != runresult.StatusCompleted {
		t.Fatalf("end = %+v", e)
	}
}

func TestRejectedRequestsAndEngineErrors(t *testing.T) {
	wantErr := errors.New("engine setup failed")
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return nil, wantErr
	}})
	t.Cleanup(s.Close)
	if _, err := s.Submit(engine.Request{}); !errors.Is(err, engine.ErrInvalidRequest) || len(s.List()) != 0 {
		t.Fatalf("invalid admission = %v", err)
	}
	if _, err := s.Execute(context.Background(), request("error")); !errors.Is(err, wantErr) {
		t.Fatalf("engine error identity lost: %v", err)
	}
	v := s.List()[0]
	if v.State != StateError || v.Error != wantErr.Error() || v.Finished == nil {
		t.Fatalf("failed job = %+v", v)
	}
}

func TestExpiredQueuedRunNeverStarts(t *testing.T) {
	started, release := make(chan string, 3), make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.FixtureJSON
		if req.FixtureJSON == "active" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	submit(t, s, request("active"))
	receive(t, started)
	// An already expired deadline must never reach the engine, even when
	// submitted internally before the foreground waiter observes cancellation.
	r, err := s.submit(context.Background(), testCaller, ModeQuick, request("expired"), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	barrier := submit(t, s, request("barrier"))
	close(release)
	if got := receive(t, started); got != "barrier" {
		t.Fatalf("expired request executed: %s", got)
	}
	if v := waitRun(t, r); v.State != StateCancelled || v.Started != nil {
		t.Fatalf("queued cancellation = %+v", v)
	}
	waitRun(t, barrier)
}

func TestHistoryRetainsActiveAndQueuedRunsBeyondTerminalLimit(t *testing.T) {
	started := make(chan string, 3)
	s := New(Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.FixtureJSON
		if req.FixtureJSON == "active" {
			<-ctx.Done()
		}
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	waitRun(t, submit(t, s, request("old")))
	receive(t, started)
	active := submit(t, s, request("active"))
	receive(t, started)
	queued := submit(t, s, request("queued"))
	if len(s.List()) != 3 {
		t.Fatal("history limit evicted unfinished work")
	}
	for _, r := range []*Handle{active, queued} {
		if _, err := s.Get(r.ID()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyEngineResultIsAnError(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		return nil, nil
	}})
	t.Cleanup(s.Close)
	r := submit(t, s, request("empty"))
	if v := waitRun(t, r); v.State != StateError || v.Report != nil || v.Error == "" {
		t.Fatalf("empty result falsely completed: %+v", v)
	}
}

// TestANewAttemptReplacesTheEventsOfTheOneBefore records two attempts of a
// requeued run: the second drops the first's events, what the first's
// Collector still emits is ignored, and a cursor in the first restarts at
// the second's first event.
func TestANewAttemptReplacesTheEventsOfTheOneBefore(t *testing.T) {
	r := &Handle{id: "run", state: StateRunning, changed: make(chan struct{}), done: make(chan struct{})}
	r.mu.Lock()
	first, _ := r.recordLocked(1)
	r.mu.Unlock()
	for seq := 1; seq <= 3; seq++ {
		first(instrument.Event{Seq: seq})
	}
	r.mu.Lock()
	second, _ := r.recordLocked(2)
	r.mu.Unlock()
	first(instrument.Event{Seq: 4})
	second(instrument.Event{Seq: 1})
	for _, tc := range []struct {
		name      string
		cursor    EventCursor
		restarted bool
		seqs      []int
	}{
		{"from the start", EventCursor{}, false, []int{1}},
		{"in the first attempt", EventCursor{Attempt: 1, Seq: 3}, true, []int{1}},
		{"in the second attempt", EventCursor{Attempt: 2, Seq: 1}, false, nil},
		{"in an attempt not begun", EventCursor{Attempt: 3, Seq: 1}, true, []int{1}},
	} {
		w := r.Since(tc.cursor)
		var seqs []int
		for _, e := range w.Events {
			seqs = append(seqs, e.Seq)
		}
		if w.Attempt != 2 || w.Restarted != tc.restarted || !slices.Equal(seqs, tc.seqs) {
			t.Errorf("%s: attempt %d, restarted %v, seqs %v", tc.name, w.Attempt, w.Restarted, seqs)
		}
	}
}
