package pg

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// installationRow is the installation's counters, read under its row lock.
type installationRow struct {
	generation   int64
	runOrdinal   int64
	suiteOrdinal int64
}

// lockInstallation takes the installation's row lock. Every write that adds
// stored bytes or checks a count limit takes it first, in every instance, so
// those checks see all earlier committed writes and no later one commits
// before this transaction ends. It is always taken before any other row lock.
func lockInstallation(ctx context.Context, tx pgx.Tx) (installationRow, error) {
	var r installationRow
	err := tx.QueryRow(ctx, "SELECT generation, run_ordinal, suite_ordinal FROM installation FOR UPDATE").Scan(&r.generation, &r.runOrdinal, &r.suiteOrdinal)
	return r, err
}

// blob is a stored object a write adds.
type blob struct {
	digest string
	size   int64
}

// storedBytes is the size of every distinct object any row references.
func storedBytes(ctx context.Context, tx pgx.Tx) (int64, error) {
	var n int64
	err := tx.QueryRow(ctx, "SELECT coalesce(sum(size), 0)::bigint FROM (SELECT DISTINCT digest, size FROM object_refs) t").Scan(&n)
	return n, err
}

// newBytes is the size of the blobs no row references yet, counting each
// digest once.
func newBytes(ctx context.Context, tx pgx.Tx, blobs ...blob) (int64, error) {
	var n int64
	seen := map[string]bool{}
	for _, b := range blobs {
		if b.digest == "" || b.size == 0 || seen[b.digest] {
			continue
		}
		seen[b.digest] = true
		var referenced bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM object_refs WHERE digest = $1)", b.digest).Scan(&referenced); err != nil {
			return 0, err
		}
		if !referenced {
			n += b.size
		}
	}
	return n, nil
}

// reserve is what a write changes in the reservations the limit check makes
// for unfinished runs, open captures and unfinished builds.
type reserve struct {
	// excludeRun, excludeCapture and excludeBuild name the record being
	// written, whose reservation the write recomputes.
	excludeRun     string
	extraRuns      int64
	excludeCapture string
	extraCapture   int64
	excludeBuild   string
	extraBuilds    int64
}

// checkSpace refuses a write that, with the bytes it adds and the room
// unfinished runs (RunBytes each), open captures (CaptureAllowance less what
// they hold) and queued or running builds without a stored binary
// (BuildBytes each) have reserved, would exceed
// Limits.StorageBytes. Evictable runs count as free (retainedBytes). It must
// run under the installation lock.
func (s *Store) checkSpace(ctx context.Context, tx pgx.Tx, additional int64, r reserve) error {
	used, err := retainedBytes(ctx, tx)
	if err != nil {
		return err
	}
	var unfinished, open int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM runs WHERE NOT terminal AND id <> $1", r.excludeRun).Scan(&unfinished); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, "SELECT coalesce(sum($2::bigint - bytes), 0)::bigint FROM captures WHERE NOT closed AND NOT memory_only AND id <> $1",
		r.excludeCapture, artifact.CaptureAllowance).Scan(&open); err != nil {
		return err
	}
	var building int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM builds WHERE state IN ('queued', 'running') AND staged_binary_digest = '' AND id <> $1",
		r.excludeBuild).Scan(&building); err != nil {
		return err
	}
	limit := s.limits.StorageBytes
	reserved := open + r.extraCapture
	each := func(n, bytes int64) {
		// Saturate rather than overflow caller-configured limits.
		switch {
		case n <= 0 || bytes <= 0:
		case n > limit/bytes:
			reserved += limit
		default:
			reserved += n * bytes
		}
	}
	each(unfinished+r.extraRuns, s.limits.RunBytes)
	each(building+r.extraBuilds, s.limits.BuildBytes)
	if used > limit || additional > limit-used || reserved > limit-used-additional {
		return artifact.ErrLimit
	}
	return nil
}

// Usage is the bytes of every object any row references, counted once per
// digest, and may exceed Limits.StorageBytes when the limit was lowered after
// they were written.
func (s *Store) Usage(ctx context.Context) (int64, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	var n int64
	err := s.pool.QueryRow(ctx, "SELECT coalesce(sum(size), 0)::bigint FROM (SELECT DISTINCT digest, size FROM object_refs) t").Scan(&n)
	return n, storageErr("usage", err)
}
