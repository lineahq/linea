package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// Scheduling. Every admitted run is queued in the repository,
// or for a service without one in a private in-memory queue. Executors claim
// the runs the service holds jobs for, each up to its free capacity, under a
// lease the service's heartbeat renews: the service's own executor one run
// at a time, and every connected worker (worker_pool.go) as many as it
// announced. A lease that ends returns the run to the queue once; on its
// last attempt the run is lost.

const (
	// DefaultLease is how long a claim lasts without a heartbeat.
	DefaultLease = 30 * time.Second
	// publishTimeout bounds each write of a run's outcome. The run's lease
	// is renewed while it is written.
	publishTimeout = 2 * time.Minute
	// maxAttempts bounds a run's assignments: one requeue after its worker
	// stopped renewing.
	maxAttempts = 2
	// builtinRuntime is the queue's runtime for runs on the service's own
	// in-process runtime.
	builtinRuntime = "builtin"
	// poolGrace is how long after its oldest run queued a pool that would
	// have to start a runtime leaves it to a pool of another instance that
	// holds it running.
	poolGrace = 2 * time.Second
)

var (
	ErrLost      = errors.New("run lost: its worker stopped renewing its lease on the run's last attempt")
	errLeaseLost = errors.New("this worker's lease on the run ended")
)

// runtimeKey is the queue's runtime for a prepared request: the identity of
// the runtime it names, or the built-in one.
func runtimeKey(req engine.Request) string {
	if req.Runtime != nil && req.Runtime.ID != "" {
		return req.Runtime.ID
	}
	return builtinRuntime
}

func newExecutorID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "worker-" + hex.EncodeToString(b[:])
}

// requestCancelHook marks a run cancelled in the queue and reports whether a
// worker holds it, so that worker ends it.
func (s *Service) requestCancelHook(r *Handle) func() (bool, error) {
	return func() (bool, error) {
		a, err := s.runQueue.RequestCancel(context.Background(), r.id)
		if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrConflict) {
			// No queue entry (a run admitted before scheduling was durable),
			// or the queue already ended it: the handle's state decides.
			return r.state != StateQueued, nil
		}
		if err != nil {
			return false, err
		}
		return a.Worker != "", nil
	}
}

// queuedKey is a runtime with queued jobs, and whether they were bound to
// connected workers.
type queuedKey struct {
	key    string
	pooled bool
}

// queuedRuntime is a queued key and when its oldest job queued.
type queuedRuntime struct {
	queuedKey
	oldest time.Time
}

// claimOrderLocked lists the runtimes of the queued jobs, oldest job first.
func (s *Service) claimOrderLocked() []queuedRuntime {
	var waiting []queuedRuntime
	for _, job := range s.jobs {
		r := job.handle
		r.mu.Lock()
		if r.state == StateQueued {
			waiting = append(waiting, queuedRuntime{queuedKey{r.runtimeKey, job.pooled}, r.created})
		}
		r.mu.Unlock()
	}
	slices.SortFunc(waiting, func(a, b queuedRuntime) int { return a.oldest.Compare(b.oldest) })
	var keys []queuedRuntime
	for _, w := range waiting {
		if !slices.ContainsFunc(keys, func(k queuedRuntime) bool { return k.queuedKey == w.queuedKey }) {
			keys = append(keys, w)
		}
	}
	return keys
}

// sweepKeysLocked adds to keys the runtimes the connected workers run
// without starting one: a worker's runtime, and a pool's own build and the
// runtimes it holds running. Another instance's run reaches this service's
// jobs through the change feed; should that ever miss one, the heartbeat's
// sweep still claims it for a worker here that serves it (adoptClaimed).
// The added keys have no oldest run, so no pool grace holds them back.
func (s *Service) sweepKeysLocked(keys []queuedRuntime) []queuedRuntime {
	add := func(key string) {
		k := queuedKey{key, true}
		if key != "" && !slices.ContainsFunc(keys, func(q queuedRuntime) bool { return q.queuedKey == k }) {
			keys = append(keys, queuedRuntime{queuedKey: k})
		}
	}
	for _, e := range s.executors {
		if e.conn == nil || e.gone {
			continue
		}
		if e.pool() == "" {
			add(e.conn.hello.Runtime)
			continue
		}
		add(e.own())
		warm, _ := e.conn.client.Warm()
		for _, key := range warm {
			add(key)
		}
	}
	return keys
}

