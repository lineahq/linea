// Package platform owns transport-neutral application workflows around the
// verification engine. Workspaces persist sources, input bundles and run evidence.
package platform

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// State is the platform run lifecycle; adapters map it to
// their own vocabulary. Done means a report exists, not that execution or
// assertions passed. Assigned is held by a worker that has not started it;
// Lost is a run whose worker stopped renewing its lease on its last attempt.
type State string

const (
	StateQueued    State = "queued"
	StateAssigned  State = "assigned"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateError     State = "error"
	StateCancelled State = "cancelled"
	StateLost      State = "lost"
)

// Terminal reports whether a run in state s has ended.
func (s State) Terminal() bool {
	return s == StateDone || s == StateError || s == StateCancelled || s == StateLost
}

// InputMode distinguishes the two first-class input workflows, independently
// of runtime's instrumented/reference execution mode.
type InputMode string

const (
	ModeQuick    InputMode = "quick"
	ModeScenario InputMode = "scenario"
)

// Run is a point-in-time view. Reports and the telemetry they contain are
// read-only; callers must not mutate them. Caller is zero for runs recorded
// before callers were stored.
type Run struct {
	ID          string
	Project     string
	Mode        InputMode
	Caller      Actor
	State       State
	Error       string
	Created     time.Time
	Started     *time.Time
	Finished    *time.Time
	Report      *engine.Report
	Interrupted bool
	Bundle      *engine.BundleInfo
	// Attempt counts the run's assignments to a worker; zero before the
	// first. A run is requeued at most once after its worker is lost.
	Attempt int
	// Listing is set by ListRuns for a run with a report.
	Listing *RunListing
	// Kept protects the run from retention and pruning (KeepRun).
	Kept bool
}

// End is the terminal observation used by run subscriptions.
type End struct {
	State  State
	Status runresult.Status
	Error  string
}

// Handle observes and cancels a run independently of any client connection.
type Handle struct {
	mu       sync.Mutex
	id       string
	project  string
	mode     InputMode
	caller   Actor
	state    State
	err      error
	created  time.Time
	started  time.Time
	finished time.Time
	// events are the recording attempt's: a new attempt drops those of the
	// one before, since the engine numbers each attempt's events from 1.
	events    []instrument.Event
	recording int
	// observer, when set, also receives every recorded event.
	observer    func(instrument.Event)
	report      *engine.Report
	changed     chan struct{}
	done        chan struct{}
	cancel      context.CancelCauseFunc
	interrupted bool
	attempt     int
	kept        bool
	generation  uint64
	// shared is set when other instances publish the run too (Options.Shared):
	// a publication they superseded leaves the handle unfinished.
	shared bool
	// ordinal is the run's admission order in the repository; zero without
	// one. stored is set while the repository holds the run's terminal
	// state, so a cache may drop the handle and read it again.
	ordinal uint64
	stored  bool
	// used is when the service last read the run (Service.uses), under the
	// service's mu.
	used uint64
	// requestCancel marks the run in the queue; held reports that a worker
	// holds it, so the worker, not the caller, ends it. Called with mu held.
	requestCancel func() (held bool, err error)
	// ended, when set, is called once the run ends, with mu held.
	ended func()
	// runtimeKey is the run's runtime in the queue (see runtimeKey).
	runtimeKey string
	// binary pins the run of a runtime built on the server to its binary,
	// recorded at admission.
	binary      *artifact.RunBinary
	persist     func(bool) error // called with mu held; true includes terminal evidence
	bundle      []byte
	bundleInfo  *engine.BundleInfo
	evaluations []retainedEvaluation // private source and result bytes; evicted with this run
	// runLog receives the Collector log while the run executes; log and
	// logTruncated keep its tail once a failed or partial run ends.
	runLog       *engine.LogTail
	log          string
	logTruncated bool
}

// ID is the stable identity assigned at submission.
func (r *Handle) ID() string { return r.id }

func (r *Handle) terminalLocked() bool { return r.state.Terminal() }

func (r *Handle) notifyLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Handle) finishLocked(state State, report *engine.Report, err error) error {
	priorState, priorReport, priorErr, priorFinished := r.state, r.report, r.err, r.finished
	priorLog, priorTruncated, priorRunLog := r.log, r.logTruncated, r.runLog
	r.state, r.report, r.err = state, report, err
	r.finished = time.Now().UTC()
	if r.runLog != nil && keepsLog(state, report) {
		r.log, r.logTruncated = r.runLog.Tail()
	}
	r.runLog = nil
	var publicationErr error
	if r.persist != nil {
		if err := r.persist(true); r.shared && errors.Is(err, artifact.ErrConflict) {
			// Another instance published the run first: the handle keeps
			// what it was and catches up on what that instance wrote.
			r.state, r.report, r.err, r.finished = priorState, priorReport, priorErr, priorFinished
			r.log, r.logTruncated, r.runLog = priorLog, priorTruncated, priorRunLog
			return persistenceError(err)
		} else if err != nil {
			publicationErr = persistenceError(err)
			r.state, r.report, r.err = StateError, nil, publicationErr
			// Best effort to record the failure without evidence (for example an
			// oversized report). An uncertain repository refuses this write.
			_ = r.persist(false)
		}
	}
	if r.ended != nil {
		r.ended()
	}
	r.notifyLocked()
	close(r.done)
	return publicationErr
}

