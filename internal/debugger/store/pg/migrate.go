package pg

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Migrations are numbered SQL files applied forward only, in one transaction,
// under a transaction-scoped advisory lock, so instances starting together
// apply each once. schema_migrations records every applied file's version,
// name and SHA-256; a store refuses to start when an applied file has changed
// or the database is ahead of the binary.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

var migrationName = regexp.MustCompile(`^(\d{3})_[a-z0-9_]+\.sql$`)

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// loadMigrations reads migrations/NNN_name.sql from fsys; the numbers must
// run from 1 without gaps.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("%w: migrations: %w", artifact.ErrCorrupt, err)
	}
	var out []migration
	for _, e := range entries {
		m := migrationName.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			return nil, fmt.Errorf("%w: migration file %q is not named NNN_name.sql", artifact.ErrCorrupt, e.Name())
		}
		raw, err := fs.ReadFile(fsys, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("%w: migrations: %w", artifact.ErrCorrupt, err)
		}
		version, _ := strconv.Atoi(m[1])
		sum := sha256.Sum256(raw)
		out = append(out, migration{version: version, name: e.Name(), sql: string(raw), checksum: hex.EncodeToString(sum[:])})
		if version != len(out) {
			return nil, fmt.Errorf("%w: migration numbers must run from 001 without gaps", artifact.ErrCorrupt)
		}
	}
	return out, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	migrations, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storageErr("migrate", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	// The lock is per schema, so installations in different schemas of one
	// database do not wait for each other.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('linea.debugger.store.migrate:' || current_schema(), 0))"); err != nil {
		return storageErr("migrate", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer     PRIMARY KEY,
		name       text        NOT NULL,
		checksum   text        NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return storageErr("migrate", err)
	}
	rows, err := tx.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return storageErr("migrate", err)
	}
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return storageErr("migrate", err)
		}
		if version > len(migrations) {
			rows.Close()
			return fmt.Errorf("%w: database has migration %s, newer than this binary", artifact.ErrUnsupportedSchema, name)
		}
		if version != applied+1 || migrations[version-1].name != name || migrations[version-1].checksum != checksum {
			rows.Close()
			return fmt.Errorf("%w: applied migration %s was changed", artifact.ErrCorrupt, name)
		}
		applied = version
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return storageErr("migrate", err)
	}
	for _, m := range migrations[applied:] {
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return storageErr("migrate "+m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)", m.version, m.name, m.checksum); err != nil {
			return storageErr("migrate "+m.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return storageErr("migrate", err)
	}
	return nil
}