// dispatch claims queued runs for every executor with free capacity and
// starts them. It reports whether it claimed any, so the caller looks again
// before it waits.
func (s *Service) dispatch() bool {
	s.mu.Lock()
	if s.closed || s.draining {
		s.mu.Unlock()
		return false
	}
	keys := s.claimOrderLocked()
	if s.sweep {
		s.sweep = false
		keys = s.sweepKeysLocked(keys)
	}
	executors := slices.Clone(s.executors)
	s.mu.Unlock()
	// Interactive jobs go before runs.
	claimed := s.claimJobs()
	now := s.now()
	var elsewhere map[string]bool
	var retry time.Time
	for _, k := range keys {
		for _, e := range preferred(executors, k.queuedKey) {
			if until, ok := graceUntil(poolRank(e, k.key), k.oldest, now); ok {
				if elsewhere == nil {
					elsewhere = s.warmElsewhere(now)
				}
				if elsewhere[k.key] {
					// Another instance's pool holds it running: leave the
					// runs to it for the grace, then look again.
					if retry.IsZero() || until.Before(retry) {
						retry = until
					}
					continue
				}
			}
			// Reserve the free slots for the claim, so an interactive
			// request cannot take one the claim fills.
			s.mu.Lock()
			n := e.free()
			if s.closed || e.gone || n <= 0 || e.conn != nil && e.held(s.waiting) {
				s.mu.Unlock()
				continue
			}
			e.busy += n
			s.mu.Unlock()
			var as []artifact.Assignment
			var err error
			if p := e.pool(); p != "" && k.key != e.own() {
				as, err = s.runQueue.ClaimBinary(s.ctx, e.id, k.key, p, n, s.now().Add(s.lease))
			} else {
				as, err = s.runQueue.Claim(s.ctx, e.id, k.key, n, s.now().Add(s.lease))
			}
			if err != nil {
				s.logError("claiming queued runs", err)
			}
			var jobs []*pendingRun
			var adopt []artifact.Assignment
			s.mu.Lock()
			e.busy -= n
			if s.closed && s.shared {
				// Closed during the claim: hand the runs straight back.
				s.mu.Unlock()
				s.releaseHeld(as)
				return true
			}
			for _, a := range as {
				claimed = true
				if s.shared && s.jobs[a.RunID] == nil {
					// Another instance's run: keep its slot while it is read.
					e.busy++
					adopt = append(adopt, a)
				} else if job := s.takeLocked(a, e); job != nil {
					jobs = append(jobs, job)
				}
			}
			s.mu.Unlock()
			for _, a := range adopt {
				if job := s.adoptClaimed(a, e); job != nil {
					jobs = append(jobs, job)
				}
			}
			for _, job := range jobs {
				s.executing.Add(1)
				go s.runJob(job)
			}
		}
	}
	if !retry.IsZero() {
		s.wakeAt(now, retry)
	}
	return claimed
}

// wakeAt wakes dispatch at at, unless a wake-up is already due by then.
func (s *Service) wakeAt(now, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.graceAt.IsZero() && !s.graceAt.After(at) {
		return
	}
	s.graceAt = at
	time.AfterFunc(at.Sub(now), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.graceAt.Equal(at) {
			s.graceAt = time.Time{}
		}
		s.wakeLocked()
	})
}

