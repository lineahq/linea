package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const (
	runtimeColumns = "name, identity, image, digest, hello, registered_by, registered_at, confirmed_at, binary_digest, binary_bytes, binary_os, binary_arch, build_id, manifest"
	// runtimeRead is what scanRuntime reads: the saved columns and the
	// suspicion MarkSuspect sets.
	runtimeRead = runtimeColumns + ", suspect_cause, suspect_pool, suspect_at"
)

func scanRuntime(row pgx.Row) (artifact.RuntimeEntry, error) {
	var e artifact.RuntimeEntry
	var confirmed, suspectAt *time.Time
	var bin artifact.RuntimeBinary
	var m artifact.Suspicion
	err := row.Scan(&e.Name, &e.Identity, &e.Image, &e.Digest, &e.Hello, &e.RegisteredBy, &e.Registered, &confirmed,
		&bin.Digest, &bin.Bytes, &bin.OS, &bin.Arch, &e.Build, &e.Manifest, &m.Cause, &m.Pool, &suspectAt)
	e.Registered = e.Registered.UTC()
	if len(e.Manifest) == 0 {
		e.Manifest = nil
	}
	if bin.Digest != "" {
		e.Binary = &bin
	}
	if suspectAt != nil {
		m.At = suspectAt.UTC()
		e.Suspect = &m
	}
	if confirmed != nil {
		e.Confirmed = confirmed.UTC()
	}
	return e, err
}

