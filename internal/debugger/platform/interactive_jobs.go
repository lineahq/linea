package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// Interactive jobs. With shared instances, an inspection or
// OTTL preview on a connected runtime goes to a worker of this instance when
// one is free; otherwise it is posted as a job row that any instance with a
// free worker of the runtime claims before its queued runs. The requester
// waits for the job's change and deletes the job once answered.

// jobClaimTimeout bounds how long a posted job waits for a worker of its
// runtime on any instance.
const jobClaimTimeout = 30 * time.Second

// interactive completes an interactive request on a connected worker that
// runs target, whose stored binary is binary: through call on a free slot
// of this instance, or as a job.
func interactive[T any](ctx context.Context, s *Service, target engine.RuntimeTarget, binary *artifact.RunBinary, kind artifact.JobKind, in any, call func(*worker.Client, *worker.PoolBinary) (*T, error)) (*T, error) {
	if e, release := s.freeSlot(target, binary); e != nil {
		defer release()
		return callOn(ctx, s, e, target, binary, call)
	}
	raw, err := s.postJob(ctx, target, kind, in)
	if err != nil {
		return nil, err
	}
	var out T
	if err := engine.Decode(raw, &out); err != nil {
		return nil, fmt.Errorf("a worker of runtime %s sent an unreadable answer: %w", target.Name, err)
	}
	return &out, nil
}

// freeSlot takes a free slot on a connected worker that runs target, a
// worker of its identity before a pool, without waiting; e is nil when
// every such worker is busy or none is connected.
func (s *Service) freeSlot(target engine.RuntimeTarget, binary *artifact.RunBinary) (e *executor, release func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.draining {
		return nil, nil
	}
	for _, pools := range []bool{false, true} {
		for _, e := range s.executors {
			if e.runs(target, binary) && (e.pool() != "") == pools && e.free() > 0 {
				e.busy++
				return e, func() { s.releaseSlot(e) }
			}
		}
	}
	return nil, nil
}

// postJob posts an interactive job and waits for its answer.
func (s *Service) postJob(ctx context.Context, target engine.RuntimeTarget, kind artifact.JobKind, in any) ([]byte, error) {
	input, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	if len(input) > artifact.MaxJobBytes {
		return nil, fmt.Errorf("%w: the request exceeds %d bytes", engine.ErrInputLimit, artifact.MaxJobBytes)
	}
	id := newJobID()
	answered := make(chan struct{}, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	s.jobWaiters[id] = answered
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.jobWaiters, id)
		s.mu.Unlock()
	}()
	if err := s.repository.PostJob(ctx, artifact.JobRecord{ID: id, Runtime: target.ID, Kind: kind, Input: input}); err != nil {
		return nil, persistenceError(err)
	}
	// A job without history: whatever happens to the request, its row goes.
	defer func() { _ = s.repository.DeleteJob(context.WithoutCancel(ctx), id) }()
	s.mu.Lock()
	s.jobsQueued = true
	s.wakeLocked()
	s.mu.Unlock()

	claimBy := time.NewTimer(jobClaimTimeout)
	defer claimBy.Stop()
	// An instance that claimed the job and then stopped never answers.
	answerBy := time.NewTimer(jobClaimTimeout + s.budgets.Execution + time.Minute)
	defer answerBy.Stop()
	poll := time.NewTicker(followInterval)
	defer poll.Stop()
	for {
		j, err := s.repository.Job(ctx, id)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, artifact.ErrNotFound):
			return nil, &Error{Code: CodeUnavailable, Message: "the request expired before a worker answered it"}
		case err != nil:
			return nil, persistenceError(err)
		case j.State == artifact.JobDone && len(j.Error) != 0:
			return nil, worker.UnmarshalError(j.Error)
		case j.State == artifact.JobDone:
			return j.Result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, ErrClosed
		case <-answered:
		case <-poll.C:
		case <-claimBy.C:
			if err := s.repository.WithdrawJob(ctx, id); err == nil {
				return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: no worker of it took the request within %s", target.Name, jobClaimTimeout)}
			}
		case <-answerBy.C:
			return nil, &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: the worker that took the request did not answer", target.Name)}
		}
	}
}

func newJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "job-" + hex.EncodeToString(b[:])
}

// jobChanged wakes the requester waiting for a job, and the dispatcher,
// which claims posted jobs.
func (s *Service) jobChanged(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if answered := s.jobWaiters[id]; answered != nil {
		select {
		case answered <- struct{}{}:
		default:
		}
		return
	}
	s.jobsQueued = true
	s.wakeLocked()
}