// preferred lists the executors that serve key k in the order they claim
// its runs: workers of its runtime, then pools that hold it
// running, then pools with room to start it, then the other pools.
func preferred(executors []*executor, k queuedKey) []*executor {
	var out []*executor
	for _, e := range executors {
		if e.serves(k.pooled, k.key) {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b *executor) int { return poolRank(a, k.key) - poolRank(b, k.key) })
	return out
}

// graceUntil reports whether an executor of rank (poolRank) leaves a runtime
// whose oldest run queued at oldest to a warm pool of another instance, and
// until when: only a pool that would have to start it, and only for
// poolGrace.
func graceUntil(rank int, oldest, now time.Time) (time.Time, bool) {
	until := oldest.Add(poolGrace)
	return until, rank >= 2 && now.Before(until)
}

// poolRank orders the executors that serve runtime key: 0 for a worker of
// it, 1 for a pool that holds it running, 2 for a pool with room to start
// it, 3 for any other pool.
func poolRank(e *executor, key string) int {
	if e.pool() == "" {
		return 0
	}
	warm, _ := e.conn.client.Warm()
	switch {
	case slices.Contains(warm, key):
		return 1
	case len(warm) < e.conn.hello.Pool.Warm:
		return 2
	}
	return 3
}

// runJob starts a taken job, executes it and publishes its outcome.
func (s *Service) runJob(job *pendingRun) {
	defer s.executing.Done()
	if !s.startJob(job) {
		return
	}
	flushed := s.flushEvents(job)
	report, err := s.run(job)
	flushed()
	job.handle.mu.Lock()
	job.handle.cancel(nil)
	job.handle.mu.Unlock()
	s.finish(job, report, err)
}

// takeLocked assigns a claimed run to executor e. A run that ended or was
// cancelled meanwhile is finished instead.
func (s *Service) takeLocked(a artifact.Assignment, e *executor) *pendingRun {
	job := s.jobs[a.RunID]
	if job == nil || s.closed || !e.serves(job.pooled, a.Runtime) || e.pool() != "" && !e.runs(engine.RuntimeTarget{ID: a.Runtime}, a.Binary) {
		// A run this service holds no job for, or one bound to the other
		// kind of executor or to no binary of a pool's: its lease ends and
		// it is requeued.
		return nil
	}
	r := job.handle
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalLocked() {
		delete(s.jobs, a.RunID)
		return nil
	}
	if a.Generation > r.generation {
		r.generation = a.Generation
	}
	if job.ctx.Err() != nil || a.CancelRequested {
		r.cancel(context.Cause(job.ctx))
		r.finishLocked(StateCancelled, nil, nil)
		delete(s.jobs, a.RunID)
		return nil
	}
	if a.Attempt == 1 && !r.created.IsZero() {
		// The first claim: the run's wait in the queue since admission.
		s.metrics.claimWait.Record(s.ctx, s.now().Sub(r.created).Seconds())
	}
	r.state, r.attempt = StateAssigned, a.Attempt
	r.notifyLocked()
	job.assignment = a
	job.exec = e
	e.busy++
	return job
}

// releaseLocked frees the slot a job held on its executor.
func (s *Service) releaseLocked(job *pendingRun) {
	if e := job.exec; e != nil {
		job.exec = nil
		e.busy--
		s.freedLocked()
		s.wakeLocked()
	}
}

