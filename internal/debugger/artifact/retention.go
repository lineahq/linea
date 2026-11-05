package artifact

import (
	"context"
	"time"
)

// RetentionPolicy bounds the terminal runs a repository keeps:
// at most Runs runs, none admitted longer than Age ago. A zero field does
// not bound. Limits.StorageBytes bounds their stored bytes as well.
// Projects replace Runs and Age for their own runs and bound those runs'
// bytes; the other projects' runs share Runs and Age.
type RetentionPolicy struct {
	Runs     int
	Age      time.Duration
	Projects []ProjectRetention
}

// ProjectRetention is one project's own retention: at most Runs of its runs,
// none admitted longer than Age ago, their evidence and bundles within
// Bytes. A zero field does not bound; Runs and Age replace the
// installation's only when either is set.
type ProjectRetention struct {
	Project string
	Runs    int
	Age     time.Duration
	Bytes   int64
}

// RunUsage is what one project's runs hold: their count and the bytes of
// their evidence and bundles.
type RunUsage struct {
	Runs  int64
	Bytes int64
}

// Eviction decides, for the evictable runs of a repository oldest first,
// which a policy removes. A store feeds it every candidate in order until
// it has a batch.
type Eviction struct {
	policy RetentionPolicy
	usage  map[string]RunUsage
	shared int64
	used   int64
	limit  int64
	own    map[string]ProjectRetention
}

// Evict starts an eviction of p over a repository whose runs hold usage per
// project and whose stored bytes are used of limit.
func (p RetentionPolicy) Evict(usage map[string]RunUsage, used, limit int64) *Eviction {
	e := &Eviction{policy: p, usage: map[string]RunUsage{}, used: used, limit: limit, own: map[string]ProjectRetention{}}
	for _, r := range p.Projects {
		e.own[r.Project] = r
	}
	for project, u := range usage {
		e.usage[project] = u
		if r, ok := e.own[project]; !ok || r.Runs == 0 && r.Age == 0 {
			e.shared += u.Runs
		}
	}
	return e
}

// Next reports why the policy removes a run of project, admitted at
// admitted and holding size bytes, and counts it removed; "" keeps it.
func (e *Eviction) Next(project string, admitted, now time.Time, size int64) string {
	u := e.usage[project]
	r, own := e.own[project]
	p, runs := RetentionPolicy{Runs: e.policy.Runs, Age: e.policy.Age}, e.shared
	replaced := own && (r.Runs > 0 || r.Age > 0)
	if replaced {
		p, runs = RetentionPolicy{Runs: r.Runs, Age: r.Age}, u.Runs
	}
	over := e.used > e.limit || own && r.Bytes > 0 && u.Bytes > r.Bytes
	reason := p.Reason(runs, admitted, now, over)
	if reason == "" {
		return ""
	}
	u.Runs, u.Bytes = u.Runs-1, u.Bytes-size
	e.usage[project], e.used = u, e.used-size
	if !replaced {
		e.shared--
	}
	return reason
}

// Eviction reasons: what the policy exceeded when a run was evicted.
const (
	EvictCount   = "count"
	EvictAge     = "age"
	EvictStorage = "storage"
)

// Reason is why p removes the oldest remaining evictable run, admitted at
// admitted, while a repository holds runs runs and its stored bytes are or
// are not over its limit; "" when p keeps it. Count comes first, then age,
// then storage.
func (p RetentionPolicy) Reason(runs int64, admitted, now time.Time, overStorage bool) string {
	switch {
	case p.Runs > 0 && runs > int64(p.Runs):
		return EvictCount
	case p.Age > 0 && admitted.Before(now.Add(-p.Age)):
		return EvictAge
	case overStorage:
		return EvictStorage
	}
	return ""
}

// EvictionBatch bounds the runs one EvictRuns call removes.
const EvictionBatch = 500

// Evicted is a run EvictRuns removed and why.
type Evicted struct {
	ID     string
	Reason string
}

// Retention removes the runs a retention policy no longer keeps.
//
// A run is evictable when it is terminal, not kept, a member of no suite and
// the origin of no artifact's head revision. Unfinished runs and the others
// are never evicted, and they count toward the policy.
type Retention interface {
	// KeepRun marks a run kept, or no longer kept. The run's generation
	// stays; the change feed reports the run (ChangeRun) so other instances
	// read the mark. An unknown or deleted run is ErrNotFound.
	KeepRun(ctx context.Context, id string, kept bool) error
	// EvictRuns deletes, as DeleteRun would, at most EvictionBatch of the
	// oldest evictable runs that put the repository beyond p at now: by
	// count, by age, or while the stored bytes exceed Limits.StorageBytes.
	// Storage may be estimated; a caller repeats until nothing is evicted.
	EvictRuns(ctx context.Context, p RetentionPolicy, now time.Time) ([]Evicted, error)
	// PruneRuns deletes, as DeleteRun would, every evictable run q selects,
	// oldest first, and reports them with the bytes of stored objects no
	// other row references. With dryRun it deletes nothing and reports
	// what it would delete.
	PruneRuns(ctx context.Context, q PruneQuery, dryRun bool, now time.Time) (Pruned, error)
	// RunStats counts the runs stored.
	RunStats(ctx context.Context) (RunStats, error)
}

// PruneQuery selects the evictable runs a manual prune removes: in Projects
// (nil: every project; empty: none), admitted before Before (zero: any
// time), whose index matches Scenario, Runtime and Result where set. An
// unindexed run matches no index filter.
type PruneQuery struct {
	Projects []string
	Before   time.Time
	Scenario string
	Runtime  string
	Result   string
}

// Pruned is what PruneRuns removed or, in a dry run, would remove: the runs,
// oldest first, and the bytes of the objects only they referenced.
type Pruned struct {
	Runs  []string
	Bytes int64
}

// RunStats counts the runs a repository stores: all of them, those kept,
// and when the oldest was admitted (zero without runs).
type RunStats struct {
	Runs   int64
	Kept   int64
	Oldest time.Time
}
