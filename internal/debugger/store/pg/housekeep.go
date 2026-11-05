package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

const (
	// keepChanges is how many of the newest changes the feed retains; a
	// instance further behind reloads (artifact.ErrExpired).
	keepChanges = 100_000
	// trimBatch bounds the changes one round deletes.
	trimBatch = 50_000
	// sweepMargin is how old an unreferenced object must be to go (one day).
	sweepMargin = 24 * time.Hour
	// tombstoneMargin is how long a tombstone outlives the oldest change the
	// feed retains: enough for any clock skew between the
	// instance that deleted and the database.
	tombstoneMargin = 24 * time.Hour
	// housekeepLock names the session advisory lock the leading handle holds
	// while it leads.
	housekeepLock = "hashtextextended('linea.debugger.store.housekeep:' || current_schema(), 0)"
)

// Lead reports whether this handle leads the upkeep of its schema. The first
// handle to ask takes a session advisory lock on a dedicated connection and
// keeps it until it closes or loses that connection; another handle is then
// elected at its next call.
func (s *Store) Lead(ctx context.Context) (bool, error) {
	if err := s.ready(ctx); err != nil {
		return false, err
	}
	s.leadMu.Lock()
	defer s.leadMu.Unlock()
	if s.closed.Load() {
		// Close resigns after it marks the store closed.
		return false, artifact.ErrClosed
	}
	if s.lead != nil {
		err := s.lead.Ping(ctx)
		if err == nil {
			return true, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// The session ended, and its lock with it.
		s.resignLocked()
	}
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, storageErr("lead", err)
	}
	var led bool
	if err := pooled.QueryRow(ctx, "SELECT pg_try_advisory_lock("+housekeepLock+")").Scan(&led); err != nil {
		pooled.Release()
		return false, storageErr("lead", err)
	}
	if !led {
		pooled.Release()
		return false, nil
	}
	// The connection leaves the pool, so neither its lifetime nor Close's
	// wait for acquired connections applies to it.
	s.lead = pooled.Hijack()
	return true, nil
}

// resign gives up the lead, if this handle holds it.
func (s *Store) resign() {
	s.leadMu.Lock()
	defer s.leadMu.Unlock()
	s.resignLocked()
}

// resignLocked releases the lock and closes the leading session; the
// explicit unlock lets the next handle lead at once, while a session that
// merely ends releases it only as its backend exits. s.leadMu is held.
func (s *Store) resignLocked() {
	if s.lead == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.lead.Exec(ctx, "SELECT pg_advisory_unlock("+housekeepLock+")")
	_ = s.lead.Close(ctx)
	s.lead = nil
}

// Housekeep does one round of upkeep if this handle leads (Lead), and
// reports whether it did: it trims the change feed and the tombstones the
// feed no longer reports, deletes interactive jobs posted before before and
// worker rows that expired before it, and sweeps unreferenced objects.
func (s *Store) Housekeep(ctx context.Context, before time.Time) (bool, error) {
	led, err := s.Lead(ctx)
	if err != nil || !led {
		return false, err
	}
	_, sweepErr := s.Sweep(ctx, sweepMargin)
	trimErr := s.TrimChanges(ctx, keepChanges, trimBatch)
	if trimErr == nil {
		trimErr = s.TrimTombstones(ctx, tombstoneMargin, trimBatch)
	}
	return true, errors.Join(trimErr, s.TrimJobs(ctx, before), s.TrimWorkers(ctx, before), sweepErr)
}

// TrimTombstones deletes up to batch tombstones of runs and suites deleted
// more than margin before the oldest change the feed retains:
// every instance has read their deletion, or reloads without them. A
// deleted ID then reads as unknown, not deleted.
func (s *Store) TrimTombstones(ctx context.Context, margin time.Duration, batch int) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "trim tombstones", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH bound AS (SELECT LEAST(coalesce(min(at), now()), now()) - make_interval(secs => $1) AS b FROM changes)
			DELETE FROM tombstones t USING (
				SELECT kind, id FROM tombstones, bound WHERE deleted_at < bound.b ORDER BY deleted_at LIMIT $2
			) d WHERE t.kind = d.kind AND t.id = d.id`, margin.Seconds(), batch)
		return err
	})
}