// startJob restores a claimed job's request when it has none and starts the
// run. It reports false when the run was finished or its lease lost instead.
func (s *Service) startJob(job *pendingRun) bool {
	var restoreErr error
	if job.restore {
		restoreErr = s.restore(job)
	}
	retry := false
	defer func() {
		// After s.mu is released: wakeAt takes it.
		if retry {
			now := s.now()
			s.wakeAt(now, now.Add(startRetryDelay))
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := job.handle
	r.mu.Lock()
	defer r.mu.Unlock()
	fail := func(state State, err error) bool {
		s.releaseLocked(job)
		r.cancel(nil)
		r.finishLocked(state, nil, err)
		s.dropJobLocked(job)
		return false
	}
	if r.terminalLocked() || r.state != StateAssigned || r.attempt != job.assignment.Attempt || s.closed && s.shared {
		s.releaseLocked(job)
		return false
	}
	if job.ctx.Err() != nil {
		return fail(StateCancelled, nil)
	}
	if restoreErr != nil {
		return fail(StateError, restoreErr)
	}
	if err := s.runQueue.Start(s.ctx, job.assignment); err != nil {
		s.releaseLocked(job)
		if errors.Is(err, artifact.ErrConflict) {
			// The lease ended before the run started; expiry requeues it.
			return false
		}
		// The lease is still held, and renewal would keep it while the
		// executor is connected: hand the run back to the queue.
		s.logError("starting a claimed run", err)
		if s.releaseUnstartedLocked(job) {
			retry = true
		}
		return false
	}
	r.state, r.started = StateRunning, time.Now().UTC()
	job.request.Observer, job.request.Log = r.recordLocked(r.attempt)
	if r.persist != nil {
		if err := r.persist(false); err != nil {
			return fail(StateError, persistenceError(err))
		}
	}
	r.notifyLocked()
	return true
}

// startRetryDelay is how long dispatch waits to claim again after a run it
// could not start was handed back, so a failing store is not retried in a
// tight loop.
const startRetryDelay = time.Second

// releaseUnstartedLocked returns a claimed run whose start could not be
// recorded to the queue, counted as released rather than as an attempt.
// It reports whether the run is queued again; if the store refused, the
// executor's next renewal tries again.
func (s *Service) releaseUnstartedLocked(job *pendingRun) bool {
	ctx, cancel := context.WithTimeout(s.ctx, releaseTimeout)
	defer cancel()
	generation, err := s.runQueue.Release(ctx, job.assignment)
	job.unreleased = err != nil && !errors.Is(err, artifact.ErrConflict)
	if err != nil {
		if job.unreleased {
			s.logError("returning a run that could not start", err)
		}
		return false
	}
	r := job.handle
	if generation > r.generation {
		r.generation = generation
	}
	r.state, r.started = StateQueued, time.Time{}
	r.notifyLocked()
	return true
}

// finish publishes a job's outcome unless a requeue superseded its attempt.
// A connected worker's run whose connection ended is left to its lease,
// which requeues it.
//
// The outcome is written without holding s.mu or the run's mu, so a slow
// repository does not hold up lease renewal or other runs. Until it is
// written the run stays running and its executor busy; expire leaves it to
// this call.
func (s *Service) finish(job *pendingRun, report *engine.Report, err error) {
	r := job.handle
	s.mu.Lock()
	defer s.mu.Unlock()
	if connectionLost(job, err) || s.closed && s.shared {
		// Another instance takes the run over when its lease ends.
		s.releaseLocked(job)
		return
	}
	r.mu.Lock()
	if r.terminalLocked() || r.state != StateRunning || r.attempt != job.assignment.Attempt {
		r.mu.Unlock()
		s.releaseLocked(job)
		return
	}
	if err == nil && report == nil {
		err = errors.New("engine returned neither a report nor an error")
	}
	state := StateDone
	if err != nil {
		state, report = StateError, nil
	}
	if r.persist == nil {
		r.finishLocked(state, report, err)
		r.mu.Unlock()
	} else {
		p, shared := r.endingLocked(state, report, err), r.shared
		r.publishing = true
		r.mu.Unlock()
		s.mu.Unlock()
		record, conflict := s.publishEnd(&p, shared)
		s.mu.Lock()
		r.mu.Lock()
		r.publishing = false
		switch {
		case conflict || r.terminalLocked():
			// Another instance published the run first, or the change feed
			// already brought this publication: the handle catches up on
			// what the repository holds.
		case record != nil:
			r.endLocked(p)
			r.storedLocked(record)
		case r.state == StateRunning && r.attempt == p.attempt:
			r.endLocked(p)
		}
		r.mu.Unlock()
	}
	s.releaseLocked(job)
	s.dropJobLocked(job)
	s.evictLocked()
}

// publishEnd writes a run's outcome p with its evidence, within
// publishTimeout. When that fails, p becomes an error without a report and
// is written without evidence, best effort: an uncertain repository refuses
// this write too. It returns the record written, if any, and whether
// another instance published the run first, which leaves p unwritten.
func (s *Service) publishEnd(p *runPublication, shared bool) (*artifact.RunRecord, bool) {
	save := func(evidence bool) (*artifact.RunRecord, error) {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		return s.savePublication(ctx, p, evidence)
	}
	record, err := save(true)
	if shared && errors.Is(err, artifact.ErrConflict) {
		return nil, true
	}
	if err != nil {
		p.state, p.report, p.err = StateError, nil, persistenceError(err)
		record, _ = save(false)
	}
	return record, false
}

func (s *Service) dropJobLocked(job *pendingRun) {
	if s.jobs[job.handle.id] == job {
		delete(s.jobs, job.handle.id)
	}
}

// leaseRenewal is an executor's lease renewal: renewLeases writes it
// without s.mu, so a slow holder of s.mu does not stop leases from being
// renewed, and tendLeases acts on it under s.mu.
type leaseRenewal struct {
	mu sync.Mutex
	// renewed is the last successful renewal.
	renewed time.Time
	// fresh is set by a renewal tendLeases has not handled yet, which returned
	// held or failed with err.
	fresh bool
	held  []artifact.Assignment
	err   error
}

// leasedLocked publishes the executor list to lease renewal. s.mu is held.
func (s *Service) leasedLocked() {
	executors := slices.Clone(s.executors)
	s.leased.Store(&executors)
}

// heartbeats renews the executors' leases, and separately handles renewals
// and expired leases, until the service closes.
func (s *Service) heartbeats() {
	defer close(s.beating)
	renewing := make(chan struct{})
	go func() {
		defer close(renewing)
		s.every(s.lease/3, s.renewLeases)
	}()
	s.every(s.lease/3, s.tendLeases)
	<-renewing
}

func (s *Service) every(d time.Duration, f func(time.Time)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			f(s.now())
		}
	}
}

