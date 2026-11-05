package platform

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// Shared instances. With Options.Shared several services use
// one repository at once. The repository is the truth; a service's runs,
// suites and tombstones are a cache of it that follows the change feed:
// every run read first catches up, so it reflects the repository at read
// time, and a follower catches up whenever the repository hints at a change,
// or at least every followInterval, to wake watchers. Any service may adopt
// a queued run, its own or another's, and executes it from its bundle.

const (
	// followInterval bounds how long a watcher waits for a change another
	// instance made when no hint arrives.
	followInterval = time.Second
	// flushInterval is how often an executing run's new events are published
	// for watchers on other instances.
	flushInterval = 250 * time.Millisecond
)

// follow catches up on hints and ticks until the service stops.
func (s *Service) follow() {
	defer close(s.following)
	if u, ok := s.repository.(artifact.Upkeep); ok {
		upkept := make(chan struct{})
		go s.upkeep(u, upkept)
		defer func() { <-upkept }()
	}
	hints := s.repository.ChangeHints()
	t := time.NewTicker(followInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-hints:
		case <-t.C:
		}
		if err := s.catchUp(s.ctx); err != nil {
			s.logError("following the change feed", err)
		}
	}
}

// upkeepInterval is how often an instance offers to do the shared
// repository's upkeep.
const upkeepInterval = 5 * time.Minute

// upkeep runs the repository's upkeep rounds until the service stops; the
// repository lets one instance at a time do a round.
func (s *Service) upkeep(u artifact.Upkeep, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(upkeepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		// A job older than any wait for its answer belongs to a requester
		// that stopped.
		before := time.Now().Add(-(jobClaimTimeout + s.budgets.Execution + time.Hour))
		if _, err := u.Housekeep(s.ctx, before); err != nil {
			s.logError("upkeep of the shared repository", err)
		}
		if builds, ok := s.serverBuilds(); ok {
			if err := s.buildUpkeep(s.ctx, builds, time.Now().UTC()); err != nil {
				s.logError("upkeep of the runtime builds", err)
			}
		}
	}
}

