package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// GrantRuntime is an explicit offline installation operation by the schema owner.
// Runtime does not receive DDL, schema-version writes, role or database privileges.
func GrantRuntime(ctx context.Context, url, role string) error {
	if role == "" {
		return fleet.ErrInvalid
	}
	quoted := pgx.Identifier{role}.Sanitize()
	return adminTx(ctx, url, func(ctx context.Context, tx pgx.Tx) error {
		statements := []string{
			"GRANT USAGE ON SCHEMA fleet TO " + quoted,
			"GRANT SELECT ON fleet.schema_version TO " + quoted,
			"GRANT SELECT,INSERT,UPDATE ON fleet.metadata,fleet.credentials,fleet.groups TO " + quoted,
			// Agents are deleted by retention and capacity eviction.
			"GRANT SELECT,INSERT,UPDATE,DELETE ON fleet.agents TO " + quoted,
			"GRANT SELECT,INSERT,UPDATE ON fleet.plugin_lifecycle TO " + quoted,
			"GRANT SELECT,INSERT,DELETE ON fleet.group_members TO " + quoted,
			// Events are updated only when a restore rotates the stream epoch.
			"GRANT SELECT,INSERT,UPDATE,DELETE ON fleet.events TO " + quoted,
			"GRANT SELECT,INSERT,UPDATE,DELETE ON fleet.replicas,fleet.leases TO " + quoted,
			"GRANT USAGE,SELECT ON SEQUENCE fleet.events_id_seq TO " + quoted,
			"GRANT SELECT,INSERT,UPDATE,DELETE ON fleet.configs,fleet.config_versions,fleet.config_assignments TO " + quoted,
			"GRANT SELECT,INSERT,UPDATE,DELETE ON fleet.rollouts,fleet.rollout_targets TO " + quoted,
			// Component sets are inserted once per build and pruned by retention.
			"GRANT SELECT,INSERT,DELETE ON fleet.component_sets TO " + quoted,
		}
		for _, query := range statements {
			if _, err := tx.Exec(ctx, query); err != nil {
				return err
			}
		}
		return nil
	})
}
