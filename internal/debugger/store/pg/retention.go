package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// evictable selects, for runs aliased r, the runs retention may remove:
// terminal, not kept, in no suite and the origin of no artifact's head
// revision.
const evictable = `r.terminal AND NOT r.kept
	AND NOT EXISTS (SELECT 1 FROM suite_members m WHERE m.run_id = r.id)
	AND NOT EXISTS (SELECT 1 FROM revisions v JOIN artifacts a ON a.head_revision_id = v.id WHERE v.origin_run = r.id)`

// retainedBytes is the size of every distinct object a row other than an
// evictable run or its evaluations references: what storage holds once
// retention has removed everything it may. Admission counts evictable runs
// as free, so history never refuses a run; retention brings the stored
// bytes back within the limit.
func retainedBytes(ctx context.Context, tx pgx.Tx) (int64, error) {
	return bytesWithout(ctx, tx, `SELECT r.id FROM runs r WHERE `+evictable)
}

// bytesWithout is the size of every distinct object a row references,
// leaving out the runs the query gone selects and their evaluations.
func bytesWithout(ctx context.Context, tx pgx.Tx, gone string, args ...any) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, `WITH gone AS (`+gone+`)
		SELECT coalesce(sum(size), 0)::bigint FROM (SELECT DISTINCT digest, size FROM (
			SELECT digest, bytes AS size FROM revisions
			UNION ALL SELECT evidence_digest, evidence_bytes FROM runs WHERE evidence_digest <> '' AND id NOT IN (SELECT id FROM gone)
			UNION ALL SELECT bundle_digest, bundle_bytes FROM runs WHERE bundle_digest <> '' AND id NOT IN (SELECT id FROM gone)
			UNION ALL SELECT binary_digest, binary_bytes FROM runs WHERE binary_digest <> '' AND NOT terminal AND id NOT IN (SELECT id FROM gone)
			UNION ALL SELECT result_digest, result_bytes FROM evaluations WHERE run_id NOT IN (SELECT id FROM gone)
			UNION ALL SELECT source_digest, source_bytes FROM evaluations WHERE run_id NOT IN (SELECT id FROM gone)
			UNION ALL SELECT digest, bytes FROM capture_segments
			UNION ALL SELECT digest, bytes FROM build_inputs
			UNION ALL SELECT binary_digest, binary_bytes FROM builds WHERE binary_digest <> ''
			UNION ALL SELECT staged_binary_digest, staged_binary_bytes FROM builds WHERE staged_binary_digest <> ''
			UNION ALL SELECT binary_digest, binary_bytes FROM runtimes WHERE binary_digest <> '') refs) t`, args...).Scan(&n)
	return n, err
}

// KeepRun marks a run kept or not. The mark is no publication: the run's
// generation stays. The runs trigger reports the update in the change feed.
func (s *Store) KeepRun(ctx context.Context, id string, kept bool) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, "UPDATE runs SET kept = $2 WHERE id = $1", id, kept)
	if err != nil {
		return storageErr("keep run", err)
	}
	if tag.RowsAffected() == 0 {
		return artifact.ErrNotFound
	}
	return nil
}

// EvictRuns removes one batch of the oldest evictable runs the policy no
// longer keeps, under the installation lock so that counts and bytes hold
// while it decides. Storage is estimated from each run's own objects and
// measured again by the next call.
func (s *Store) EvictRuns(ctx context.Context, p artifact.RetentionPolicy, now time.Time) ([]artifact.Evicted, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var out []artifact.Evicted
	err := s.inTx(ctx, "evict runs", func(tx pgx.Tx) error {
		out = nil
		if _, err := lockInstallation(ctx, tx); err != nil {
			return err
		}
		usage, err := runUsage(ctx, tx)
		if err != nil {
			return err
		}
		used, err := storedBytes(ctx, tx)
		if err != nil {
			return err
		}
		eviction := p.Evict(usage, used, s.limits.StorageBytes)
		// Every candidate is read: a project's own policy can remove a run
		// younger than one the installation's keeps.
		rows, err := tx.Query(ctx, `SELECT r.id, r.project_id, r.admitted_at, r.evidence_bytes + r.bundle_bytes FROM runs r WHERE `+evictable+`
			ORDER BY r.ordinal FOR UPDATE OF r`)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() && len(ids) < artifact.EvictionBatch {
			var id, project string
			var admitted time.Time
			var size int64
			if err := rows.Scan(&id, &project, &admitted, &size); err != nil {
				rows.Close()
				return err
			}
			reason := eviction.Next(project, admitted, now, size)
			if reason == "" {
				continue
			}
			ids = append(ids, id)
			out = append(out, artifact.Evicted{ID: id, Reason: reason})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return removeRuns(ctx, tx, ids, now)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runUsage counts each project's runs and the bytes of their evidence and
// bundles.
func runUsage(ctx context.Context, tx pgx.Tx) (map[string]artifact.RunUsage, error) {
	rows, err := tx.Query(ctx, "SELECT project_id, count(*), COALESCE(sum(evidence_bytes + bundle_bytes), 0) FROM runs GROUP BY project_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]artifact.RunUsage{}
	for rows.Next() {
		var project string
		var u artifact.RunUsage
		if err := rows.Scan(&project, &u.Runs, &u.Bytes); err != nil {
			return nil, err
		}
		out[project] = u
	}
	return out, rows.Err()
}

// PruneRuns removes every evictable run q selects in one transaction under
// the installation lock. A dry run takes the same locks and measures the
// same way, then commits nothing.
func (s *Store) PruneRuns(ctx context.Context, q artifact.PruneQuery, dryRun bool, now time.Time) (artifact.Pruned, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.Pruned{}, err
	}
	out := artifact.Pruned{Runs: []string{}}
	if q.Projects != nil && len(q.Projects) == 0 {
		return out, nil
	}
	var before any
	if !q.Before.IsZero() {
		before = q.Before.UTC()
	}
	err := s.inTx(ctx, "prune runs", func(tx pgx.Tx) error {
		if _, err := lockInstallation(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT r.id FROM runs r WHERE `+evictable+`
			AND ($1::text[] IS NULL OR r.project_id = ANY($1)) AND ($2::timestamptz IS NULL OR r.admitted_at < $2)
			AND ($3 = '' OR r.scenario = $3) AND ($4 = '' OR r.runtime_name = $4) AND ($5 = '' OR r.result = $5)
			ORDER BY r.ordinal FOR UPDATE OF r`, q.Projects, before, q.Scenario, q.Runtime, q.Result)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		used, err := storedBytes(ctx, tx)
		if err != nil {
			return err
		}
		left, err := bytesWithout(ctx, tx, "SELECT unnest($1::text[]) AS id", ids)
		if err != nil {
			return err
		}
		out = artifact.Pruned{Runs: ids, Bytes: used - left}
		if dryRun {
			return nil
		}
		return removeRuns(ctx, tx, ids, now)
	})
	if err != nil {
		return artifact.Pruned{}, err
	}
	return out, nil
}

// RunStats counts the stored runs, the kept ones and the oldest admission.
func (s *Store) RunStats(ctx context.Context) (artifact.RunStats, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.RunStats{}, err
	}
	var st artifact.RunStats
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE kept), min(admitted_at) FROM runs").Scan(&st.Runs, &st.Kept, &oldest)
	if err != nil {
		return artifact.RunStats{}, storageErr("run stats", err)
	}
	if oldest != nil {
		st.Oldest = oldest.UTC()
	}
	return st, nil
}
