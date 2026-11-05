package postgres

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

const leaderLease = "leader"

// reapBatch and trimBatch bound each leader transaction.
const reapBatch, trimBatch = 1000, 5000

// heartbeatLoop proves liveness, holds or seeks the leader lease, and runs
// singleton jobs while leader. A replica that cannot renew its heartbeat for
// most of the liveness window fences itself before others reap its agents.
func (s *Store) heartbeatLoop() {
	defer s.loops.Done()
	ticker := time.NewTicker(s.options.Heartbeat)
	defer ticker.Stop()
	failure := "none"
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		if s.stopped.Load() {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, s.options.Heartbeat)
		tag, err := s.pool.Exec(ctx, "UPDATE fleet.replicas SET heartbeat_at=now() WHERE id=$1", s.replica)
		cancel()
		switch {
		case err == nil && tag.RowsAffected() == 1:
			s.heartbeat.Store(time.Now().UnixNano())
		case err == nil:
			// The leader reaped this replica; its ownership is already gone.
			s.markLost("registration reaped")
		default:
			failure = errorClass(err)
		}
		if age := time.Since(time.Unix(0, s.heartbeat.Load())); age >= s.options.Alive*8/10 {
			s.markLost("heartbeat not renewed for " + age.Round(time.Second).String() + " (last error: " + failure + ")")
		}
		select {
		case <-s.lost:
			s.leader.Store(false)
			continue
		default:
		}
		ctx, cancel = context.WithTimeout(s.ctx, 2*s.options.Heartbeat)
		s.lead(ctx)
		cancel()
	}
}

// errorClass names a failure without connection details for the fencing
// reason: a timeout, a SQLSTATE, or a connection error.
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &pgErr):
		return "SQLSTATE " + pgErr.Code
	default:
		return "connection"
	}
}

func (s *Store) lead(ctx context.Context) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO fleet.leases(name,holder,expires_at) VALUES($1,$2,now() + $3 * interval '1 millisecond')
		ON CONFLICT(name) DO UPDATE SET holder=EXCLUDED.holder,expires_at=EXCLUDED.expires_at
		WHERE fleet.leases.holder=EXCLUDED.holder OR fleet.leases.expires_at < now()`, leaderLease, s.replica, s.aliveMillis())
	s.leader.Store(err == nil && tag.RowsAffected() == 1)
	if !s.leader.Load() {
		return
	}
	// Jobs are idempotent, so a brief overlap during lease handover is safe.
	// Reaping gets half a heartbeat, so the next renewal is not delayed.
	_, _ = s.reapAll(ctx, s.options.Heartbeat/2)
	_ = s.trimEvents(ctx, s.options.MaxEvents)
	_ = s.retain(ctx)
	_ = s.pruneComponentSets(ctx)
}

// reapAll repeats full batches until the budget is spent, so a crashed
// replica's thousands of agents are released within one leader tick instead
// of one batch per tick. Every batch is its own short transaction.
func (s *Store) reapAll(ctx context.Context, budget time.Duration) (int64, error) {
	deadline := time.Now().Add(budget)
	var total int64
	for {
		n, err := s.reap(ctx)
		total += n
		if err != nil || n < int64(s.reapBatch) || !time.Now().Before(deadline) {
			return total, err
		}
	}
}

// reap clears ownership held by dead or vanished replicas in bounded,
// UID-ordered batches and records one disconnect event per agent.
func (s *Store) reap(ctx context.Context) (int64, error) {
	var cleared int64
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `WITH victims AS (
				SELECT a.id, o.heartbeat_at FROM fleet.agents a LEFT JOIN fleet.replicas o ON o.id=a.owner_replica
				WHERE a.owner_replica IS NOT NULL AND (o.id IS NULL OR o.heartbeat_at < now() - $1 * interval '1 millisecond')
				ORDER BY a.id LIMIT $2 FOR UPDATE OF a SKIP LOCKED
			), cleared AS (
				UPDATE fleet.agents a SET owner_replica=NULL, disconnected_at=COALESCE(v.heartbeat_at, now())
				FROM victims v WHERE a.id=v.id RETURNING a.id
			)
			INSERT INTO fleet.events(kind,subject_id,actor_id) SELECT 'agent.disconnected', id, 'server' FROM cleared ORDER BY id`, s.aliveMillis(), s.reapBatch)
		if err != nil {
			return err
		}
		cleared = tag.RowsAffected()
		if cleared > 0 {
			if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_events','')"); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM fleet.replicas r WHERE r.heartbeat_at < now() - $1 * interval '1 millisecond'
			AND NOT EXISTS (SELECT 1 FROM fleet.agents a WHERE a.owner_replica=r.id)`, s.aliveMillis())
		return err
	})
	s.reaped.Add(uint64(cleared))
	return cleared, err
}

