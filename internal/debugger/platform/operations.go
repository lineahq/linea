package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// Operations are the single definition of each capability. Each
// takes the caller's context and identity first and returns a typed *Error.
// Operations that start no Collector do not use the execution queue.

var errMode = fmt.Errorf("%w: mode must be quick with a quick input or scenario with a scenario input", engine.ErrInvalidRequest)

// begin starts every operation: the caller must be valid and hold the
// permission the operation declares.
func (s *Service) begin(ctx context.Context, caller Caller, perm authz.Permission) error {
	if err := caller.validate(); err != nil {
		return classify(err, CodeInvalidRequest)
	}
	if err := s.authorize(caller, perm); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return classify(err, CodeCancelled)
	}
	switch perm {
	case authz.RunRead, authz.RunSubmit, authz.RunDelete:
		// Reads and changes of runs and suites see every instance's writes
		// committed before them.
		if err := s.catchUp(ctx); err != nil {
			if ctx.Err() != nil {
				return classify(ctx.Err(), CodeCancelled)
			}
			return artifactError(err)
		}
	}
	return nil
}

// InspectConfiguration validates a configuration with the debugger's Collector
// registry: validity, diagnostics, redacted effective config and topology. An
// invalid configuration is a result, not an error.
func (s *Service) InspectConfiguration(ctx context.Context, caller Caller, req InspectRequest) (_ *Inspection, err error) {
	defer s.audit(&caller, "InspectConfiguration", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.resolveContext(ctx, caller, &req.Config, &req.Scenario); err != nil {
		return nil, err
	}
	name, err := s.projectRuntime(caller.Project, req.Runtime)
	if err != nil {
		return nil, err
	}
	rt, err := s.runtimeFor(ctx, name)
	if err != nil {
		return nil, err
	}
	ins, err := s.inspect(ctx, rt, req.engine())
	if ins != nil {
		ins.Runtime = answeredBy(rt)
	}
	return ins, classify(err, CodeValidationFailed)
}

// inspect performs the host reads here and completes the inspection on the
// runtime, which reads nothing from the host.
func (s *Service) inspect(ctx context.Context, rt Runtime, req engine.InspectRequest) (*engine.Inspection, error) {
	if rt == nil {
		return engine.InspectWithPolicy(ctx, req, s.policy)
	}
	in, done, err := engine.CaptureInspection(ctx, req, s.policyFor(rt))
	if err != nil || done != nil {
		return done, err
	}
	return rt.Inspect(ctx, in)
}

// PreviewTransformation is the OTTL playground: it replays a transform
// processor on a payload without running the Collector. Its output is never
// verified and never a passing test.
func (s *Service) PreviewTransformation(ctx context.Context, caller Caller, req PreviewRequest) (_ *PreviewResult, err error) {
	defer s.audit(&caller, "PreviewTransformation", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.resolveContext(ctx, caller, &req.Config, &req.Scenario); err != nil {
		return nil, err
	}
	name, err := s.projectRuntime(caller.Project, req.Runtime)
	if err != nil {
		return nil, err
	}
	rt, err := s.runtimeFor(ctx, name)
	if err != nil {
		return nil, err
	}
	ev, err := s.evaluateOTTL(ctx, rt, req.engine())
	if ev != nil {
		ev.Runtime = answeredBy(rt)
	}
	return ev, classify(err, CodeValidationFailed)
}

func (s *Service) evaluateOTTL(ctx context.Context, rt Runtime, req engine.OTTLRequest) (*engine.OTTLEvaluation, error) {
	if rt == nil {
		return engine.EvaluateOTTLWithPolicy(ctx, req, s.policy)
	}
	in, done, err := engine.CaptureOTTL(ctx, req, s.policyFor(rt))
	if err != nil || done != nil {
		return done, err
	}
	return rt.EvaluateOTTL(ctx, in)
}

// SubmitRun captures and validates sources, binds the input bundle and admits
// the run. Cancelling ctx abandons preparation; an admitted run belongs to the
// service and continues after the caller leaves.
func (s *Service) SubmitRun(ctx context.Context, caller Caller, req RunRequest) (_ Run, err error) {
	defer s.audit(&caller, "SubmitRun", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return Run{}, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return Run{}, err
	}
	er, err := s.runRequest(ctx, caller, req)
	if err != nil {
		return Run{}, err
	}
	r, err := s.submit(ctx, caller, req.Mode, er, time.Time{})
	if err != nil {
		return Run{}, classify(err, CodeValidationFailed)
	}
	return r.View(false), nil
}

// ExecuteRun submits a run and waits for its report, bounded by ctx: cancelling
// ctx cancels this run and returns any report it produced.
func (s *Service) ExecuteRun(ctx context.Context, caller Caller, req RunRequest) (_ *Report, err error) {
	defer s.audit(&caller, "ExecuteRun", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	er, err := s.runRequest(ctx, caller, req)
	if err != nil {
		return nil, err
	}
	rep, err := s.execute(ctx, caller, req.Mode, er)
	return rep, classify(err, CodeValidationFailed)
}

// runRequest resolves references and maps a run request onto the engine.
func (s *Service) runRequest(ctx context.Context, caller Caller, req RunRequest) (engine.Request, error) {
	req, refs, err := s.resolveRun(ctx, caller, req)
	if err != nil {
		return engine.Request{}, err
	}
	er, err := req.engine()
	if err != nil {
		return engine.Request{}, classify(err, CodeInvalidRequest)
	}
	er.Sources = refs
	return er, nil
}

func (s *Service) lookup(ctx context.Context, caller Caller, perm authz.Permission, id string) (*Handle, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	r, err := s.handle(ctx, id)
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	caller.touch(id)
	if err := s.authorizeIn(caller, perm, r.project); err != nil {
		return nil, err
	}
	return r, nil
}

// GetRun returns a run's state and, once done, its report without snapshots.
func (s *Service) GetRun(ctx context.Context, caller Caller, id string) (_ Run, err error) {
	defer s.audit(&caller, "GetRun", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return Run{}, err
	}
	return r.View(false), nil
}

// ListRuns returns a page of retained runs newest first, without reports
// but with their summaries, narrowed by q's filter.
func (s *Service) ListRuns(ctx context.Context, caller Caller, q RunQuery) (_ *RunPage, err error) {
	defer s.audit(&caller, "ListRuns", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if err := q.validate(); err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	page, err := s.list(ctx, q, s.projects(caller, authz.RunRead))
	return page, classify(err, CodeInternal)
}

// WatchRun returns the run's events after cursor and a channel closed on
// its next change. Checking and subscribing are atomic, so callers that
// watch again after Changed miss nothing. A cursor in an earlier attempt of
// a requeued run restarts the watch at the current attempt's first event.
func (s *Service) WatchRun(ctx context.Context, caller Caller, id string, cursor EventCursor) (_ RunWatch, err error) {
	defer s.audit(&caller, "WatchRun", &err)()
	if cursor.Attempt < 0 || cursor.Seq < 0 || (cursor.Attempt == 0 && cursor.Seq != 0) {
		return RunWatch{}, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid event cursor %d:%d", cursor.Attempt, cursor.Seq)}
	}
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return RunWatch{}, err
	}
	w := r.Since(cursor)
	if w.Terminal {
		w.End = r.End()
	}
	return w, nil
}

// CancelRun ends a queued run immediately or asks a running Collector to stop;
// a running run still publishes a report with status cancelled.
func (s *Service) CancelRun(ctx context.Context, caller Caller, id string) (_ Cancellation, err error) {
	defer s.audit(&caller, "CancelRun", &err)()
	r, err := s.lookup(ctx, caller, authz.RunSubmit, id)
	if err != nil {
		return Cancellation{}, err
	}
	effective, err := r.cancelRequest(context.Canceled)
	if err != nil {
		return Cancellation{}, classify(err, CodeStorageError)
	}
	return Cancellation{Run: r.View(false), Effective: effective}, nil
}

// GetRunReport returns the complete report, snapshots included. With wait it
// blocks until the run ends or ctx ends. A run without a report is a conflict,
// except a failed run (run_failed) and failed publication (storage_error).
func (s *Service) GetRunReport(ctx context.Context, caller Caller, id string, wait bool) (_ *Report, err error) {
	defer s.audit(&caller, "GetRunReport", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return nil, err
	}
	if wait {
		if err := r.Wait(ctx); err != nil {
			return nil, classify(err, CodeCancelled)
		}
	}
	v := r.View(true)
	switch v.State {
	case StateDone:
		return v.Report, nil
	case StateError:
		cause := r.Err()
		e := &Error{Code: CodeRunFailed, Message: v.Error, cause: cause}
		if v.Interrupted {
			e.Code = CodeConflict
		} else if errors.Is(cause, ErrPersistence) {
			e.Code = CodeStorageError
		}
		return nil, e
	case StateCancelled:
		return nil, &Error{Code: CodeConflict, Message: "run was cancelled before it started"}
	}
	return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run is %s", v.State)}
}

func (s *Service) finished(ctx context.Context, caller Caller, perm authz.Permission, id string, full bool) (Run, error) {
	r, err := s.lookup(ctx, caller, perm, id)
	if err != nil {
		return Run{}, err
	}
	v := r.View(full)
	if v.Report == nil {
		return Run{}, &Error{Code: CodeConflict, Message: fmt.Sprintf("run is %s", v.State)}
	}
	return v, nil
}

// GetSnapshot returns a recorded OTLP JSON snapshot's original bytes by
// content reference, from the instrumented or the reference run.
func (s *Service) GetSnapshot(ctx context.Context, caller Caller, id, ref string) (_ []byte, err error) {
	defer s.audit(&caller, "GetSnapshot", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, id)
	if err != nil {
		return nil, err
	}
	if rep := r.View(true).Report; rep != nil {
		for _, res := range []*runresult.Result{rep.Result, rep.Reference} {
			if res == nil {
				continue
			}
			if data, ok := res.Snapshots[ref]; ok {
				return data, nil
			}
		}
	}
	return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("snapshot %q not found", ref)}
}

// GetStageChanges returns a finished run's stage outcomes, diffs and OTTL
// replays.
func (s *Service) GetStageChanges(ctx context.Context, caller Caller, id string) (_ *StageChanges, err error) {
	defer s.audit(&caller, "GetStageChanges", &err)()
	v, err := s.finished(ctx, caller, authz.RunRead, id, false)
	if err != nil {
		return nil, err
	}
	return &StageChanges{Stages: v.Report.Stages, OTTL: v.Report.OTTL}, nil
}