// claimJobs claims posted jobs for every connected worker with free slots,
// before any queued run. It reports whether it claimed any.
func (s *Service) claimJobs() bool {
	type reservation struct {
		e *executor
		n int
	}
	s.mu.Lock()
	if !s.shared || !s.jobsQueued || s.closed || s.draining {
		s.mu.Unlock()
		return false
	}
	var reserved []reservation
	for _, e := range s.executors {
		if n := e.free(); e.conn != nil && !e.gone && n > 0 {
			e.busy += n
			reserved = append(reserved, reservation{e, n})
		}
	}
	// Workers that are all busy look again when a slot frees.
	s.jobsQueued = len(reserved) == 0
	s.mu.Unlock()
	claimed := false
	var built map[string]*artifact.RunBinary
	for _, r := range reserved {
		var jobs []artifact.JobRecord
		var binaries []*artifact.RunBinary
		var err error
		if p := r.e.pool(); p == "" {
			jobs, err = s.repository.ClaimJobs(s.ctx, r.e.id, r.e.conn.hello.Runtime, r.n)
			binaries = make([]*artifact.RunBinary, len(jobs))
		} else {
			// A pool claims the jobs of its own build first, then those of
			// every runtime built for its platform.
			if own := r.e.own(); own != "" {
				jobs, err = s.repository.ClaimJobs(s.ctx, r.e.id, own, r.n)
				binaries = make([]*artifact.RunBinary, len(jobs))
			}
			if built == nil && err == nil {
				built, err = s.builtRuntimes(s.ctx)
			}
			for identity, b := range built {
				if err != nil || len(jobs) == r.n {
					break
				}
				if b.Platform != p {
					continue
				}
				var more []artifact.JobRecord
				more, err = s.repository.ClaimJobs(s.ctx, r.e.id, identity, r.n-len(jobs))
				jobs = append(jobs, more...)
				for range more {
					binaries = append(binaries, b)
				}
			}
		}
		if err != nil {
			s.logError("claiming interactive jobs", err)
		}
		s.mu.Lock()
		r.e.busy -= r.n - len(jobs)
		if err != nil || len(jobs) == r.n {
			s.jobsQueued = true // there may be more
		}
		s.freedLocked()
		s.mu.Unlock()
		for i, j := range jobs {
			claimed = true
			s.executing.Add(1)
			go s.answerJob(r.e, j, binaries[i])
		}
	}
	return claimed
}

// builtRuntimes maps the identities of the catalog's runtimes built on the
// server to their binaries.
func (s *Service) builtRuntimes(ctx context.Context) (map[string]*artifact.RunBinary, error) {
	entries, err := s.catalog.RuntimeEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]*artifact.RunBinary{}
	for _, e := range entries {
		if b := e.Binary; b != nil {
			out[e.Identity] = &artifact.RunBinary{Digest: b.Digest, Bytes: b.Bytes, Platform: b.OS + "/" + b.Arch}
		}
	}
	return out, nil
}

// answerJob completes a claimed job on worker e, running binary when e is
// a pool, and records the answer.
func (s *Service) answerJob(e *executor, j artifact.JobRecord, binary *artifact.RunBinary) {
	defer s.executing.Done()
	defer s.releaseSlot(e)
	ctx, cancel := context.WithTimeout(s.ctx, s.budgets.Execution)
	defer cancel()
	var out any
	b, err := s.binaryFor(ctx, e, engine.RuntimeTarget{ID: j.Runtime, Name: j.Runtime}, binary)
	switch {
	case err != nil:
	case j.Kind == artifact.JobInspect:
		var in engine.InspectionInput
		if err = json.Unmarshal(j.Input, &in); err == nil {
			out, err = e.conn.client.InspectOn(ctx, b, &in)
		}
	case j.Kind == artifact.JobOTTL:
		var in engine.OTTLInput
		if err = json.Unmarshal(j.Input, &in); err == nil {
			out, err = e.conn.client.EvaluateOTTLOn(ctx, b, &in)
		}
	default:
		err = fmt.Errorf("%w: unknown interactive job kind %q", engine.ErrInvalidRequest, j.Kind)
	}
	err = s.suspected(e, b, err)
	var result []byte
	if err == nil {
		if result, err = json.Marshal(out); err == nil && len(result) > artifact.MaxJobBytes {
			result, err = nil, fmt.Errorf("%w: the answer exceeds %d bytes", engine.ErrInputLimit, artifact.MaxJobBytes)
		}
	}
	var failure []byte
	if err != nil {
		if failure, err = worker.MarshalError(err); err != nil {
			s.logError("encoding an interactive job's error", err)
			return
		}
	}
	if err := s.repository.FinishJob(s.ctx, j.ID, e.id, result, failure); err != nil && !errors.Is(err, artifact.ErrNotFound) {
		s.logError("answering an interactive job", err)
	}
}
