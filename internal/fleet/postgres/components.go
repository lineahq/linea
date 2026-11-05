package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Component set bounds. A set no agent references is garbage after an hour:
// agents store their build hash with their first report.
const (
	maxComponentSets   = 1000
	componentSetUnused = "1 hour"
)

// ComponentSet returns the component types of an agent build.
func (s *Store) ComponentSet(ctx context.Context, hash string) (configmgmt.Components, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	var b []byte
	err := s.pool.QueryRow(ctx, "SELECT components FROM fleet.component_sets WHERE hash=$1", hash).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fleet.ErrNotFound
	}
	if err != nil {
		return nil, normalize(err)
	}
	c := configmgmt.ParseComponents(b)
	if c == nil {
		return nil, fleet.ErrNotFound
	}
	return c, nil
}

// SaveComponentSet stores a build's component types once; the first report
// wins. It refuses new builds beyond maxComponentSets.
func (s *Store) SaveComponentSet(ctx context.Context, hash string, c configmgmt.Components) error {
	if !validComponentsHash(hash) || c == nil {
		return fleet.ErrInvalid
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `INSERT INTO fleet.component_sets(hash,components,first_seen_at)
		SELECT $1,$2::jsonb,now() WHERE (SELECT count(*) FROM fleet.component_sets) < $3
		ON CONFLICT(hash) DO NOTHING`, hash, string(c.JSON()), maxComponentSets)
	if err != nil {
		return normalize(err)
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.component_sets WHERE hash=$1)", hash).Scan(&exists); err != nil {
			return normalize(err)
		}
		if !exists {
			return fleet.ErrCapacity
		}
	}
	return nil
}

func validComponentsHash(h string) bool {
	if len(h) < 2 || len(h) > 2*configmgmt.MaxComponentsHash || len(h)%2 != 0 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// pruneComponentSets removes sets that no agent references.
func (s *Store) pruneComponentSets(ctx context.Context) error {
	ctx, cancel := bounded(ctx)
	defer cancel()
	_, err := s.pool.Exec(ctx, `DELETE FROM fleet.component_sets c WHERE c.first_seen_at < now() - interval '`+componentSetUnused+`'
		AND NOT EXISTS (SELECT 1 FROM fleet.agents a WHERE a.available_components_hash=c.hash)`)
	return normalize(err)
}