// trimEvents keeps at most MaxEvents by ID range. Only events from finished
// transactions are deleted, and the floor moves to the highest deleted
// position so a cursor below it is reported as expired.
func (s *Store) trimEvents(ctx context.Context, maxEvents int) error {
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var floorXID *string
		var floorID *int64
		err := tx.QueryRow(ctx, `WITH bound AS (SELECT max(id) - $1 AS b FROM fleet.events),
			doomed AS (
				SELECT e.id FROM fleet.events e, bound WHERE e.id <= bound.b
				AND e.xid < pg_snapshot_xmin(pg_current_snapshot()) ORDER BY e.id LIMIT $2
			), deleted AS (
				DELETE FROM fleet.events e USING doomed d WHERE e.id=d.id RETURNING e.xid, e.id
			)
			SELECT xid::text AS floor_xid, id FROM deleted ORDER BY xid DESC, id DESC LIMIT 1`, maxEvents, trimBatch).Scan(&floorXID, &floorID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE fleet.metadata SET event_floor_xid=$1::text::xid8, event_floor_id=$2
			WHERE singleton AND (event_floor_xid, event_floor_id) < ($1::text::xid8, $2)`, *floorXID, *floorID)
		return err
	})
}

// flushLoop persists in-memory last-seen times for this replica's agents.
func (s *Store) flushLoop() {
	defer s.loops.Done()
	ticker := time.NewTicker(s.options.Flush)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		_ = s.flushSeen(ctx)
		cancel()
	}
}

func (s *Store) SeenAgent(id string, generation uint64, now time.Time) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if current, ok := s.seen[id]; !ok || current.generation <= generation {
		s.seen[id] = seenAt{generation: generation, at: now}
	}
}

func (s *Store) takeSeen(id string, generation uint64) *time.Time {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	entry, ok := s.seen[id]
	if !ok || entry.generation != generation {
		return nil
	}
	delete(s.seen, id)
	return &entry.at
}

// flushSeen writes one statement for all agents that reported since the last
// flush. Rows are conditional on ownership and generation, sorted by UID.
func (s *Store) flushSeen(ctx context.Context) error {
	s.seenMu.Lock()
	pending := s.seen
	s.seen = make(map[string]seenAt, len(pending))
	s.seenMu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	generations := make([]int64, len(ids))
	times := make([]time.Time, len(ids))
	for i, id := range ids {
		generations[i], times[i] = int64(pending[id].generation), pending[id].at
	}
	_, err := s.pool.Exec(ctx, `UPDATE fleet.agents a SET last_seen_at=v.seen
		FROM unnest($1::text[], $2::bigint[], $3::timestamptz[]) AS v(id, gen, seen)
		WHERE a.id=v.id AND a.generation=v.gen AND a.owner_replica=$4 AND (a.last_seen_at IS NULL OR a.last_seen_at < v.seen)`, ids, generations, times, s.replica)
	if err != nil {
		s.flushErrors.Add(1)
		// Keep the newest unsaved observations for the next attempt.
		s.seenMu.Lock()
		for id, entry := range pending {
			if current, ok := s.seen[id]; !ok || current.generation < entry.generation {
				s.seen[id] = entry
			}
		}
		s.seenMu.Unlock()
		return fleet.ErrUnavailable
	}
	return nil
}

// listen holds one dedicated session for cross-replica hints and reconnects
// with backoff. Hints are never required for correctness.
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
	for _, channel := range []string{"fleet_events", "fleet_agent_takeover", "fleet_credential_revoked", "fleet_config"} {
		if _, err := conn.Exec(s.ctx, "LISTEN "+channel); err != nil {
			return false
		}
	}
	// Anything may have been missed while disconnected.
	s.wakeEvents()
	s.signalMu.Lock()
	config := s.onConfig
	s.signalMu.Unlock()
	if config != nil {
		config("")
	}
	for {
		n, err := conn.WaitForNotification(s.ctx)
		if err != nil {
			return true
		}
		s.signalMu.Lock()
		takeover, revoked, config := s.onTakeover, s.onRevoked, s.onConfig
		s.signalMu.Unlock()
		switch n.Channel {
		case "fleet_events":
			s.wakeEvents()
		case "fleet_agent_takeover":
			id, value, ok := strings.Cut(n.Payload, ":")
			generation, err := strconv.ParseUint(value, 10, 64)
			if ok && err == nil && fleet.ValidID(id) && takeover != nil {
				takeover(id, generation)
			}
		case "fleet_credential_revoked":
			if fleet.ValidID(n.Payload) && revoked != nil {
				revoked(n.Payload)
			}
		case "fleet_config":
			if (n.Payload == "" || fleet.ValidID(n.Payload)) && config != nil {
				config(n.Payload)
			}
		}
	}
}

// StopAgents runs after the gateway closed every socket: it drains the writer,
// flushes last-seen times, releases remaining ownership with events and
// deregisters the replica.
func (s *Store) StopAgents(ctx context.Context) error {
	var result error
	s.stopOnce.Do(func() {
		s.writer.drain(ctx)
		_ = s.flushSeen(ctx)
		s.stopped.Store(true)
		result = s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `WITH cleared AS (
					UPDATE fleet.agents SET owner_replica=NULL, disconnected_at=now() WHERE owner_replica=$1 RETURNING id
				)
				INSERT INTO fleet.events(kind,subject_id,actor_id) SELECT 'agent.disconnected', id, 'server' FROM cleared ORDER BY id`, s.replica)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "DELETE FROM fleet.leases WHERE holder=$1", s.replica); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "DELETE FROM fleet.replicas WHERE id=$1", s.replica); err != nil {
				return err
			}
			return s.event(ctx, tx, "replica.stopped", s.replica, "server", nil)
		})
	})
	return result
}
