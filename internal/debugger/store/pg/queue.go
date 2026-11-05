package pg

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const assignmentColumns = "id, runtime, worker, queue_state, attempt, released, lease_expires_at, cancel_requested, generation, binary_digest, binary_bytes, binary_platform"

func scanAssignment(row pgx.Row) (artifact.Assignment, error) {
	var a artifact.Assignment
	var state string
	var expires *time.Time
	var attempt, released, generation int64
	var bin artifact.RunBinary
	err := row.Scan(&a.RunID, &a.Runtime, &a.Worker, &state, &attempt, &released, &expires, &a.CancelRequested, &generation,
		&bin.Digest, &bin.Bytes, &bin.Platform)
	a.State, a.Attempt, a.Released, a.Generation = artifact.QueueState(state), int(attempt), int(released), uint64(generation)
	if expires != nil {
		a.Expires = expires.UTC()
	}
	if bin.Digest != "" {
		a.Binary = &bin
	}
	return a, err
}

func collectAssignments(rows pgx.Rows, op string) ([]artifact.Assignment, error) {
	defer rows.Close()
	out := []artifact.Assignment{}
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, storageErr(op, err)
		}
		out = append(out, a)
	}
	return out, storageErr(op, rows.Err())
}

func validWorker(worker string) bool { return worker != "" && len(worker) <= 256 }

// Claim assigns up to n queued runs of runtime with FOR UPDATE SKIP LOCKED,
// so instances claiming at once never take the same run or wait for each
// other.
func (s *Store) Claim(ctx context.Context, worker, runtime string, n int, expires time.Time) ([]artifact.Assignment, error) {
	return s.claim(ctx, worker, runtime, "", n, expires)
}

// ClaimBinary is Claim for a pool worker: it assigns only runs pinned to a
// binary of platform.
func (s *Store) ClaimBinary(ctx context.Context, worker, runtime, platform string, n int, expires time.Time) ([]artifact.Assignment, error) {
	if !validPlatform(platform) {
		if err := s.ready(ctx); err != nil {
			return nil, err
		}
		return nil, invalid("invalid claim")
	}
	return s.claim(ctx, worker, runtime, platform, n, expires)
}

// claim assigns queued runs of runtime; a platform limits it to runs pinned
// to a binary of that platform.
func (s *Store) claim(ctx context.Context, worker, runtime, platform string, n int, expires time.Time) ([]artifact.Assignment, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validWorker(worker) || n < 0 || expires.IsZero() {
		return nil, invalid("invalid claim")
	}
	if n == 0 {
		return []artifact.Assignment{}, nil
	}
	rows, err := s.pool.Query(ctx, `WITH picked AS (
			SELECT id FROM runs WHERE queue_state = 'queued' AND runtime = $1
				AND ($5 = '' OR (binary_platform = $5 AND binary_digest <> ''))
			ORDER BY priority, ordinal FOR UPDATE SKIP LOCKED LIMIT $2),
		claimed AS (
			UPDATE runs r SET queue_state = 'assigned', worker = $3, attempt = r.attempt + 1, lease_expires_at = $4
			FROM picked WHERE r.id = picked.id RETURNING r.*)
		SELECT `+assignmentColumns+` FROM claimed ORDER BY priority, ordinal`,
		runtime, n, worker, expires.UTC(), platform)
	if err != nil {
		return nil, storageErr("claim", err)
	}
	return collectAssignments(rows, "claim")
}