// heartbeat renews every executor's leases and handles the renewals and
// expired leases.
func (s *Service) heartbeat(now time.Time) {
	s.renewLeases(now)
	s.tendLeases(now)
}

// renewLeases renews every executor's leases, each within a third of the
// lease, and records the outcome for tendLeases.
func (s *Service) renewLeases(now time.Time) {
	leased := s.leased.Load()
	if leased == nil {
		return
	}
	for _, e := range *leased {
		ctx, cancel := context.WithTimeout(s.ctx, s.lease/3)
		held, err := s.runQueue.Renew(ctx, e.id, now.Add(s.lease))
		cancel()
		if err == nil {
			s.metrics.leaseRenewals.Add(s.ctx, 1, renewed)
		} else if s.ctx.Err() == nil {
			s.metrics.leaseRenewals.Add(s.ctx, 1, renewFailed)
		}
		e.lease.mu.Lock()
		if err == nil {
			e.lease.renewed = now
		}
		e.lease.fresh, e.lease.held, e.lease.err = true, held, err
		e.lease.mu.Unlock()
	}
}

// tendLeases handles every executor's last renewal, has a shared service's next
// dispatch sweep (sweepKeysLocked), then requeues or ends runs whose lease
// ended.
func (s *Service) tendLeases(now time.Time) {
	s.mu.Lock()
	executors := slices.Clone(s.executors)
	s.mu.Unlock()
	for _, e := range executors {
		s.renewed(e, now)
	}
	s.publishWorkers(s.ctx, now)
	if s.shared {
		s.mu.Lock()
		s.sweep = true
		s.wakeLocked()
		s.mu.Unlock()
	}
	expired, err := s.runQueue.Expired(s.ctx, now)
	if err != nil {
		s.logError("listing expired leases", err)
		return
	}
	// The scan is the scheduler's regular read of the store: StorageReady
	// judges an outage by when it last answered.
	s.metrics.storageAnswered(s.now())
	for _, a := range expired {
		if s.shared {
			// Any instance ends or requeues an expired run; one this service
			// has not seen yet is read first.
			s.mu.Lock()
			known := s.runs[a.RunID] != nil
			s.mu.Unlock()
			if !known {
				if err := s.applyRun(s.ctx, a.RunID); err != nil {
					s.logError("reading an expired run", err)
					continue
				}
			}
		}
		s.expire(a, now)
	}
}

