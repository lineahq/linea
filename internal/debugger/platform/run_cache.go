package platform

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// DefaultRunCache is how many terminal runs a service with a repository
// keeps in memory after reading them.
const DefaultRunCache = 1000

// A service with a repository holds handles for its unfinished runs, the
// members of its suites and the RunCache terminal runs read most recently.
// Every other run stays in the repository until a read names it (load).

// touchLocked marks r as just read, for evictLocked. s.mu is held.
func (s *Service) touchLocked(r *Handle) {
	s.uses++
	r.used = s.uses
}

// load caches run id from the repository when the service does not hold
// it. An unknown or deleted run is not an error: the caller's lookup
// reports it.
func (s *Service) load(ctx context.Context, id string) error {
	if s.repository == nil {
		return nil
	}
	s.mu.Lock()
	r, held := s.runs[id]
	if held {
		s.touchLocked(r)
	}
	skip := held || s.deletedRuns.has(id) || s.closed
	s.mu.Unlock()
	if skip {
		return nil
	}
	record, err := s.repository.Run(ctx, id)
	if errors.Is(err, artifact.ErrNotFound) {
		return nil
	}
	if err == nil {
		err = s.applyRecord(ctx, record, true)
	}
	return artifactError(err)
}

// evictCacheLocked drops the least recently read terminal runs beyond the
// cache's size. Unfinished runs, suite members, runs with a job and runs
// whose terminal state the repository does not hold yet stay. s.mu is held.
func (s *Service) evictCacheLocked() {
	if len(s.runs) <= s.runCache {
		return
	}
	idle := make([]*Handle, 0, len(s.runs))
	for id, r := range s.runs {
		if s.jobs[id] != nil || s.suiteRuns[id] {
			continue
		}
		r.mu.Lock()
		stored := r.stored
		r.mu.Unlock()
		if stored {
			idle = append(idle, r)
		}
	}
	if len(idle) <= s.runCache {
		return
	}
	slices.SortFunc(idle, func(a, b *Handle) int { return cmp.Compare(a.used, b.used) })
	gone := make(map[string]bool, len(idle)-s.runCache)
	for _, r := range idle[:len(idle)-s.runCache] {
		delete(s.runs, r.id)
		gone[r.id] = true
	}
	s.order = slices.DeleteFunc(s.order, func(id string) bool { return gone[id] })
}

// storedRuns reads every run q selects, oldest first.
func (s *Service) storedRuns(ctx context.Context, q artifact.RunQuery) ([]artifact.RunRecord, error) {
	q.Limit = artifact.MaxRunPage
	var records []artifact.RunRecord
	for {
		page, err := s.repository.ListRuns(ctx, q)
		if err != nil {
			return nil, err
		}
		records = append(records, page.Runs...)
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	slices.Reverse(records)
	return records, nil
}

// indexRuns indexes the runs stored before run indexes existed, which list
// unfiltered until then. A run that cannot be read stays unindexed; reading it
// reports why.
func (s *Service) indexRuns(ctx context.Context) error {
	q := artifact.RunQuery{Unindexed: true, Limit: artifact.MaxRunPage}
	for {
		page, err := s.repository.ListRuns(ctx, q)
		if err != nil {
			return err
		}
		for _, record := range page.Runs {
			r, err := s.handleFromRecord(ctx, record)
			if errors.Is(err, artifact.ErrCorrupt) || errors.Is(err, artifact.ErrUnsupportedSchema) {
				s.logError("indexing run "+record.ID, err)
				continue
			}
			if err != nil {
				return err
			}
			if err := s.repository.IndexRun(ctx, r.id, runIndex(r.state, r.report)); err != nil && !errors.Is(err, artifact.ErrNotFound) {
				return err
			}
		}
		if page.NextCursor == "" {
			return nil
		}
		q.Cursor = page.NextCursor
	}
}

// loadSuiteMembers caches the member runs of a stored suite, which the
// service keeps while it holds the suite. A record it cannot decode is left
// to suiteFromRecordLocked to refuse.
func (s *Service) loadSuiteMembers(ctx context.Context, record artifact.SuiteRecord) error {
	var m suiteMetadata
	if decodeStored(record.Metadata, &m) != nil {
		return nil
	}
	for _, member := range m.Members {
		if member.RunID == "" {
			continue
		}
		if err := s.load(ctx, member.RunID); err != nil {
			return err
		}
	}
	return nil
}