// Start moves an assignment the worker still holds to running.
func (s *Store) Start(ctx context.Context, a artifact.Assignment) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET queue_state = 'running'
		WHERE id = $1 AND worker = $2 AND attempt = $3 AND queue_state = 'assigned'`, a.RunID, a.Worker, a.Attempt)
	if err != nil {
		return storageErr("start", err)
	}
	if tag.RowsAffected() == 0 {
		return artifact.ErrConflict
	}
	return nil
}

// Renew extends the worker's leases in one statement.
func (s *Store) Renew(ctx context.Context, worker string, expires time.Time) ([]artifact.Assignment, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validWorker(worker) || expires.IsZero() {
		return nil, invalid("invalid renewal")
	}
	rows, err := s.pool.Query(ctx, `UPDATE runs SET lease_expires_at = $2
		WHERE worker = $1 AND queue_state IN ('assigned', 'running')
		RETURNING `+assignmentColumns, worker, expires.UTC())
	if err != nil {
		return nil, storageErr("renew", err)
	}
	out, err := collectAssignments(rows, "renew")
	slices.SortFunc(out, func(x, y artifact.Assignment) int { return cmp.Compare(x.RunID, y.RunID) })
	return out, err
}

// RequestCancel marks a run; one no worker holds leaves the queue.
func (s *Store) RequestCancel(ctx context.Context, id string) (artifact.Assignment, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.Assignment{}, err
	}
	var out artifact.Assignment
	err := s.inTx(ctx, "cancel", func(tx pgx.Tx) error {
		var terminal bool
		err := tx.QueryRow(ctx, "SELECT terminal FROM runs WHERE id = $1 FOR UPDATE", id).Scan(&terminal)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if terminal {
			return artifact.ErrConflict
		}
		out, err = scanAssignment(tx.QueryRow(ctx, `UPDATE runs SET cancel_requested = true,
				queue_state = CASE WHEN worker = '' THEN NULL ELSE queue_state END
			WHERE id = $1 RETURNING id, runtime, worker, coalesce(queue_state, ''), attempt, released, lease_expires_at, cancel_requested, generation,
				binary_digest, binary_bytes, binary_platform`, id))
		return err
	})
	return out, err
}

// Expired lists leases that ended before now.
func (s *Store) Expired(ctx context.Context, now time.Time) ([]artifact.Assignment, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+assignmentColumns+` FROM runs
		WHERE queue_state IN ('assigned', 'running') AND lease_expires_at < $1 ORDER BY ordinal`, now.UTC())
	if err != nil {
		return nil, storageErr("expired leases", err)
	}
	return collectAssignments(rows, "expired leases")
}

// Requeue returns an expired assignment's run to the queue, once per lease.
func (s *Store) Requeue(ctx context.Context, a artifact.Assignment, now time.Time) (uint64, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	var generation int64
	err := s.pool.QueryRow(ctx, `UPDATE runs SET queue_state = 'queued', worker = '', lease_expires_at = NULL, generation = generation + 1, updated_at = now()
		WHERE id = $1 AND worker = $2 AND attempt = $3 AND queue_state IN ('assigned', 'running') AND lease_expires_at < $4
		RETURNING generation`, a.RunID, a.Worker, a.Attempt, now.UTC()).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, artifact.ErrConflict
	}
	if err != nil {
		return 0, storageErr("requeue", err)
	}
	return uint64(generation), nil
}

// Release returns a held run to the queue before its lease ends.
func (s *Store) Release(ctx context.Context, a artifact.Assignment) (uint64, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	var generation int64
	err := s.pool.QueryRow(ctx, `UPDATE runs SET queue_state = 'queued', worker = '', lease_expires_at = NULL,
			released = released + 1, generation = generation + 1, updated_at = now()
		WHERE id = $1 AND worker = $2 AND attempt = $3 AND queue_state IN ('assigned', 'running')
		RETURNING generation`, a.RunID, a.Worker, a.Attempt).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, artifact.ErrConflict
	}
	if err != nil {
		return 0, storageErr("release", err)
	}
	return uint64(generation), nil
}

// Assignment returns an unfinished run's queue entry.
func (s *Store) Assignment(ctx context.Context, id string) (artifact.Assignment, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.Assignment{}, err
	}
	a, err := scanAssignment(s.pool.QueryRow(ctx, "SELECT "+assignmentColumns+" FROM runs WHERE id = $1 AND queue_state IS NOT NULL", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.Assignment{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.Assignment{}, storageErr("assignment", err)
	}
	return a, nil
}

// validPlatform reports whether p is an "os/arch" a run's binary can name.
func validPlatform(p string) bool {
	return artifact.RunBinary{Digest: "sha256:" + strings.Repeat("0", 64), Bytes: 1, Platform: p}.Valid()
}
