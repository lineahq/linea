package pg

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// changeChannel is the NOTIFY channel of the change feed; the payload is the
// schema of the writing installation, so installations sharing a database
// ignore each other's hints.
const changeChannel = "linea_debugger"

// ChangeHead returns the oldest transaction still in progress as a cursor: a
// change after it may yet commit, every one before it has.
func (s *Store) ChangeHead(ctx context.Context) (artifact.ChangeCursor, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.ChangeCursor{}, err
	}
	var xmin string
	if err := s.pool.QueryRow(ctx, "SELECT pg_snapshot_xmin(pg_current_snapshot())::text").Scan(&xmin); err != nil {
		return artifact.ChangeCursor{}, storageErr("change head", err)
	}
	x, err := strconv.ParseUint(xmin, 10, 64)
	if err != nil {
		return artifact.ChangeCursor{}, storageErr("change head", err)
	}
	return artifact.ChangeCursor{XID: x}, nil
}

// Changes returns changes of finished transactions after cursor.
func (s *Store) Changes(ctx context.Context, after artifact.ChangeCursor, limit int) ([]artifact.Change, artifact.ChangeCursor, error) {
	if err := s.ready(ctx); err != nil {
		return nil, after, err
	}
	if limit <= 0 || limit > artifact.MaxChanges {
		return nil, after, invalid("invalid change limit")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, after, storageErr("changes", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var floorXID string
	var floorID int64
	if err := tx.QueryRow(ctx, "SELECT xid::text, id FROM change_floor").Scan(&floorXID, &floorID); err != nil {
		return nil, after, storageErr("changes", err)
	}
	fx, err := strconv.ParseUint(floorXID, 10, 64)
	if err != nil {
		return nil, after, storageErr("changes", err)
	}
	if after.XID < fx || after.XID == fx && after.ID < floorID {
		return nil, after, artifact.ErrExpired
	}
	rows, err := tx.Query(ctx, `SELECT xid::text, id, kind, ref FROM changes
		WHERE (xid, id) > ($1::text::xid8, $2) AND xid < pg_snapshot_xmin(pg_current_snapshot()) ORDER BY xid, id LIMIT $3`,
		strconv.FormatUint(after.XID, 10), after.ID, limit)
	if err != nil {
		return nil, after, storageErr("changes", err)
	}
	defer rows.Close()
	out := []artifact.Change{}
	next := after
	for rows.Next() {
		var c artifact.Change
		var xid, kind string
		if err := rows.Scan(&xid, &c.Cursor.ID, &kind, &c.ID); err != nil {
			return nil, after, storageErr("changes", err)
		}
		if c.Cursor.XID, err = strconv.ParseUint(xid, 10, 64); err != nil {
			return nil, after, storageErr("changes", err)
		}
		c.Kind = artifact.ChangeKind(kind)
		out = append(out, c)
		next = c.Cursor
	}
	if err := rows.Err(); err != nil {
		return nil, after, storageErr("changes", err)
	}
	return out, next, nil
}

// TrimChanges deletes changes of finished transactions beyond the newest
// keep, at most batch at a time, and raises the floor to the newest deleted,
// so a cursor below it expires.
func (s *Store) TrimChanges(ctx context.Context, keep, batch int) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "trim changes", func(tx pgx.Tx) error {
		var floorXID *string
		var floorID *int64
		err := tx.QueryRow(ctx, `WITH bound AS (SELECT max(id) - $1 AS b FROM changes),
			doomed AS (
				SELECT c.id FROM changes c, bound WHERE c.id <= bound.b
				AND c.xid < pg_snapshot_xmin(pg_current_snapshot()) ORDER BY c.id LIMIT $2
			), deleted AS (
				DELETE FROM changes c USING doomed d WHERE c.id = d.id RETURNING c.xid, c.id
			)
			SELECT xid::text, id FROM deleted ORDER BY xid DESC, id DESC LIMIT 1`, keep, batch).Scan(&floorXID, &floorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE change_floor SET xid = $1::text::xid8, id = $2
			WHERE (xid, id) < ($1::text::xid8, $2)`, *floorXID, *floorID)
		return err
	})
}

// ChangeHints returns a channel that a dedicated LISTEN session signals. The
// session starts on the first call and stops at Close.
func (s *Store) ChangeHints() <-chan struct{} {
	s.hintsOnce.Do(func() {
		s.hints = make(chan struct{}, 1)
		if s.closed.Load() {
			return
		}
		s.loops.Add(1)
		go s.listen()
	})
	return s.hints
}

func (s *Store) hint() {
	select {
	case s.hints <- struct{}{}:
	default:
	}
}

// listen holds one session for hints and reconnects with backoff.
func (s *Store) listen() {
	defer s.loops.Done()
	const first = 100 * time.Millisecond
	backoff := first
	for s.ctx.Err() == nil {
		// The store has no logger and hints are optional, so a lost session's
		// error is not reported: reconnecting is the whole recovery.
		if conn, err := s.pool.Acquire(s.ctx); err == nil {
			if s.receive(conn.Conn()) {
				// The session listened, so the next failure is a new one.
				backoff = first
			}
			// Never return a listening session to the pool.
			_ = conn.Conn().Close(context.Background())
			conn.Release()
		}
		if s.ctx.Err() != nil {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 5*time.Second)
	}
}

// receive listens on conn until it fails and reports whether it listened.
func (s *Store) receive(conn *pgx.Conn) bool {
	var schema string
	if err := conn.QueryRow(s.ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return false
	}
	if _, err := conn.Exec(s.ctx, "LISTEN "+changeChannel); err != nil {
		return false
	}
	// Anything may have committed while disconnected.
	s.hint()
	for {
		n, err := conn.WaitForNotification(s.ctx)
		if err != nil {
			return true
		}
		if n.Payload == schema {
			s.hint()
		}
	}
}

// runColumns are the columns scanRun reads.
const runColumns = `id, project_id, generation, ordinal, terminal, metadata, evidence_digest, evidence_bytes, bundle_digest, bundle_bytes,
	indexed, coalesce(scenario, ''), coalesce(runtime_name, ''), coalesce(result, ''), kept`

func scanRun(row pgx.Row) (artifact.RunRecord, error) {
	var r artifact.RunRecord
	var generation, ordinal int64
	var metadata []byte
	var indexed bool
	var x artifact.RunIndex
	if err := row.Scan(&r.ID, &r.Project, &generation, &ordinal, &r.Terminal, &metadata, &r.Digest, &r.Bytes, &r.BundleDigest, &r.BundleBytes,
		&indexed, &x.Scenario, &x.Runtime, &x.Result, &r.Kept); err != nil {
		return r, err
	}
	r.Generation, r.Ordinal, r.Metadata = uint64(generation), uint64(ordinal), metadata
	if indexed {
		r.Index = &x
	}
	return r, nil
}

// Run returns one run's record.
func (s *Store) Run(ctx context.Context, id string) (artifact.RunRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.RunRecord{}, err
	}
	r, err := scanRun(s.pool.QueryRow(ctx, "SELECT "+runColumns+" FROM runs WHERE id = $1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.RunRecord{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.RunRecord{}, storageErr("run", err)
	}
	return r, nil
}

// AppendRunEvents keeps a batch of an unfinished run's live events.
func (s *Store) AppendRunEvents(ctx context.Context, id string, b artifact.EventBatch) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	if b.Attempt <= 0 || b.First <= 0 || b.Last < b.First || len(b.Events) == 0 {
		return invalid("invalid event batch")
	}
	return s.inTx(ctx, "run events", func(tx pgx.Tx) error {
		// The share lock orders the batch before a terminal publication,
		// which deletes the batches.
		var terminal bool
		err := tx.QueryRow(ctx, "SELECT terminal FROM runs WHERE id = $1 FOR SHARE", id).Scan(&terminal)
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil {
			return err
		}
		if terminal {
			return artifact.ErrConflict
		}
		var latest, last int
		var size int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(attempt), 0),
				coalesce(max(last_seq) FILTER (WHERE attempt = $2), 0),
				coalesce(sum(length(payload)) FILTER (WHERE attempt = $2), 0)
			FROM run_events WHERE run_id = $1`, id, b.Attempt).Scan(&latest, &last, &size); err != nil {
			return err
		}
		if latest > b.Attempt || last >= b.First {
			return artifact.ErrConflict
		}
		if size+int64(len(b.Events)) > artifact.MaxRunEventBytes {
			return artifact.ErrLimit
		}
		if _, err := tx.Exec(ctx, "DELETE FROM run_events WHERE run_id = $1 AND attempt < $2", id, b.Attempt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO run_events (run_id, attempt, first_seq, last_seq, payload) VALUES ($1, $2, $3, $4, $5)",
			id, b.Attempt, b.First, b.Last, b.Events)
		return err
	})
}

// RunEvents returns the latest attempt's batches that end after after.
func (s *Store) RunEvents(ctx context.Context, id string, after int) ([]artifact.EventBatch, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT attempt, first_seq, last_seq, payload FROM run_events
		WHERE run_id = $1 AND attempt = (SELECT max(attempt) FROM run_events WHERE run_id = $1) AND last_seq > $2
		ORDER BY first_seq`, id, after)
	if err != nil {
		return nil, storageErr("run events", err)
	}
	defer rows.Close()
	out := []artifact.EventBatch{}
	for rows.Next() {
		var b artifact.EventBatch
		if err := rows.Scan(&b.Attempt, &b.First, &b.Last, &b.Events); err != nil {
			return nil, storageErr("run events", err)
		}
		out = append(out, b)
	}
	return out, storageErr("run events", rows.Err())
}
