package pg

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// buildColumns lists a build's columns for the table aliased a, with its
// inputs as three arrays in order. The subqueries make every read, and every
// UPDATE ... RETURNING, one statement and so one snapshot.
func buildColumns(a string) string {
	return fmt.Sprintf(`%[1]s.id, %[1]s.principal, %[1]s.name, %[1]s.build_key, %[1]s.spec, %[1]s.state, %[1]s.reused_from, %[1]s.builder,
		%[1]s.attempt, %[1]s.lease_expires_at, %[1]s.cancel_requested, %[1]s.log_lines, %[1]s.result, %[1]s.binary_digest, %[1]s.binary_bytes,
		%[1]s.created_at, %[1]s.started_at, %[1]s.finished_at, %[1]s.ordinal,
		ARRAY(SELECT i.module FROM build_inputs i WHERE i.build_id = %[1]s.id ORDER BY i.ord),
		ARRAY(SELECT i.digest FROM build_inputs i WHERE i.build_id = %[1]s.id ORDER BY i.ord),
		ARRAY(SELECT i.bytes FROM build_inputs i WHERE i.build_id = %[1]s.id ORDER BY i.ord)`, a)
}

// buildRow is a build with its admission ordinal.
type buildRow struct {
	artifact.BuildRecord
	ordinal uint64
}

func scanBuild(row pgx.Row) (buildRow, error) {
	var b buildRow
	var state string
	var expires, started, finished *time.Time
	var ordinal int64
	var modules, digests []string
	var sizes []int64
	err := row.Scan(&b.ID, &b.Principal, &b.Name, &b.Key, &b.Spec, &state, &b.ReusedFrom, &b.Builder, &b.Attempt, &expires, &b.CancelRequested,
		&b.LogLines, &b.Result, &b.BinaryDigest, &b.BinaryBytes, &b.Created, &started, &finished, &ordinal, &modules, &digests, &sizes)
	if err != nil {
		return b, err
	}
	b.State, b.ordinal, b.Created = artifact.BuildState(state), uint64(ordinal), b.Created.UTC()
	for _, t := range []struct {
		from *time.Time
		to   *time.Time
	}{{expires, &b.Expires}, {started, &b.Started}, {finished, &b.Finished}} {
		if t.from != nil {
			*t.to = t.from.UTC()
		}
	}
	for i := range modules {
		b.Inputs = append(b.Inputs, artifact.BuildInput{Module: modules[i], Digest: digests[i], Bytes: sizes[i]})
	}
	return b, nil
}

func collectBuilds(rows pgx.Rows, op string) ([]buildRow, error) {
	defer rows.Close()
	var out []buildRow
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, storageErr(op, err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr(op, err)
	}
	// UPDATE ... RETURNING follows no order.
	slices.SortFunc(out, func(x, y buildRow) int { return cmp.Compare(x.ordinal, y.ordinal) })
	return out, nil
}

func records(rows []buildRow) []artifact.BuildRecord {
	out := make([]artifact.BuildRecord, len(rows))
	for i, r := range rows {
		out[i] = r.BuildRecord
	}
	return out
}

// buildMissing tells a build that does not exist from one in another state.
func buildMissing(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id, op string) error {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM builds WHERE id = $1)", id).Scan(&exists); err != nil {
		return storageErr(op, err)
	}
	if exists {
		return artifact.ErrConflict
	}
	return artifact.ErrNotFound
}