// catchUp applies every change committed before it started. Catch-ups run
// one at a time; the cursor advances only past applied changes.
func (s *Service) catchUp(ctx context.Context) error {
	if !s.shared {
		return nil
	}
	s.followMu.Lock()
	defer s.followMu.Unlock()
	for {
		changes, next, err := s.repository.Changes(ctx, s.cursor, artifact.MaxChanges)
		if errors.Is(err, artifact.ErrExpired) {
			if err := s.reload(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := s.applyChanges(ctx, changes); err != nil {
			return err
		}
		s.cursor = next
		if len(changes) < artifact.MaxChanges {
			return nil
		}
	}
}

// reload reads the unfinished runs, the cached runs that were unfinished,
// every suite and every tombstone again after the feed expired past the
// cursor. s.followMu is held.
func (s *Service) reload(ctx context.Context) error {
	head, err := s.repository.ChangeHead(ctx)
	if err != nil {
		return err
	}
	records, err := s.storedRuns(ctx, artifact.RunQuery{Unfinished: true})
	if err != nil {
		return err
	}
	unfinished := make(map[string]bool, len(records))
	for _, record := range records {
		unfinished[record.ID] = true
		if err := s.applyRecord(ctx, record, false); err != nil {
			return err
		}
	}
	// A cached run that ended meanwhile is no longer listed as unfinished.
	var ended []string
	s.mu.Lock()
	for id, r := range s.runs {
		r.mu.Lock()
		if !r.terminalLocked() && !unfinished[id] {
			ended = append(ended, id)
		}
		r.mu.Unlock()
	}
	s.mu.Unlock()
	for _, id := range ended {
		if err := s.applyRun(ctx, id); err != nil {
			return err
		}
	}
	runs, suites, err := s.repository.Tombstones(ctx)
	if err != nil {
		return err
	}
	for _, t := range runs {
		s.applyDeletion(t.ID)
	}
	for _, t := range suites {
		s.applyDeletion(t.ID)
	}
	if err := s.applySuites(ctx); err != nil {
		return err
	}
	if err := s.loadDefaultRuntime(ctx); err != nil {
		return err
	}
	if err := s.loadRoleBindings(ctx); err != nil {
		return err
	}
	if err := s.loadProjects(ctx); err != nil {
		return err
	}
	s.cursor = head
	return nil
}

func (s *Service) applyChanges(ctx context.Context, changes []artifact.Change) error {
	type key struct {
		kind artifact.ChangeKind
		id   string
	}
	seen := map[key]bool{}
	suites := false
	for _, c := range changes {
		if k := (key{c.Kind, c.ID}); seen[k] {
			continue
		} else {
			seen[k] = true
		}
		var err error
		switch c.Kind {
		case artifact.ChangeRun:
			err = s.applyRun(ctx, c.ID)
		case artifact.ChangeEvents:
			err = s.applyEvents(ctx, c.ID)
		case artifact.ChangeSuite:
			suites = true
		case artifact.ChangeDeletion:
			s.applyDeletion(c.ID)
		case artifact.ChangeJob:
			s.jobChanged(c.ID)
		case artifact.ChangeBuild:
			s.buildChanged(c.ID)
		case artifact.ChangeDefaultRuntime:
			err = s.loadDefaultRuntime(ctx)
		case artifact.ChangeRoleBindings:
			err = s.loadRoleBindings(ctx)
		case artifact.ChangeProjects:
			err = s.loadProjects(ctx)
		}
		// Evaluations are read from the repository.
		if err != nil {
			return err
		}
	}
	if suites {
		return s.applySuites(ctx)
	}
	return nil
}

// applyRun reads a run's record again and caches it.
func (s *Service) applyRun(ctx context.Context, id string) error {
	record, err := s.repository.Run(ctx, id)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil // deleted; its tombstone's change follows
	}
	if err != nil {
		return err
	}
	return s.applyRecord(ctx, record, false)
}

// applyRecord caches a run's record unless the cache already holds that
// generation or a later one. A terminal run the cache does not hold is
// left out unless load is set; with load, a record this
// service cannot read is an error rather than left out.
func (s *Service) applyRecord(ctx context.Context, record artifact.RunRecord, load bool) error {
	s.mu.Lock()
	r := s.runs[record.ID]
	deleted := s.deletedRuns.has(record.ID)
	var executing bool
	if job := s.jobs[record.ID]; job != nil {
		executing = job.exec != nil
	}
	s.mu.Unlock()
	if !deleted && r == nil && !load {
		// A run this instance does not hold, most often another instance's
		// admission: the upkeep's leader may have runs to evict.
		s.nudgeRetention()
	}
	if deleted || r == nil && record.Terminal && !load {
		return nil
	}
	if r != nil {
		r.mu.Lock()
		current := record.Generation <= r.generation
		if current {
			// KeepRun changes the mark without a publication.
			r.kept = record.Kept
		}
		r.mu.Unlock()
		if current {
			if executing && !record.Terminal {
				return s.checkCancel(ctx, r)
			}
			return nil
		}
	}
	fresh, err := s.handleFromRecord(ctx, record)
	if !load && (errors.Is(err, artifact.ErrCorrupt) || errors.Is(err, artifact.ErrUnsupportedSchema)) {
		// A record this service cannot read stays out of its cache rather
		// than stopping it from following every other change.
		s.logError("reading run "+record.ID, err)
		return nil
	}
	if err != nil {
		return err
	}
	var a *artifact.Assignment
	if !record.Terminal {
		assignment, err := s.runQueue.Assignment(ctx, record.ID)
		if err != nil && !errors.Is(err, artifact.ErrNotFound) {
			return err
		}
		if err == nil {
			a = &assignment
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.deletedRuns.has(record.ID) {
		return nil
	}
	s.touchLocked(s.adoptLocked(fresh, a, false))
	s.evictLocked()
	return nil
}

// checkCancel stops a run this service executes when another instance asked
// the queue to cancel it, without waiting for the next lease renewal.
func (s *Service) checkCancel(ctx context.Context, r *Handle) error {
	a, err := s.runQueue.Assignment(ctx, r.id)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if a.CancelRequested {
		r.mu.Lock()
		r.cancel(context.Canceled)
		r.mu.Unlock()
	}
	return nil
}

// adoptLocked brings fresh, read from the repository with its queue entry
// a, into the cache and returns the cached handle. A run this service did
// not know is added; a known one takes the newer record. A queued run gets a
// job restored from its bundle when claimed, so this service may execute
// it; claimed adopts a run this service's executor just claimed whatever
// its queue state. s.mu is held.
func (s *Service) adoptLocked(fresh *Handle, a *artifact.Assignment, claimed bool) *Handle {
	fresh.shared = true
	if a != nil {
		fresh.runtimeKey = a.Runtime
	}
	r := s.runs[fresh.id]
	if r == nil {
		r = fresh
		s.queueing(r)
		s.insertLocked(r)
	} else if updated := s.updateLocked(r, fresh); updated != nil {
		r = updated
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job := s.jobs[r.id]
	switch {
	case job != nil && job.exec != nil:
	case r.terminalLocked():
		if job != nil {
			delete(s.jobs, r.id)
		}
	case claimed || a != nil && a.State == artifact.QueueQueued:
		if !claimed {
			// The queue is the truth on whether a run waits; a requeue is
			// published before the run's record.
			r.state, r.started = StateQueued, time.Time{}
		}
		if job == nil && r.bundleInfo != nil {
			s.jobs[r.id] = s.recoveredJobLocked(r)
			s.wakeLocked()
		}
	default:
		// Another instance holds the run.
		if job != nil {
			delete(s.jobs, r.id)
		}
	}
	return r
}

// updateLocked moves the cached handle r to fresh's newer record and returns
// the handle now cached, or nil when r is already as new. A run that ended
// keeps its handle; one a stale terminal state was cached for is replaced.
// s.mu is held.
func (s *Service) updateLocked(r *Handle, fresh *Handle) *Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fresh.generation <= r.generation {
		return nil
	}
	if job := s.jobs[r.id]; job != nil && job.exec != nil {
		// This service executes the run, and another instance wrote it since:
		// its lease ended there, so what it publishes now is stale.
		r.cancel(errLeaseLost)
	}
	if r.terminalLocked() && !fresh.terminalLocked() {
		s.queueing(fresh)
		fresh.runtimeKey = r.runtimeKey
		s.runs[r.id] = fresh
		r.notifyLocked()
		return fresh
	}
	attempt, ended := r.recording, r.terminalLocked()
	r.state, r.err, r.started, r.finished = fresh.state, fresh.err, fresh.started, fresh.finished
	r.interrupted, r.attempt, r.generation, r.ordinal, r.stored = fresh.interrupted, fresh.attempt, fresh.generation, fresh.ordinal, fresh.stored
	r.report, r.log, r.logTruncated, r.bundleInfo = fresh.report, fresh.log, fresh.logTruncated, fresh.bundleInfo
	r.shared = true
	if fresh.terminalLocked() {
		r.events, r.recording = fresh.events, fresh.recording
		r.runLog = nil
		r.notifyLocked()
		if !ended {
			if r.ended != nil {
				r.ended()
			}
			close(r.done)
		}
		return r
	}
	if r.attempt != attempt {
		r.events, r.recording = nil, r.attempt
	}
	r.notifyLocked()
	return r
}

// insertLocked adds a handle to the cache in admission order. s.mu is held.
func (s *Service) insertLocked(r *Handle) {
	s.runs[r.id] = r
	i := len(s.order)
	for i > 0 && s.runs[s.order[i-1]].ordinal > r.ordinal {
		i--
	}
	s.order = slices.Insert(s.order, i, r.id)
}

// adoptClaimed brings a run this service's executor e claimed, but held no
// job for, into the cache and takes it. It reports nil when the run cannot be
// read; its lease then ends and it is requeued.
func (s *Service) adoptClaimed(a artifact.Assignment, e *executor) *pendingRun {
	record, err := s.repository.Run(s.ctx, a.RunID)
	var fresh *Handle
	if err == nil {
		fresh, err = s.handleFromRecord(s.ctx, record)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.busy--
	if err != nil {
		s.logError("adopting run "+a.RunID, err)
		return nil
	}
	if s.closed || s.deletedRuns.has(a.RunID) {
		return nil
	}
	s.adoptLocked(fresh, &a, true)
	return s.takeLocked(a, e)
}

// applyEvents reads the live events another instance published for a run
// it executes.
func (s *Service) applyEvents(ctx context.Context, id string) error {
	s.mu.Lock()
	r := s.runs[id]
	local := s.jobs[id] != nil && s.jobs[id].exec != nil
	s.mu.Unlock()
	if r == nil || local {
		return nil
	}
	r.mu.Lock()
	terminal, attempt, after := r.terminalLocked(), r.recording, len(r.events)
	r.mu.Unlock()
	if terminal {
		return nil
	}
	batches, err := s.repository.RunEvents(ctx, id, after)
	if err == nil && len(batches) != 0 && batches[0].Attempt != attempt {
		after = 0
		batches, err = s.repository.RunEvents(ctx, id, 0)
	}
	if err != nil || len(batches) == 0 {
		return err
	}
	var events []instrument.Event
	for _, b := range batches {
		var decoded []instrument.Event
		if err := json.Unmarshal(b.Events, &decoded); err != nil {
			s.logError("reading the events of run "+id, err)
			return nil
		}
		events = append(events, decoded...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.terminalLocked() || r.recording != attempt || len(r.events) != after && after != 0 {
		return nil // changed meanwhile; the next change reads again
	}
	if batches[0].Attempt != r.recording {
		r.events, r.recording = nil, batches[0].Attempt
	}
	for _, e := range events {
		if e.Seq == len(r.events)+1 {
			r.events = append(r.events, e)
		}
	}
	r.notifyLocked()
	return nil
}

// applyDeletion forgets a deleted run or suite.
func (s *Service) applyDeletion(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(id, "suite-") {
		if s.deletedSuites.has(id) {
			return
		}
		if suite, ok := s.suites[id]; ok {
			for _, m := range suite.Members {
				delete(s.suiteRuns, m.RunID)
			}
		}
		delete(s.suites, id)
		s.suiteOrder = slices.DeleteFunc(s.suiteOrder, func(sid string) bool { return sid == id })
		s.deletedSuites.add(id, time.Now())
		return
	}
	if !s.deletedRuns.has(id) {
		delete(s.jobs, id)
		s.forgetRunsLocked(id)
	}
}

// applySuites caches the suites another instance recorded.
func (s *Service) applySuites(ctx context.Context) error {
	records, err := s.repository.Suites(ctx)
	if err != nil {
		return err
	}
	// Store listings are newest first; the service keeps admission order.
	for i := len(records) - 1; i >= 0; i-- {
		record := records[i]
		s.mu.Lock()
		_, known := s.suites[record.ID]
		known = known || s.deletedSuites.has(record.ID)
		s.mu.Unlock()
		if known {
			continue
		}
		if err := s.loadSuiteMembers(ctx, record); err != nil {
			return err
		}
		s.mu.Lock()
		if _, ok := s.suites[record.ID]; !ok && !s.deletedSuites.has(record.ID) {
			if suite, err := s.suiteFromRecordLocked(record); err != nil {
				s.logError("reading suite "+record.ID, err)
			} else {
				s.addSuiteLocked(suite)
			}
		}
		s.mu.Unlock()
	}
	return nil
}

// flushEvents publishes an executing run's new events for watchers on
// other instances until stop is called, which publishes the last of them.
func (s *Service) flushEvents(job *pendingRun) (stop func()) {
	if !s.shared {
		return func() {}
	}
	r := job.handle
	attempt := job.assignment.Attempt
	quit, done := make(chan struct{}), make(chan struct{})
	flushed := 0
	full := false
	flush := func() {
		r.mu.Lock()
		if r.recording != attempt || len(r.events) <= flushed || full {
			r.mu.Unlock()
			return
		}
		events := slices.Clone(r.events[flushed:])
		r.mu.Unlock()
		raw, err := json.Marshal(events)
		if err != nil {
			return
		}
		err = s.repository.AppendRunEvents(s.ctx, r.id, artifact.EventBatch{Attempt: attempt, First: events[0].Seq, Last: events[len(events)-1].Seq, Events: raw})
		switch {
		case err == nil:
			flushed += len(events)
		case errors.Is(err, artifact.ErrLimit):
			// Watchers elsewhere see the events so far; the evidence
			// archive keeps them all.
			full = true
		default:
			s.logError("publishing the events of run "+r.id, err)
		}
	}
	go func() {
		defer close(done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-quit:
				flush()
				return
			case <-t.C:
				flush()
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}

// releaseTimeout bounds handing held runs back as the service closes.
const releaseTimeout = 10 * time.Second

// Drain stops the service claiming queued runs and interactive jobs, and
// waits until the work it holds ends or ctx does. Requests keep being
// served meanwhile. With shared instances, Close then hands what is left back
// to the queue without spending an attempt, so another instance runs it.
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	for {
		s.mu.Lock()
		busy := slices.ContainsFunc(s.executors, func(e *executor) bool { return e.busy > 0 })
		freed := s.freed
		s.mu.Unlock()
		if !busy {
			return nil
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// releaseHeld returns runs this closing service holds to the queue.
func (s *Service) releaseHeld(held []artifact.Assignment) {
	if len(held) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	for _, a := range held {
		// A conflict is a run already finished or taken over.
		if _, err := s.runQueue.Release(ctx, a); err != nil && !errors.Is(err, artifact.ErrConflict) {
			s.logError("releasing a held run", err)
		}
	}
}
