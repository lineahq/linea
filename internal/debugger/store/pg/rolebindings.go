package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Role bindings are outside StorageBytes, like tokens: a full installation
// must not lock people out. artifact.MaxRoleBindings bounds them instead.
// Writes run under the family's advisory lock, so the first write, before
// the set's row exists, is ordered too.

// RoleBindings returns the managed role bindings and their revision.
func (s *Store) RoleBindings(ctx context.Context) (artifact.RoleBindingSet, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	var set artifact.RoleBindingSet
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		set, err = readRoleBindings(ctx, tx)
		return err
	})
	return set, storageErr("role bindings", err)
}

// AddRoleBinding adds a managed role binding at revision expected.
func (s *Store) AddRoleBinding(ctx context.Context, expected uint64, b artifact.RoleBindingRecord) (artifact.RoleBindingSet, error) {
	if err := artifact.ValidateRoleBinding(b); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	kind, subject := "group", b.Group
	if b.Principal != "" {
		kind, subject = "principal", b.Principal
	}
	return s.writeRoleBindings(ctx, "add role binding", expected, func(tx pgx.Tx, set artifact.RoleBindingSet) error {
		for _, existing := range set.Bindings {
			if existing.ID == b.ID || existing.SameGrant(b) {
				return fmt.Errorf("%w: the role binding exists", artifact.ErrConflict)
			}
		}
		if len(set.Bindings) >= artifact.MaxRoleBindings {
			return artifact.ErrLimit
		}
		_, err := tx.Exec(ctx, `INSERT INTO role_bindings (id, subject_kind, subject, role, scope, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, b.ID, kind, subject, b.Role, b.Scope, b.CreatedBy, b.Created.UTC())
		return err
	})
}

// RemoveRoleBinding removes a managed role binding at revision expected.
func (s *Store) RemoveRoleBinding(ctx context.Context, expected uint64, id string) (artifact.RoleBindingSet, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.RoleBindingSet{}, err
	}
	return s.writeRoleBindings(ctx, "remove role binding", expected, func(tx pgx.Tx, _ artifact.RoleBindingSet) error {
		tag, err := tx.Exec(ctx, "DELETE FROM role_bindings WHERE id = $1", id)
		if err == nil && tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: role binding %s", artifact.ErrNotFound, id)
		}
		return err
	})
}

// writeRoleBindings runs write on the set at revision expected and raises
// the revision, which records the change.
func (s *Store) writeRoleBindings(ctx context.Context, op string, expected uint64, write func(pgx.Tx, artifact.RoleBindingSet) error) (artifact.RoleBindingSet, error) {
	var out artifact.RoleBindingSet
	err := s.inTx(ctx, op, func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "role_bindings"); err != nil {
			return err
		}
		set, err := readRoleBindings(ctx, tx)
		if err != nil {
			return err
		}
		if set.Revision != expected {
			return fmt.Errorf("%w: role bindings are at revision %d, not %d", artifact.ErrConflict, set.Revision, expected)
		}
		if err := write(tx, set); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO role_binding_set (id, revision) VALUES ('bindings', 1)
			ON CONFLICT (id) DO UPDATE SET revision = role_binding_set.revision + 1`); err != nil {
			return err
		}
		out, err = readRoleBindings(ctx, tx)
		return err
	})
	if err != nil {
		return artifact.RoleBindingSet{}, err
	}
	return out, nil
}

func readRoleBindings(ctx context.Context, tx pgx.Tx) (artifact.RoleBindingSet, error) {
	var set artifact.RoleBindingSet
	var revision int64
	if err := tx.QueryRow(ctx, "SELECT revision FROM role_binding_set").Scan(&revision); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return set, err
	}
	set.Revision = uint64(revision)
	rows, err := tx.Query(ctx, `SELECT id, subject_kind, subject, role, scope, created_by, created_at
		FROM role_bindings ORDER BY created_at, id`)
	if err != nil {
		return set, err
	}
	defer rows.Close()
	for rows.Next() {
		var b artifact.RoleBindingRecord
		var kind, subject string
		if err := rows.Scan(&b.ID, &kind, &subject, &b.Role, &b.Scope, &b.CreatedBy, &b.Created); err != nil {
			return set, err
		}
		if kind == "principal" {
			b.Principal = subject
		} else {
			b.Group = subject
		}
		b.Created = b.Created.UTC()
		set.Bindings = append(set.Bindings, b)
	}
	return set, rows.Err()
}