// AdmitBuild stores a build's inputs and its record in one transaction under
// the installation lock, which also serializes the limit checks and keeps
// ordinals in commit order. A done build of the same key makes the new one
// done at once and takes no queue slot.
func (s *Store) AdmitBuild(ctx context.Context, a artifact.BuildAdmission, limits artifact.BuildLimits) (artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.BuildRecord{}, err
	}
	if err := artifact.ValidateBuildAdmission(a); err != nil {
		return artifact.BuildRecord{}, err
	}
	if !validText(a.ID, a.Principal, a.Name) {
		return artifact.BuildRecord{}, invalid("build text is not valid UTF-8")
	}
	// The manifest comes first, then the sources by module.
	contents := [][]byte{a.Manifest}
	inputs := []artifact.BuildInput{{Digest: digestOf(a.Manifest), Bytes: int64(len(a.Manifest))}}
	for _, src := range slices.SortedFunc(slices.Values(a.Sources), func(x, y artifact.BuildSource) int { return cmp.Compare(x.Module, y.Module) }) {
		if !validText(src.Module) {
			return artifact.BuildRecord{}, invalid("build module is not valid UTF-8")
		}
		contents = append(contents, src.Archive)
		inputs = append(inputs, artifact.BuildInput{Module: src.Module, Digest: digestOf(src.Archive), Bytes: int64(len(src.Archive))})
	}
	var out artifact.BuildRecord
	err := s.inTx(ctx, "admit build", func(tx pgx.Tx) error {
		if _, err := lockInstallation(ctx, tx); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM builds WHERE id = $1)", a.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return artifact.ErrConflict
		}
		var reuse struct {
			id, digest string
			size       int64
			result     []byte
		}
		err := tx.QueryRow(ctx, `SELECT id, binary_digest, binary_bytes, result FROM builds
			WHERE build_key = $1 AND state = 'done' ORDER BY ordinal DESC LIMIT 1`, a.Key).Scan(&reuse.id, &reuse.digest, &reuse.size, &reuse.result)
		reused := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !reused {
			var principal, total int
			if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE principal = $1), count(*) FROM builds WHERE state IN ('queued', 'running')`,
				a.Principal).Scan(&principal, &total); err != nil {
				return err
			}
			if principal >= limits.PerPrincipal || total >= limits.Total {
				return artifact.ErrLimit
			}
		}
		blobs := make([]blob, len(inputs))
		for i, in := range inputs {
			blobs[i] = blob{in.Digest, in.Bytes}
		}
		additional, err := newBytes(ctx, tx, blobs...)
		if err != nil {
			return err
		}
		// A queued build reserves room for its binary; a reused one has it.
		var room reserve
		if !reused {
			room.extraBuilds = 1
		}
		if err := s.checkSpace(ctx, tx, additional, room); err != nil {
			return err
		}
		for i, in := range inputs {
			if err := s.putBlob(ctx, in.Digest, contents[i]); err != nil {
				return err
			}
		}
		created := a.Created.UTC()
		if reused {
			_, err = tx.Exec(ctx, `INSERT INTO builds (id, principal, name, build_key, spec, state, reused_from, result, binary_digest, binary_bytes, created_at, started_at, finished_at)
				VALUES ($1, $2, $3, $4, $5, 'done', $6, $7, $8, $9, $10, $10, $10)`,
				a.ID, a.Principal, a.Name, a.Key, a.Spec, reuse.id, reuse.result, reuse.digest, reuse.size, created)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO builds (id, principal, name, build_key, spec, state, created_at) VALUES ($1, $2, $3, $4, $5, 'queued', $6)`,
				a.ID, a.Principal, a.Name, a.Key, a.Spec, created)
		}
		if err != nil {
			return err
		}
		for i, in := range inputs {
			if _, err := tx.Exec(ctx, "INSERT INTO build_inputs (build_id, ord, module, digest, bytes) VALUES ($1, $2, $3, $4, $5)", a.ID, i, in.Module, in.Digest, in.Bytes); err != nil {
				return err
			}
		}
		b, err := scanBuild(tx.QueryRow(ctx, "SELECT "+buildColumns("b")+" FROM builds b WHERE b.id = $1", a.ID))
		out = b.BuildRecord
		return err
	})
	if err != nil {
		return artifact.BuildRecord{}, err
	}
	return out, nil
}

// Build returns a build.
func (s *Store) Build(ctx context.Context, id string) (artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.BuildRecord{}, err
	}
	b, err := scanBuild(s.pool.QueryRow(ctx, "SELECT "+buildColumns("b")+" FROM builds b WHERE b.id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.BuildRecord{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.BuildRecord{}, storageErr("build", err)
	}
	return b.BuildRecord, nil
}

// BuildManifest returns the manifest a build was admitted with, read from
// the object store and checked against its digest.
func (s *Store) BuildManifest(ctx context.Context, id string) ([]byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var digest string
	var size int64
	err := s.pool.QueryRow(ctx, "SELECT digest, bytes FROM build_inputs WHERE build_id = $1 AND module = '' LIMIT 1", id).Scan(&digest, &size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, artifact.ErrNotFound
	}
	if err != nil {
		return nil, storageErr("build manifest", err)
	}
	return s.readBlob(ctx, digest, size)
}

// ListBuilds returns a page of builds, newest first, keyed on the admission
// ordinal. One row beyond the limit tells whether another page follows.
func (s *Store) ListBuilds(ctx context.Context, q artifact.BuildQuery) (*artifact.BuildPage, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	q, before, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+buildColumns("b")+` FROM builds b
		WHERE ($1 = '' OR b.principal = $1) AND ($2 = 0 OR b.ordinal < $2)
		ORDER BY b.ordinal DESC LIMIT $3`, q.Principal, int64(before), q.Limit+1)
	if err != nil {
		return nil, storageErr("build list", err)
	}
	// collectBuilds sorts oldest first; a page is newest first.
	found, err := collectBuilds(rows, "build list")
	if err != nil {
		return nil, err
	}
	slices.Reverse(found)
	p := &artifact.BuildPage{Builds: []artifact.BuildRecord{}}
	if len(found) > q.Limit {
		found = found[:q.Limit]
		p.NextCursor = q.NextCursor(found[q.Limit-1].ordinal)
	}
	p.Builds = append(p.Builds, records(found)...)
	return p, nil
}

