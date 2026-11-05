package pg

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// SetWorkers replaces instance's rows with workers, valid until expires.
func (s *Store) SetWorkers(ctx context.Context, instance string, workers []artifact.PresentWorker, expires time.Time) error {
	if err := artifact.ValidatePresence(instance, workers); err != nil {
		return err
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	ids := make([]string, len(workers))
	identities := make([]string, len(workers))
	capacities := make([]int32, len(workers))
	// Identities hold no spaces, so each worker's warm list travels as one
	// string.
	warm := make([]string, len(workers))
	own := make([]string, len(workers))
	for i, w := range workers {
		ids[i], identities[i], capacities[i], warm[i], own[i] = w.ID, w.Identity, int32(min(w.Capacity, 1<<30)), strings.Join(w.Warm, " "), w.Own
	}
	return s.inTx(ctx, "set workers", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM workers WHERE instance = $1", instance); err != nil {
			return err
		}
		if len(workers) == 0 {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO workers (instance, id, identity, capacity, expires_at, warm, own)
			SELECT $1, w.id, w.identity, w.capacity, $5, string_to_array(w.warm, ' '), w.own
			FROM unnest($2::text[], $3::text[], $4::integer[], $6::text[], $7::text[]) AS w (id, identity, capacity, warm, own)`,
			instance, ids, identities, capacities, expires.UTC(), warm, own)
		return err
	})
}

// WorkerCounts counts the workers of unexpired rows by runtime identity, a
// pool also under its own build's.
func (s *Store) WorkerCounts(ctx context.Context, now time.Time) (map[string]artifact.WorkerCount, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT identity, count(*), sum(capacity),
			count(*) FILTER (WHERE own), coalesce(sum(capacity) FILTER (WHERE own), 0) FROM (
			SELECT identity, capacity, false AS own FROM workers WHERE expires_at > $1
			UNION ALL SELECT own, capacity, true FROM workers WHERE expires_at > $1 AND own <> ''
		) w GROUP BY identity`, now.UTC())
	if err != nil {
		return nil, storageErr("worker counts", err)
	}
	defer rows.Close()
	out := map[string]artifact.WorkerCount{}
	for rows.Next() {
		var identity string
		var workers, slots, ownWorkers, ownSlots int64
		if err := rows.Scan(&identity, &workers, &slots, &ownWorkers, &ownSlots); err != nil {
			return nil, storageErr("worker counts", err)
		}
		out[identity] = artifact.WorkerCount{Workers: int(workers), Slots: int(slots), OwnWorkers: int(ownWorkers), OwnSlots: int(ownSlots)}
	}
	return out, storageErr("worker counts", rows.Err())
}

// WarmRuntimes returns the runtimes pool workers hold running in the
// unexpired rows of instances other than except.
func (s *Store) WarmRuntimes(ctx context.Context, now time.Time, except string) (map[string]bool, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT DISTINCT unnest(warm) FROM workers WHERE expires_at > $1 AND instance <> $2 AND cardinality(warm) > 0", now.UTC(), except)
	if err != nil {
		return nil, storageErr("warm runtimes", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, storageErr("warm runtimes", err)
		}
		out[id] = true
	}
	return out, storageErr("warm runtimes", rows.Err())
}

// TrimWorkers deletes rows that expired before before.
func (s *Store) TrimWorkers(ctx context.Context, before time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM workers WHERE expires_at < $1", before.UTC())
	return storageErr("trim workers", err)
}