// View includes snapshot stores only when full is true. Neither view changes
// the retained canonical report.
func (r *Handle) View(full bool) Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := Run{ID: r.id, Project: r.project, Mode: r.mode, Caller: r.caller, State: r.state, Created: r.created, Report: r.report, Interrupted: r.interrupted, Attempt: r.attempt, Kept: r.kept}
	if r.bundleInfo != nil {
		info := *r.bundleInfo
		v.Bundle = &info
	}
	if r.err != nil {
		v.Error = r.err.Error()
	}
	if !r.started.IsZero() {
		t := r.started
		v.Started = &t
	}
	if !r.finished.IsZero() {
		t := r.finished
		v.Finished = &t
	}
	if r.report != nil && !full {
		v.Report = withoutSnapshots(r.report)
	}
	return v
}

func withoutSnapshots(rep *engine.Report) *engine.Report {
	c := *rep
	if c.Result != nil {
		res := *c.Result
		res.Snapshots = nil
		c.Result = &res
	}
	if c.Reference != nil {
		res := *c.Reference
		res.Snapshots = nil
		c.Reference = &res
	}
	return &c
}

// Since returns the recorded events after cursor as RunWatch does, without
// the outcome. Event data is read-only. Checking and subscribing are atomic,
// so a caller cannot miss a transition between reading and waiting.
func (r *Handle) Since(cursor EventCursor) RunWatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := RunWatch{Attempt: r.recording, Terminal: r.terminalLocked(), Changed: r.changed}
	seq := cursor.Seq
	if w.Restarted = cursor.Attempt != 0 && cursor.Attempt != r.recording; w.Restarted {
		seq = 0
	}
	for _, e := range r.events {
		if e.Seq > seq {
			w.Events = append(w.Events, e)
		}
	}
	return w
}

// recordLocked starts recording attempt: the earlier attempt's events and
// log are dropped, and what its Collector still emits is ignored. It
// returns the attempt's observer and log.
func (r *Handle) recordLocked(attempt int) (func(instrument.Event), *engine.LogTail) {
	r.events, r.recording = nil, attempt
	log := &engine.LogTail{}
	r.runLog = log
	observer := r.observer
	return func(e instrument.Event) {
		r.mu.Lock()
		if r.recording != attempt || r.terminalLocked() {
			r.mu.Unlock()
			return
		}
		r.events = append(r.events, e)
		r.notifyLocked()
		r.mu.Unlock()
		if observer != nil {
			observer(e)
		}
	}, log
}

// End returns the current lifecycle and execution outcomes separately.
func (r *Handle) End() End {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := End{State: r.state}
	if r.err != nil {
		e.Error = r.err.Error()
	}
	if r.report != nil && r.report.Result != nil {
		e.Status = r.report.Result.Status
	}
	return e
}

// Wait stops observing when ctx ends; it never cancels the run.
func (r *Handle) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return nil
	default:
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Err is the engine, persistence or recovery error. Read after Wait succeeds.
func (r *Handle) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Cancel immediately terminates a queued run or requests cancellation of a
// running Collector. A started run can still publish a partial report. Repeated
// cancellation and cancellation after completion are safe. Queued cancellation
// returns a persistence error if it could not publish its terminal state.
func (r *Handle) Cancel() error {
	return r.cancelWithCause(context.Canceled)
}

func (r *Handle) cancelWithCause(cause error) error {
	_, err := r.cancelRequest(cause)
	return err
}

// cancelRequest reports whether the run was still unfinished, so the request
// took effect.
func (r *Handle) cancelRequest(cause error) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalLocked() {
		return false, nil
	}
	held := r.state != StateQueued
	if r.requestCancel != nil {
		h, err := r.requestCancel()
		if err != nil {
			// The queue is unreachable: stop what this process executes, and
			// leave a queued run for the queue to end.
			if held {
				r.cancel(cause)
			}
			return false, persistenceError(err)
		}
		held = h
	}
	r.cancel(cause)
	if !held {
		return true, r.finishLocked(StateCancelled, nil, nil)
	}
	return true, nil
}