// BuildLog returns the kept lines from line from on. One statement reads the
// count and the lines, so they agree.
func (s *Store) BuildLog(ctx context.Context, id string, from int) ([]string, int, error) {
	if err := s.ready(ctx); err != nil {
		return nil, 0, err
	}
	from = max(from, 0)
	rows, err := s.pool.Query(ctx, `SELECT b.log_lines, l.line, l.text FROM builds b
		LEFT JOIN build_log l ON l.build_id = b.id AND l.line >= $2 WHERE b.id = $1 ORDER BY l.line`, id, from)
	if err != nil {
		return nil, 0, storageErr("build log", err)
	}
	defer rows.Close()
	lines := []string{}
	total, first, found := 0, from, false
	for rows.Next() {
		var line *int
		var text *string
		if err := rows.Scan(&total, &line, &text); err != nil {
			return nil, 0, storageErr("build log", err)
		}
		found = true
		if line != nil {
			if len(lines) == 0 {
				first = *line
			}
			lines = append(lines, *text)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, storageErr("build log", err)
	}
	if !found {
		return nil, 0, artifact.ErrNotFound
	}
	if len(lines) == 0 {
		// The kept lines start at max(0, total-MaxBuildLogLines).
		first = max(from, total-artifact.MaxBuildLogLines)
	}
	return lines, first, nil
}

// ClaimBuilds leases up to n queued builds with FOR UPDATE SKIP LOCKED, so
// builders claiming at once never take the same build or wait for each other.
func (s *Store) ClaimBuilds(ctx context.Context, builder string, n int, now, expires time.Time) ([]artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validWorker(builder) || !validText(builder) || n < 0 || expires.IsZero() {
		return nil, invalid("invalid build claim")
	}
	if n == 0 {
		return []artifact.BuildRecord{}, nil
	}
	rows, err := s.pool.Query(ctx, `WITH picked AS (
			SELECT id FROM builds WHERE state = 'queued' ORDER BY ordinal FOR UPDATE SKIP LOCKED LIMIT $1)
		UPDATE builds b SET state = 'running', builder = $2, attempt = b.attempt + 1, lease_expires_at = $3, started_at = $4
		FROM picked WHERE b.id = picked.id RETURNING `+buildColumns("b"), n, builder, expires.UTC(), now.UTC())
	if err != nil {
		return nil, storageErr("claim builds", err)
	}
	claimed, err := collectBuilds(rows, "claim builds")
	return records(claimed), err
}

// RenewBuilds extends the builder's leases in one statement, never shortening
// one. It reports nothing on the change feed.
func (s *Store) RenewBuilds(ctx context.Context, builder string, expires time.Time) ([]artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !validWorker(builder) || !validText(builder) || expires.IsZero() {
		return nil, invalid("invalid build renewal")
	}
	rows, err := s.pool.Query(ctx, `UPDATE builds b SET lease_expires_at = greatest(b.lease_expires_at, $2)
		WHERE b.builder = $1 AND b.state = 'running' RETURNING `+buildColumns("b"), builder, expires.UTC())
	if err != nil {
		return nil, storageErr("renew builds", err)
	}
	renewed, err := collectBuilds(rows, "renew builds")
	return records(renewed), err
}

// AppendBuildLog adds lines to a build the builder holds and keeps the last
// artifact.MaxBuildLogLines.
func (s *Store) AppendBuildLog(ctx context.Context, id, builder string, attempt int, lines []string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	clipped := make([]string, len(lines))
	for i, line := range lines {
		clipped[i] = artifact.ClipBuildLine(line)
	}
	return s.inTx(ctx, "append build log", func(tx pgx.Tx) error {
		var state, holder string
		var held, total int
		err := tx.QueryRow(ctx, "SELECT state, builder, attempt, log_lines FROM builds WHERE id = $1 FOR UPDATE", id).Scan(&state, &holder, &held, &total)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if state != string(artifact.BuildRunning) || holder != builder || held != attempt {
			return artifact.ErrConflict
		}
		if len(clipped) == 0 {
			return nil
		}
		// Only the last lines a build keeps are written.
		first := total
		if over := len(clipped) - artifact.MaxBuildLogLines; over > 0 {
			first, clipped = total+over, clipped[over:]
		}
		next := total + len(lines)
		if _, err := tx.Exec(ctx, `INSERT INTO build_log (build_id, line, text)
			SELECT $1, $2 + t.n - 1, t.text FROM unnest($3::text[]) WITH ORDINALITY AS t(text, n)`, id, first, clipped); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM build_log WHERE build_id = $1 AND line < $2", id, next-artifact.MaxBuildLogLines); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE builds SET log_lines = $2 WHERE id = $1", id, next)
		return err
	})
}

