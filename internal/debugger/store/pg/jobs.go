package pg

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const jobColumns = "id, runtime, kind, input, state, worker, result, error, created_at"

func scanJob(row pgx.Row) (artifact.JobRecord, error) {
	var j artifact.JobRecord
	var kind, state string
	if err := row.Scan(&j.ID, &j.Runtime, &kind, &j.Input, &state, &j.Worker, &j.Result, &j.Error, &j.Created); err != nil {
		return j, err
	}
	j.Kind, j.State, j.Created = artifact.JobKind(kind), artifact.JobState(state), j.Created.UTC()
	return j, nil
}

// PostJob queues a job.
func (s *Store) PostJob(ctx context.Context, j artifact.JobRecord) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := artifact.ValidateJob(j); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "INSERT INTO jobs (id, runtime, kind, input) VALUES ($1, $2, $3, $4)", j.ID, j.Runtime, string(j.Kind), j.Input)
	return storageErr("post job", err)
}

// ClaimJobs claims the oldest queued jobs of runtime; concurrent claimers
// skip each other's rows.
func (s *Store) ClaimJobs(ctx context.Context, worker, runtime string, n int) ([]artifact.JobRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if worker == "" || n <= 0 {
		return nil, invalid("a claim needs a worker and a positive count")
	}
	rows, err := s.pool.Query(ctx, `UPDATE jobs j SET state = 'claimed', worker = $1
		FROM (SELECT id FROM jobs WHERE state = 'queued' AND runtime = $2 ORDER BY created_at, id LIMIT $3 FOR UPDATE SKIP LOCKED) c
		WHERE j.id = c.id RETURNING j.id, j.runtime, j.kind, j.input, j.state, j.worker, j.result, j.error, j.created_at`, worker, runtime, n)
	if err != nil {
		return nil, storageErr("claim jobs", err)
	}
	defer rows.Close()
	out := []artifact.JobRecord{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, storageErr("claim jobs", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr("claim jobs", err)
	}
	// RETURNING follows no order.
	slices.SortFunc(out, func(a, b artifact.JobRecord) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// FinishJob records a claimed job's answer.
func (s *Store) FinishJob(ctx context.Context, id, worker string, result, failure []byte) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := artifact.ValidateJobAnswer(result, failure); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE jobs SET state = 'done', result = $3, error = $4
		WHERE id = $1 AND state = 'claimed' AND worker = $2`, id, worker, result, failure)
	if err != nil {
		return storageErr("finish job", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return s.jobMissing(ctx, id, "finish job")
}

// Job returns a job.
func (s *Store) Job(ctx context.Context, id string) (artifact.JobRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.JobRecord{}, err
	}
	j, err := scanJob(s.pool.QueryRow(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.JobRecord{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.JobRecord{}, storageErr("job", err)
	}
	return j, nil
}

// WithdrawJob deletes an unclaimed job.
func (s *Store) WithdrawJob(ctx context.Context, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1 AND state = 'queued'", id)
	if err != nil {
		return storageErr("withdraw job", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	return s.jobMissing(ctx, id, "withdraw job")
}

// jobMissing tells a deleted job from one in another state.
func (s *Store) jobMissing(ctx context.Context, id, op string) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)", id).Scan(&exists); err != nil {
		return storageErr(op, err)
	}
	if exists {
		return artifact.ErrConflict
	}
	return artifact.ErrNotFound
}

// DeleteJob deletes a job.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM jobs WHERE id = $1", id)
	return storageErr("delete job", err)
}

// TrimJobs deletes jobs posted before before.
func (s *Store) TrimJobs(ctx context.Context, before time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM jobs WHERE created_at < $1", before.UTC())
	return storageErr("trim jobs", err)
}
