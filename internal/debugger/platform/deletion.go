package platform

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// deletionMemory is how long a service remembers a deleted run or suite ID: a
// read naming it in that time is "deleted", later only unknown, as after the
// repository drops its tombstone. It outlasts any read or adoption under way
// when the deletion happened.
const deletionMemory = 24 * time.Hour

// deletedIDs remembers deleted IDs for deletionMemory after their deletion,
// so the set follows the rate of deletions rather than all of history. s.mu
// guards it.
type deletedIDs struct {
	at     map[string]time.Time
	pruned time.Time
}

func newDeletedIDs() deletedIDs { return deletedIDs{at: map[string]time.Time{}} }

func (d *deletedIDs) has(id string) bool {
	_, ok := d.at[id]
	return ok
}

// add remembers id, deleted at, unless that is longer ago than
// deletionMemory, and forgets the IDs deleted before that, at most once per
// hour.
func (d *deletedIDs) add(id string, at time.Time) {
	now := time.Now()
	horizon := now.Add(-deletionMemory)
	if at.After(horizon) {
		if old, ok := d.at[id]; !ok || at.After(old) {
			d.at[id] = at
		}
	}
	if now.Sub(d.pruned) < time.Hour {
		return
	}
	d.pruned = now
	for id, at := range d.at {
		if !at.After(horizon) {
			delete(d.at, id)
		}
	}
}

// errRunDeleted answers every read naming a deleted run.
func errRunDeleted() error {
	return &Error{Code: CodeNotFound, Message: "run was deleted", cause: ErrUnknownRun}
}

func errSuiteDeleted() error { return &Error{Code: CodeNotFound, Message: "suite was deleted"} }

// unknownRunLocked is the error for a run ID the service does not hold.
func (s *Service) unknownRunLocked(id string) error {
	if s.deletedRuns.has(id) {
		return errRunDeleted()
	}
	return ErrUnknownRun
}

func (s *Service) unknownSuiteLocked(id string) error {
	if s.deletedSuites.has(id) {
		return errSuiteDeleted()
	}
	return errUnknownSuite()
}

// recoverTombstones restores the deleted run and suite IDs of a workspace.
func (s *Service) recoverTombstones() error {
	runs, suites, err := s.repository.Tombstones(context.Background())
	if err != nil {
		return err
	}
	for _, t := range runs {
		s.deletedRuns.add(t.ID, t.Deleted)
		s.listings.drop(t.ID)
	}
	for _, t := range suites {
		s.deletedSuites.add(t.ID, t.Deleted)
	}
	return nil
}

// DeleteRun deletes a finished run that belongs to no suite: its report,
// evidence, input bundle and recorded evaluations. Every later read naming it
// is not_found "run was deleted". A queued or running run, or a suite member,
// is a conflict. Only the run's owner or an admin may delete it.
func (s *Service) DeleteRun(ctx context.Context, caller Caller, id string) (err error) {
	defer s.audit(&caller, "DeleteRun", &err)()
	if err := s.begin(ctx, caller, authz.RunDelete); err != nil {
		return err
	}
	if err := s.load(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return classify(ErrClosed, CodeUnavailable)
	}
	r, ok := s.runs[id]
	if !ok {
		return classify(s.unknownRunLocked(id), CodeNotFound)
	}
	if err := s.mayChangeRunLocked(caller, r); err != nil {
		return err
	}
	r.mu.Lock()
	terminal := r.terminalLocked()
	r.mu.Unlock()
	if !terminal {
		return &Error{Code: CodeConflict, Message: "run is queued or running; cancel it before deleting it"}
	}
	if s.suiteRuns[id] {
		for _, sid := range s.suiteOrder {
			if slices.ContainsFunc(s.suites[sid].Members, func(m SuiteMember) bool { return m.RunID == id }) {
				return &Error{Code: CodeConflict, Message: fmt.Sprintf("run belongs to suite %s; delete the suite to delete its runs", sid)}
			}
		}
		return &Error{Code: CodeConflict, Message: "run belongs to a suite being submitted"}
	}
	if s.repository != nil {
		if err := s.repository.DeleteRun(ctx, id, time.Now()); err != nil {
			return deletionError(err)
		}
	}
	s.forgetRunsLocked(id)
	return nil
}

// DeleteSuite deletes a suite and its member runs, each as DeleteRun would.
// A member that is queued or running is a conflict and nothing is deleted.
func (s *Service) DeleteSuite(ctx context.Context, caller Caller, id string) (err error) {
	defer s.audit(&caller, "DeleteSuite", &err)()
	if err := s.begin(ctx, caller, authz.RunDelete); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return classify(ErrClosed, CodeUnavailable)
	}
	suite, ok := s.suites[id]
	if !ok {
		return s.unknownSuiteLocked(id)
	}
	if err := s.authorizeIn(caller, authz.RunDelete, suite.Project); err != nil {
		return err
	}
	members := []string{}
	for _, m := range suite.Members {
		if m.RunID == "" {
			continue
		}
		r, ok := s.runs[m.RunID]
		if !ok {
			return classify(fmt.Errorf("%w: suite member run is missing", ErrPersistence), CodeStorageError)
		}
		r.mu.Lock()
		terminal := r.terminalLocked()
		r.mu.Unlock()
		if !terminal {
			return &Error{Code: CodeConflict, Message: "a member run is queued or running; cancel it before deleting the suite"}
		}
		members = append(members, m.RunID)
	}
	if s.repository != nil {
		if err := s.repository.DeleteSuite(ctx, id, time.Now()); err != nil {
			return deletionError(err)
		}
	}
	delete(s.suites, id)
	s.suiteOrder = slices.DeleteFunc(s.suiteOrder, func(sid string) bool { return sid == id })
	for _, m := range members {
		delete(s.suiteRuns, m)
	}
	s.deletedSuites.add(id, time.Now())
	s.forgetRunsLocked(members...)
	return nil
}

// deletionError maps a repository refusal. The service checked the run's
// state first, so a repository conflict means storage disagrees with it.
func deletionError(err error) error {
	if errors.Is(err, artifact.ErrNotFound) || errors.Is(err, artifact.ErrConflict) {
		err = fmt.Errorf("%w: run journal disagrees with the service: %w", ErrPersistence, err)
	}
	return artifactError(err)
}