// SaveRuntime records an entry, replacing the one of the same name. The
// catalog's count is checked under its advisory lock, so instances
// registering at once stay within the bound.
//
// An entry with a binary and a build whose row the store still has must name
// the binary that build holds, staged or done; any other is ErrConflict, as
// from a builder whose lease passed to another attempt. That check runs under
// the installation lock, which Sweep holds to delete, so the binary is still
// referenced when the entry starts to reference it. An entry whose build row
// is gone keeps the catalog's own reference: the catalog is the authority for
// which binaries exist, and the platform registers a build before it ends.
func (s *Store) SaveRuntime(ctx context.Context, e artifact.RuntimeEntry) error {
	if err := artifact.ValidateRuntimeEntry(e); err != nil {
		return err
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	var confirmed *time.Time
	if !e.Confirmed.IsZero() {
		at := e.Confirmed.UTC()
		confirmed = &at
	}
	hello := e.Hello
	if hello == nil {
		hello = []byte{}
	}
	manifest := e.Manifest
	if manifest == nil {
		manifest = []byte{}
	}
	var bin artifact.RuntimeBinary
	if e.Binary != nil {
		bin = *e.Binary
	}
	return s.inTx(ctx, "save runtime", func(tx pgx.Tx) error {
		if e.Binary != nil && e.Build != "" {
			if _, err := lockInstallation(ctx, tx); err != nil {
				return err
			}
			var exists, holds bool
			if err := tx.QueryRow(ctx, `SELECT true, (staged_binary_digest = $2 AND staged_binary_bytes = $3) OR (binary_digest = $2 AND binary_bytes = $3)
				FROM builds WHERE id = $1`, e.Build, bin.Digest, bin.Bytes).Scan(&exists, &holds); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if exists && !holds {
				return fmt.Errorf("%w: build %s does not hold binary %s", artifact.ErrConflict, e.Build, bin.Digest)
			}
		}
		if err := lockFamily(ctx, tx, "runtimes"); err != nil {
			return err
		}
		var others int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM runtimes WHERE name <> $1", e.Name).Scan(&others); err != nil {
			return err
		}
		if others >= artifact.MaxRuntimeEntries {
			return fmt.Errorf("%w: at most %d runtimes", artifact.ErrLimit, artifact.MaxRuntimeEntries)
		}
		_, err := tx.Exec(ctx, `INSERT INTO runtimes (`+runtimeColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
			ON CONFLICT (name) DO UPDATE SET identity = EXCLUDED.identity, image = EXCLUDED.image, digest = EXCLUDED.digest,
				hello = EXCLUDED.hello, registered_by = EXCLUDED.registered_by, registered_at = EXCLUDED.registered_at,
				confirmed_at = EXCLUDED.confirmed_at, binary_digest = EXCLUDED.binary_digest, binary_bytes = EXCLUDED.binary_bytes,
				binary_os = EXCLUDED.binary_os, binary_arch = EXCLUDED.binary_arch, build_id = EXCLUDED.build_id,
				manifest = EXCLUDED.manifest,
				suspect_cause = '', suspect_pool = '', suspect_at = NULL`,
			e.Name, e.Identity, e.Image, e.Digest, hello, e.RegisteredBy, e.Registered.UTC(), confirmed,
			bin.Digest, bin.Bytes, bin.OS, bin.Arch, e.Build, manifest)
		return err
	})
}

// Runtime returns name's entry.
func (s *Store) Runtime(ctx context.Context, name string) (artifact.RuntimeEntry, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.RuntimeEntry{}, err
	}
	e, err := scanRuntime(s.pool.QueryRow(ctx, "SELECT "+runtimeRead+" FROM runtimes WHERE name = $1", name))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.RuntimeEntry{}, artifact.ErrNotFound
	}
	return e, storageErr("runtime", err)
}

// RuntimeEntries returns every entry, by name.
func (s *Store) RuntimeEntries(ctx context.Context) ([]artifact.RuntimeEntry, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+runtimeRead+" FROM runtimes ORDER BY name COLLATE \"C\"")
	if err != nil {
		return nil, storageErr("runtimes", err)
	}
	defer rows.Close()
	out := []artifact.RuntimeEntry{}
	for rows.Next() {
		e, err := scanRuntime(rows)
		if err != nil {
			return nil, storageErr("runtimes", err)
		}
		out = append(out, e)
	}
	return out, storageErr("runtimes", rows.Err())
}

// ConfirmRuntimes confirms every unconfirmed entry of identity at at.
func (s *Store) ConfirmRuntimes(ctx context.Context, identity string, at time.Time) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "UPDATE runtimes SET confirmed_at = $2 WHERE identity = $1 AND confirmed_at IS NULL", identity, at.UTC())
	return storageErr("confirm runtimes", err)
}

// DeleteRuntime removes name's entry. The catalog's advisory lock orders it
// with SetDefaultRuntime, so the default never names a removed entry.
func (s *Store) DeleteRuntime(ctx context.Context, name string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "delete runtime", func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "runtimes"); err != nil {
			return err
		}
		var isDefault bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM default_runtime WHERE name = $1)", name).Scan(&isDefault); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM runtimes WHERE name = $1", name)
		switch {
		case err != nil:
			return err
		case tag.RowsAffected() == 0:
			return artifact.ErrNotFound
		case isDefault:
			return fmt.Errorf("%w: runtime %s is the default runtime", artifact.ErrConflict, name)
		}
		return nil
	})
}

// DefaultRuntime returns the default runtime.
func (s *Store) DefaultRuntime(ctx context.Context) (artifact.DefaultRuntime, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	var d artifact.DefaultRuntime
	err := s.pool.QueryRow(ctx, "SELECT name, set_by, set_at FROM default_runtime").Scan(&d.Name, &d.SetBy, &d.Set)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.DefaultRuntime{}, artifact.ErrNotFound
	}
	d.Set = d.Set.UTC()
	return d, storageErr("default runtime", err)
}

// SetDefaultRuntime records the default runtime under the catalog's
// advisory lock (DeleteRuntime).
func (s *Store) SetDefaultRuntime(ctx context.Context, d artifact.DefaultRuntime, entry bool) (artifact.DefaultRuntime, error) {
	if err := artifact.ValidateDefaultRuntime(d); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	if err := s.ready(ctx); err != nil {
		return artifact.DefaultRuntime{}, err
	}
	var previous artifact.DefaultRuntime
	err := s.inTx(ctx, "set default runtime", func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "runtimes"); err != nil {
			return err
		}
		if entry {
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM runtimes WHERE name = $1)", d.Name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: runtime %s", artifact.ErrNotFound, d.Name)
			}
		}
		err := tx.QueryRow(ctx, "SELECT name, set_by, set_at FROM default_runtime").Scan(&previous.Name, &previous.SetBy, &previous.Set)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		previous.Set = previous.Set.UTC()
		_, err = tx.Exec(ctx, `INSERT INTO default_runtime (id, name, set_by, set_at) VALUES ('default', $1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at`,
			d.Name, d.SetBy, d.Set.UTC())
		return err
	})
	if err != nil {
		return artifact.DefaultRuntime{}, err
	}
	return previous, nil
}

// MarkSuspect marks every unmarked entry whose binary has digest.
func (s *Store) MarkSuspect(ctx context.Context, digest string, m artifact.Suspicion) (int, error) {
	if err := artifact.ValidateSuspicion(m); err != nil {
		return 0, err
	}
	if err := s.ready(ctx); err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runtimes SET suspect_cause = $2, suspect_pool = $3, suspect_at = $4
		WHERE binary_digest = $1 AND suspect_at IS NULL`, digest, m.Cause, m.Pool, m.At.UTC())
	if err != nil {
		return 0, storageErr("mark suspect", err)
	}
	return int(tag.RowsAffected()), nil
}