// renewed handles an executor's last renewal: it stops the executor's runs
// when cancellation was requested or the leases were not renewed in time,
// and publishes again a terminal state a failed write left held.
func (s *Service) renewed(e *executor, now time.Time) {
	e.lease.mu.Lock()
	fresh, held, err, renewed := e.lease.fresh, e.lease.held, e.lease.err, e.lease.renewed
	e.lease.fresh, e.lease.held, e.lease.err = false, nil, nil
	e.lease.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if fresh && err != nil {
		s.logError("renewing leases", err)
	}
	if now.After(renewed.Add(s.lease)) {
		for _, job := range s.jobs {
			if job.exec == e {
				job.handle.mu.Lock()
				if !job.handle.terminalLocked() && context.Cause(job.ctx) == nil {
					s.metrics.leasesLost.Add(s.ctx, 1)
				}
				job.handle.cancel(errLeaseLost)
				job.handle.mu.Unlock()
			}
		}
	}
	for _, a := range held {
		r := s.runs[a.RunID]
		if r == nil {
			continue
		}
		job := s.jobs[a.RunID]
		r.mu.Lock()
		switch {
		case r.terminalLocked() && r.persist != nil:
			_ = r.persist(false)
		case a.CancelRequested && job != nil && job.exec == e:
			r.cancel(context.Canceled)
		case job != nil && job.unreleased && job.exec == nil && a.Attempt == job.assignment.Attempt:
			if s.releaseUnstartedLocked(job) {
				s.wakeLocked()
			}
		}
		r.mu.Unlock()
	}
}

