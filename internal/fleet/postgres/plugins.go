package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

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

func (s *Store) SetPluginLifecycle(ctx context.Context, actor string, item fleet.PluginLifecycle, expected int64) (fleet.PluginLifecycle, error) {
	switch {
	case !pluginsdk.ValidID(item.ID) || expected < 0:
		return item, fleet.ErrInvalid
	case item.Desired != "enabled" && item.Desired != "draining" && item.Desired != "disabled":
		return item, fleet.Invalid("desired must be enabled, draining or disabled")
	}
	err := s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireAdmin(ctx, tx, actor); err != nil {
			return err
		}
		var current int64
		err := tx.QueryRow(ctx, "SELECT revision FROM fleet.plugin_lifecycle WHERE id=$1", item.ID).Scan(&current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if current != expected {
			return fleet.ErrPrecondition
		}
		if current == 0 {
			var count int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.plugin_lifecycle").Scan(&count); err != nil {
				return err
			}
			if count >= 256 {
				return fleet.ErrCapacity
			}
		}
		item.Revision = current + 1
		if _, err := tx.Exec(ctx, "INSERT INTO fleet.plugin_lifecycle(id,desired,revision) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET desired=EXCLUDED.desired,revision=EXCLUDED.revision", item.ID, item.Desired, item.Revision); err != nil {
			return err
		}
		return s.event(ctx, tx, "plugin."+item.Desired, item.ID, actor, nil)
	})
	return item, err
}