// FinishBuild ends a build the builder holds in attempt.
func (s *Store) FinishBuild(ctx context.Context, id, builder string, attempt int, o artifact.BuildOutcome, now time.Time) (artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.BuildRecord{}, err
	}
	if err := artifact.ValidateBuildOutcome(o); err != nil {
		return artifact.BuildRecord{}, err
	}
	b, err := scanBuild(s.pool.QueryRow(ctx, `UPDATE builds b SET state = $4, result = $5, binary_digest = $6, binary_bytes = $7,
			staged_binary_digest = '', staged_binary_bytes = 0, builder = '', lease_expires_at = NULL, finished_at = $8
		WHERE b.id = $1 AND b.state = 'running' AND b.builder = $2 AND b.attempt = $3 RETURNING `+buildColumns("b"),
		id, builder, attempt, string(o.State), o.Result, o.BinaryDigest, o.BinaryBytes, now.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.BuildRecord{}, buildMissing(ctx, s.pool, id, "finish build")
	}
	if err != nil {
		return artifact.BuildRecord{}, storageErr("finish build", err)
	}
	return b.BuildRecord, nil
}

// CancelBuild cancels a queued build and flags a running one for its builder.
func (s *Store) CancelBuild(ctx context.Context, id string, now time.Time) (artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.BuildRecord{}, err
	}
	var out artifact.BuildRecord
	err := s.inTx(ctx, "cancel build", func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, "SELECT state FROM builds WHERE id = $1 FOR UPDATE", id).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if artifact.BuildState(state).Terminal() {
			return artifact.ErrConflict
		}
		b, err := scanBuild(tx.QueryRow(ctx, `UPDATE builds b SET
				state = CASE WHEN b.state = 'queued' THEN 'cancelled' ELSE b.state END,
				finished_at = CASE WHEN b.state = 'queued' THEN $2 ELSE b.finished_at END,
				cancel_requested = b.state = 'running'
			WHERE b.id = $1 RETURNING `+buildColumns("b"), id, now.UTC()))
		out = b.BuildRecord
		return err
	})
	if err != nil {
		return artifact.BuildRecord{}, err
	}
	return out, nil
}

// ExpireBuilds ends the leases that ran out before now in one statement,
// skipping builds a builder is finishing at that moment: a build whose
// cancellation was requested is cancelled, one that used its attempts fails
// with failed, and any other returns to the queue.
func (s *Store) ExpireBuilds(ctx context.Context, now time.Time, maxAttempts int, failed []byte) ([]artifact.BuildRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if maxAttempts < 1 || len(failed) > artifact.MaxBuildResult {
		return nil, invalid("invalid build expiry")
	}
	rows, err := s.pool.Query(ctx, `WITH due AS (
			SELECT id FROM builds WHERE state = 'running' AND lease_expires_at < $1 ORDER BY ordinal FOR UPDATE SKIP LOCKED)
		UPDATE builds b SET
			state = CASE WHEN b.cancel_requested THEN 'cancelled' WHEN b.attempt >= $2 THEN 'failed' ELSE 'queued' END,
			result = CASE WHEN NOT b.cancel_requested AND b.attempt >= $2 THEN $3::bytea ELSE b.result END,
			finished_at = CASE WHEN b.cancel_requested OR b.attempt >= $2 THEN $1 ELSE NULL END,
			staged_binary_digest = '', staged_binary_bytes = 0, builder = '', lease_expires_at = NULL
		FROM due WHERE b.id = due.id AND b.state = 'running' RETURNING `+buildColumns("b"), now.UTC(), maxAttempts, failed)
	if err != nil {
		return nil, storageErr("expire builds", err)
	}
	expired, err := collectBuilds(rows, "expire builds")
	return records(expired), err
}

// TrimBuilds deletes the terminal builds beyond the newest keep. Their inputs
// and binaries become unreferenced for Sweep. It adds no object, so it needs
// no installation lock.
func (s *Store) TrimBuilds(ctx context.Context, keep int) (int, error) {
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	if keep < 0 {
		return 0, invalid("invalid build trim")
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM builds WHERE id IN (
		SELECT id FROM builds WHERE state IN ('done', 'failed', 'cancelled') ORDER BY ordinal DESC OFFSET $1)`, keep)
	if err != nil {
		return 0, storageErr("trim builds", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountBuilds counts the unfinished builds by state in one statement.
func (s *Store) CountBuilds(ctx context.Context) (artifact.BuildCounts, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.BuildCounts{}, err
	}
	var c artifact.BuildCounts
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'queued'), count(*) FILTER (WHERE state = 'running')
		FROM builds WHERE state IN ('queued', 'running')`).Scan(&c.Queued, &c.Running)
	if err != nil {
		return artifact.BuildCounts{}, storageErr("count builds", err)
	}
	return c, nil
}