// expire requeues a run whose lease ended, or ends it: cancelled when
// cancellation was requested, lost on its last attempt.
func (s *Service) expire(a artifact.Assignment, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[a.RunID]
	if r == nil || s.closed {
		return // another installation's run, or deleted
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.publishing {
		// finish is writing the run's outcome; a lease still ended after
		// that write is listed again.
		return
	}
	if job := s.jobs[r.id]; job != nil && job.exec != nil {
		// This service's own lease ended: whatever its executor publishes
		// now is stale.
		if !r.terminalLocked() && context.Cause(job.ctx) == nil {
			s.metrics.leasesLost.Add(s.ctx, 1)
		}
		r.cancel(errLeaseLost)
	}
	if r.terminalLocked() {
		return
	}
	if a.Generation > r.generation {
		r.generation = a.Generation
	}
	r.attempt = a.Attempt
	if a.CancelRequested || a.Attempt-a.Released >= maxAttempts {
		if a.CancelRequested {
			r.finishLocked(StateCancelled, nil, nil)
		} else {
			r.finishLocked(StateLost, nil, ErrLost)
			s.metrics.expiries.Add(s.ctx, 1, expLost)
		}
		delete(s.jobs, r.id)
		return
	}
	generation, err := s.runQueue.Requeue(s.ctx, a, now)
	if err != nil {
		if !errors.Is(err, artifact.ErrConflict) {
			s.logError("requeueing a run", err)
		}
		return
	}
	job, err := s.requeuedJobLocked(r)
	r.generation, r.state, r.started = generation, StateQueued, time.Time{}
	if err == nil && r.persist != nil {
		if perr := r.persist(false); perr != nil {
			err = persistenceError(perr)
		}
	}
	if err != nil {
		r.cancel(nil)
		r.interrupted = errors.Is(err, ErrInterrupted)
		r.finishLocked(StateError, nil, err)
		delete(s.jobs, r.id)
		return
	}
	r.notifyLocked()
	s.jobs[r.id] = job
	s.metrics.expiries.Add(s.ctx, 1, expRequeued)
	s.wakeLocked()
}

// requeuedJobLocked is a fresh job for a requeued run: the previous job's
// request under a new context, or, for a recovered run, one restored from its
// bundle when claimed.
func (s *Service) requeuedJobLocked(r *Handle) (*pendingRun, error) {
	old := s.jobs[r.id]
	if old == nil && r.bundleInfo == nil {
		return nil, ErrInterrupted
	}
	base := s.ctx
	release := func() {}
	if old != nil {
		if deadline, ok := old.ctx.Deadline(); ok {
			base, release = context.WithDeadline(base, deadline)
		}
	}
	ctx, cancelCause := context.WithCancelCause(base)
	r.cancel = func(cause error) {
		cancelCause(cause)
		release()
	}
	if old == nil {
		return &pendingRun{handle: r, ctx: ctx, restore: true, pooled: s.recoversPooled(r)}, nil
	}
	// A run adopted from another instance that this one never claimed still
	// waits to be restored from its bundle.
	if old.restore {
		return &pendingRun{handle: r, ctx: ctx, restore: true, pooled: old.pooled}, nil
	}
	return &pendingRun{handle: r, ctx: ctx, request: old.request, runtime: old.runtime, pooled: old.pooled}, nil
}

// recoveredJobLocked is the job of a run recovered queued, restored from its
// bundle when claimed.
func (s *Service) recoveredJobLocked(r *Handle) *pendingRun {
	ctx, cancel := context.WithCancelCause(s.ctx)
	r.cancel = cancel
	return &pendingRun{handle: r, ctx: ctx, restore: true, pooled: s.recoversPooled(r)}
}

// recoversPooled decides which executor takes a recovered run, whose
// binding was not kept: a service that accepts workers and has no runtime
// store (a team server) runs every runtime but the built-in one on
// connected workers, as the catalog binds them.
func (s *Service) recoversPooled(r *Handle) bool {
	return s.workerToken != "" && s.runtimes == nil && r.runtimeKey != builtinRuntime
}

// restore decodes a recovered run's bundle into its job's request, binding
// the runtime the bundle names. It runs on the worker, outside s.mu.
func (s *Service) restore(job *pendingRun) error {
	r := job.handle
	raw, err := s.repository.RunBundle(job.ctx, r.id)
	if err != nil {
		return persistenceError(err)
	}
	target, err := engine.BundleRuntime(raw)
	if err != nil {
		return err
	}
	var req engine.Request
	var b *boundRuntime
	switch {
	case target == nil:
		req, err = engine.DecodeBundle(job.ctx, raw)
	case job.pooled:
		if b, err = s.pooledByIdentity(job.ctx, *target); err != nil {
			return err
		}
		if b == nil {
			return &Error{Code: CodeUnavailable, Message: fmt.Sprintf("runtime %s is not available: no worker of it is connected", target.Name)}
		}
		req, err = engine.DecodeBundleForRuntime(job.ctx, raw, target.ID)
	default:
		if b, err = s.bind(job.ctx, target.Name); err != nil {
			return err
		}
		if b.target.ID != target.ID {
			return fmt.Errorf("%w: runtime %s is now %s, not %s as when the run was admitted", engine.ErrDistribution, target.Name, b.target.ID, target.ID)
		}
		req, err = engine.DecodeBundleForRuntime(job.ctx, raw, target.ID)
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	s.observeLocked(r, &req)
	r.mu.Unlock()
	job.request, job.runtime, job.restore = req, b, false
	return nil
}

func (s *Service) wakeLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) logError(what string, err error) {
	if s.logger != nil && !errors.Is(err, context.Canceled) {
		s.logger.Warn("scheduler: "+what, zap.Error(err))
	}
}
