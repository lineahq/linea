package memory

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// KeepRun marks a run kept or not, as the PostgreSQL store does: the run's
// generation stays and the change feed reports the run.
func (s *Store) KeepRun(ctx context.Context, id string, kept bool) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.runs == nil {
		return artifact.ErrNotFound
	}
	r, ok := d.runs.Records[id]
	if !ok {
		return artifact.ErrNotFound
	}
	r.Kept = kept
	next := *d.runs
	next.Records = maps.Clone(d.runs.Records)
	next.Records[id] = r
	size := d.runsSize + recordDelta(d.runs.Records, id, r)
	if size > s.limits.CatalogBytes {
		return artifact.ErrLimit
	}
	d.runs, d.runsSize = &next, size
	d.change(artifact.ChangeRun, id)
	return nil
}

// EvictRuns removes one batch of the oldest evictable runs the policy no
// longer keeps. Storage is estimated from each run's own objects, as the
// PostgreSQL store does.
func (s *Store) EvictRuns(ctx context.Context, p artifact.RetentionPolicy, now time.Time) ([]artifact.Evicted, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.runs == nil {
		return nil, artifact.ErrNotFound
	}
	candidates := d.evictable(func(artifact.RunRecord) bool { return true })
	usage := map[string]artifact.RunUsage{}
	for _, r := range d.runs.Records {
		u := usage[r.Project]
		u.Runs, u.Bytes = u.Runs+1, u.Bytes+r.Bytes+r.BundleBytes
		usage[r.Project] = u
	}
	used, err := d.measure(1<<63 - 1)
	if err != nil {
		return nil, err
	}
	eviction := p.Evict(usage, used, s.limits.StorageBytes)
	out := []artifact.Evicted{}
	ids := []string{}
	for _, r := range candidates {
		if len(ids) == artifact.EvictionBatch {
			break
		}
		reason := eviction.Next(r.Project, d.admitted[r.ID], now, r.Bytes+r.BundleBytes)
		if reason == "" {
			continue
		}
		ids = append(ids, r.ID)
		out = append(out, artifact.Evicted{ID: r.ID, Reason: reason})
	}
	if len(ids) == 0 {
		return out, nil
	}
	if err := s.removeRuns(ids, now); err != nil {
		return nil, err
	}
	return out, nil
}

// evictable lists the evictable runs that match, oldest first. d.mu is held.
func (d *Data) evictable(match func(artifact.RunRecord) bool) []artifact.RunRecord {
	origins := map[string]bool{}
	for _, a := range d.cat.Artifacts {
		if o := d.cat.Revisions[a.Head].Origin; o != nil && o.RunID != "" {
			origins[o.RunID] = true
		}
	}
	var out []artifact.RunRecord
	for id, r := range d.runs.Records {
		if r.Terminal && !r.Kept && d.suiteOf(id) == "" && !origins[id] && match(r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b artifact.RunRecord) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return out
}

// removeRuns deletes runs with their evaluations, keeping tombstones and
// reporting each deletion. d.mu is held.
func (s *Store) removeRuns(ids []string, now time.Time) error {
	d := s.d
	if err := s.publishDeletion(d.withoutRuns(ids, now)); err != nil {
		return err
	}
	for _, id := range ids {
		delete(d.admitted, id)
		d.change(artifact.ChangeDeletion, id)
	}
	return nil
}

// PruneRuns removes every evictable run q selects, as the PostgreSQL store
// does. Bytes counts the stored objects only those runs referenced.
func (s *Store) PruneRuns(ctx context.Context, q artifact.PruneQuery, dryRun bool, now time.Time) (artifact.Pruned, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.Pruned{}, err
	}
	if d.runs == nil {
		return artifact.Pruned{}, artifact.ErrNotFound
	}
	match := func(r artifact.RunRecord) bool {
		x := artifact.RunIndex{}
		if r.Index != nil {
			x = *r.Index
		} else if q.Scenario != "" || q.Runtime != "" || q.Result != "" {
			return false
		}
		return (q.Projects == nil || slices.Contains(q.Projects, r.Project)) &&
			(q.Before.IsZero() || d.admitted[r.ID].Before(q.Before)) &&
			(q.Scenario == "" || x.Scenario == q.Scenario) && (q.Runtime == "" || x.Runtime == q.Runtime) && (q.Result == "" || x.Result == q.Result)
	}
	out := artifact.Pruned{Runs: []string{}}
	gone := map[string]bool{}
	for _, r := range d.evictable(match) {
		out.Runs = append(out.Runs, r.ID)
		gone[r.ID] = true
	}
	live, left := d.live(nil), d.live(gone)
	for dg := range live {
		if !left[dg] {
			out.Bytes += int64(len(d.blobs[dg]))
		}
	}
	if dryRun || len(out.Runs) == 0 {
		return out, nil
	}
	if err := s.removeRuns(out.Runs, now); err != nil {
		return artifact.Pruned{}, err
	}
	return out, nil
}

// RunStats counts the stored runs, the kept ones and the oldest admission.
func (s *Store) RunStats(ctx context.Context) (artifact.RunStats, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.RunStats{}, err
	}
	var st artifact.RunStats
	if d.runs == nil {
		return st, nil
	}
	for id, r := range d.runs.Records {
		st.Runs++
		if r.Kept {
			st.Kept++
		}
		if at, ok := d.admitted[id]; ok && (st.Oldest.IsZero() || at.Before(st.Oldest)) {
			st.Oldest = at
		}
	}
	return st, nil
}
