package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

// pluginChannel carries the plugin ID of every committed state change, as a
// hint; the table is the truth. It is sent in the changing transaction.
const pluginChannel = "fleet_plugin"

// pluginColumns is the PluginState projection. SeenAge is computed with
// database time, in microseconds, and is -1 when the plugin was never seen
// under the epoch. A seen_at written by a transaction that started after
// this one reads as age 0.
const pluginColumns = `id,desired,revision,epoch,coalesce(instance_id,''),
	CASE WHEN seen_at IS NULL THEN -1 ELSE greatest(0,(extract(epoch FROM now()-seen_at)*1000000)::bigint) END`

func scanPluginState(row scanner) (fleet.PluginState, error) {
	var state fleet.PluginState
	var micros int64
	if err := row.Scan(&state.ID, &state.Desired, &state.Revision, &state.Epoch, &state.InstanceID, &micros); err != nil {
		return fleet.PluginState{}, err
	}
	state.SeenAge = time.Duration(micros) * time.Microsecond
	return state, nil
}

// pluginState reads one row inside tx. A missing row is pgx.ErrNoRows.
func pluginState(ctx context.Context, tx pgx.Tx, id string) (fleet.PluginState, error) {
	return scanPluginState(tx.QueryRow(ctx, "SELECT "+pluginColumns+" FROM fleet.plugin_lifecycle WHERE id=$1", id))
}

func notifyPlugin(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, "SELECT pg_notify('"+pluginChannel+"',$1)", id)
	return err
}

func (s *Store) PluginLifecycles(ctx context.Context) ([]fleet.PluginLifecycle, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT id,desired,revision FROM fleet.plugin_lifecycle ORDER BY id")
	if err != nil {
		return nil, normalize(err)
	}
	defer rows.Close()
	items := []fleet.PluginLifecycle{}
	for rows.Next() {
		var item fleet.PluginLifecycle
		if err := rows.Scan(&item.ID, &item.Desired, &item.Revision); err != nil {
			return nil, normalize(err)
		}
		items = append(items, item)
	}
	return items, normalize(rows.Err())
}

func (s *Store) PluginStates(ctx context.Context) ([]fleet.PluginState, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT "+pluginColumns+" FROM fleet.plugin_lifecycle ORDER BY id")
	if err != nil {
		return nil, normalize(err)
	}
	defer rows.Close()
	items := []fleet.PluginState{}
	for rows.Next() {
		state, err := scanPluginState(rows)
		if err != nil {
			return nil, normalize(err)
		}
		items = append(items, state)
	}
	return items, normalize(rows.Err())
}

// SeenPlugin is T1 and T2 in one conditional UPDATE: the row must still carry
// the caller's epoch, and either the instance differs (the epoch rotates), or
// the plugin was never seen under this epoch or not within the refresh
// interval (the epoch stays). A statement that changes nothing returns the
// current row; a missing row is inserted.
func (s *Store) SeenPlugin(ctx context.Context, id, epoch, instance string) (fleet.PluginState, error) {
	if !pluginsdk.ValidID(id) || !pluginsdk.ValidUID(instance) {
		return fleet.PluginState{}, fleet.ErrInvalid
	}
	var state fleet.PluginState
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = scanPluginState(tx.QueryRow(ctx, `UPDATE fleet.plugin_lifecycle
SET instance_id=$3,
    epoch=CASE WHEN instance_id IS NOT DISTINCT FROM $3 THEN epoch ELSE $4 END,
    seen_at=now()
WHERE id=$1 AND epoch=$2
  AND (instance_id IS DISTINCT FROM $3 OR seen_at IS NULL OR seen_at < now()-make_interval(secs => $5::float8))
RETURNING `+pluginColumns, id, epoch, instance, pluginsdk.RandomID(), fleet.PluginSeenRefresh.Seconds()))
		if err == nil {
			return notifyPlugin(ctx, tx, id)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Nothing changed: the caller's epoch is stale, the write is not due
		// yet, or there is no row.
		if state, err = pluginState(ctx, tx, id); err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.plugin_lifecycle").Scan(&count); err != nil {
			return err
		}
		if count >= fleet.MaxPluginRows {
			return fleet.ErrCapacity
		}
		state, err = scanPluginState(tx.QueryRow(ctx, `INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch,instance_id,seen_at)
VALUES($1,'enabled',0,$2,$3,now())
ON CONFLICT (id) DO NOTHING
RETURNING `+pluginColumns, id, pluginsdk.RandomID(), instance))
		if err == nil {
			return notifyPlugin(ctx, tx, id)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// Another replica inserted first; adopt its row.
			state, err = pluginState(ctx, tx, id)
		}
		return err
	})
	return state, err
}

// FencePlugin is T3 (unseenOnly false) and T4 (unseenOnly true): one
// conditional UPDATE that rotates the epoch and clears seen_at. T4 only
// matches a plugin that was seen, but not within the window, so a plugin
// already fenced and still unreachable keeps its epoch. A statement that
// changes nothing returns the current row; a missing row is ErrNotFound.
func (s *Store) FencePlugin(ctx context.Context, id, epoch string, unseenOnly bool) (fleet.PluginState, error) {
	if !pluginsdk.ValidID(id) {
		return fleet.PluginState{}, fleet.ErrInvalid
	}
	var state fleet.PluginState
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		state, err = scanPluginState(tx.QueryRow(ctx, `UPDATE fleet.plugin_lifecycle
SET epoch=$3, seen_at=NULL
WHERE id=$1 AND epoch=$2
  AND (NOT $4::boolean OR seen_at <= now()-make_interval(secs => $5::float8))
RETURNING `+pluginColumns, id, epoch, pluginsdk.RandomID(), unseenOnly, fleet.PluginSeenWindow.Seconds()))
		if err == nil {
			return notifyPlugin(ctx, tx, id)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		state, err = pluginState(ctx, tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fleet.ErrNotFound
		}
		return err
	})
	return state, err
}

// SetPluginLifecycle is T5. It locks the row, so a concurrent probe or fence
// waits and then finds a different epoch.
func (s *Store) SetPluginLifecycle(ctx context.Context, actor string, item fleet.PluginLifecycle, expected int64) (fleet.PluginLifecycle, error) {
	if err := fleet.ValidPluginLifecycle(item, expected); err != nil {
		return item, err
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var current int64
		exists := true
		err := tx.QueryRow(ctx, "SELECT revision FROM fleet.plugin_lifecycle WHERE id=$1 FOR UPDATE", item.ID).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			exists = false
		} else if err != nil {
			return err
		}
		if current != expected {
			return fleet.ErrPrecondition
		}
		item.Revision = current + 1
		if exists {
			if _, err := tx.Exec(ctx, "UPDATE fleet.plugin_lifecycle SET desired=$2,revision=$3,epoch=$4,seen_at=NULL WHERE id=$1", item.ID, item.Desired, item.Revision, pluginsdk.RandomID()); err != nil {
				return err
			}
		} else {
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.plugin_lifecycle").Scan(&count); err != nil {
				return err
			}
			if count >= fleet.MaxPluginRows {
				return fleet.ErrCapacity
			}
			tag, err := tx.Exec(ctx, "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch) VALUES($1,$2,$3,$4) ON CONFLICT (id) DO NOTHING", item.ID, item.Desired, item.Revision, pluginsdk.RandomID())
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				// A probe created the row after the read above.
				return fleet.ErrPrecondition
			}
		}
		if err := notifyPlugin(ctx, tx, item.ID); err != nil {
			return err
		}
		return s.event(ctx, tx, "plugin."+item.Desired, item.ID, actor, nil)
	})
	return item, err
}
