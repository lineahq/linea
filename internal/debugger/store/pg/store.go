// Package pg implements artifact.Repository on PostgreSQL and S3-compatible
// object storage.
//
// PostgreSQL holds every row: artifacts, revisions, the run journal,
// evaluations, suites, tombstones, capture metadata, audit, tokens and
// sessions. Object storage holds every byte payload, content-addressed under
// <prefix>/sha256/<hex>; rows keep the digest and size. A writer uploads its
// objects before it commits the row that references them, so a row never
// names a missing object; a failed or refused write can leave an unreferenced
// object, which Sweep removes.
//
// Several instances share one database. No in-process state is a source of
// truth: every write that must hold across instances (revision, run, suite and
// capture counts, storage limits, ordinals, optimistic generations, unique
// hashes, the audit sequence) runs in a transaction under a row lock or a
// constraint.
package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// S3Config selects an S3-compatible bucket. Every object key starts with
// Prefix, which scopes one installation inside a shared bucket.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	Region    string
	UseTLS    bool
	// UsePathStyle addresses buckets as endpoint/bucket/key instead of
	// bucket.endpoint/key, which most S3-compatible servers need.
	UsePathStyle bool
}

// Config opens a Store.
type Config struct {
	// DatabaseURL is a PostgreSQL connection URL. Tables are created in, and
	// resolved through, the connection's search_path.
	DatabaseURL string
	S3          S3Config
	// Objects replaces the S3 client built from S3, for tests; S3.Prefix still
	// scopes its keys.
	Objects ObjectStore
	Limits  artifact.Limits
	// MaxConns bounds the connection pool; nonpositive means 8.
	MaxConns int32
}

// Store is an artifact.Repository over PostgreSQL and object storage.
type Store struct {
	pool    *pgxpool.Pool
	objects ObjectStore
	prefix  string
	limits  artifact.Limits
	id      string

	closed    atomic.Bool
	closeOnce sync.Once
	// ctx ends background work, the hint listener, at Close.
	ctx       context.Context
	stop      context.CancelFunc
	loops     sync.WaitGroup
	hintsOnce sync.Once
	hints     chan struct{}
	// leadMu guards lead, the session that holds the housekeeping lock while
	// this handle leads the upkeep (Lead).
	leadMu sync.Mutex
	lead   *pgx.Conn
	// generation is the last workspace generation read, returned by Info when
	// the database cannot be reached.
	generation atomic.Uint64
}

var _ artifact.Repository = (*Store)(nil)

// Open connects, applies pending migrations under an advisory lock, and
// returns the store of the installation in the database, creating its ID on
// first use.
func Open(ctx context.Context, cfg Config) (_ *Store, err error) {
	if cfg.DatabaseURL == "" {
		return nil, invalid("database URL is required")
	}
	objects := cfg.Objects
	if objects == nil {
		if objects, err = newS3(ctx, cfg.S3); err != nil {
			return nil, err
		}
	}
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, invalid("database URL is invalid")
	}
	pc.MaxConns = 8
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "linea-debugger"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("%w: connect: %w", artifact.ErrStorage, err)
	}
	defer func() {
		if err != nil {
			pool.Close()
		}
	}()
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("%w: connect: %w", artifact.ErrStorage, err)
	}
	if err := migrate(ctx, pool, migrationFiles); err != nil {
		return nil, err
	}
	s := &Store{pool: pool, objects: objects, prefix: cleanPrefix(cfg.S3.Prefix), limits: cfg.Limits.Defaults()}
	s.ctx, s.stop = context.WithCancel(context.Background())
	if s.id, err = s.installation(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// installation returns the installation ID, generating and persisting one on
// first use. Instances starting together agree on the first writer's ID.
func (s *Store) installation(ctx context.Context) (string, error) {
	var id string
	var err error
	// An instance that loses the insert race sees no row until the winner
	// commits; the statement is retried.
	for range 5 {
		err = s.pool.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO installation (id) VALUES ($1) ON CONFLICT (singleton) DO NOTHING RETURNING id
			)
			SELECT id FROM ins UNION ALL SELECT id FROM installation LIMIT 1`, newID()).Scan(&id)
		if !errors.Is(err, pgx.ErrNoRows) {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("%w: installation: %w", artifact.ErrStorage, err)
	}
	return id, nil
}

// Close closes the connection pool; later calls fail with ErrClosed.
func (s *Store) Close() {
	s.closed.Store(true)
	s.closeOnce.Do(func() {
		s.stop()
		s.loops.Wait()
		s.resign()
		s.pool.Close()
	})
}

// Limits returns the effective limits (nonpositive values replaced by their
// defaults).
func (s *Store) Limits() artifact.Limits { return s.limits }

// Info returns the installation's ID, schema and workspace generation (the
// number of revisions).
func (s *Store) Info() artifact.Info {
	info := artifact.Info{ID: s.id, Schema: artifact.SchemaVersion, Generation: s.generation.Load()}
	if s.closed.Load() {
		return info
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var g int64
	if s.pool.QueryRow(ctx, "SELECT generation FROM installation").Scan(&g) == nil {
		info.Generation = uint64(g)
		s.generation.Store(info.Generation)
	}
	return info
}

func (s *Store) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return artifact.ErrClosed
	}
	return nil
}

// ObjectKey returns the object-storage key of a digest.
func (s *Store) ObjectKey(digest string) (string, error) { return objectKey(s.prefix, digest) }

// inTx runs fn in a transaction, committing when it returns nil. Errors that
// are not already repository errors become ErrStorage.
func (s *Store) inTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return storageErr(op, err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := fn(tx); err != nil {
		return storageErr(op, err)
	}
	return storageErr(op, tx.Commit(ctx))
}

// storageErr passes repository errors and cancellations through and wraps
// anything else as ErrStorage. Constraint violations that mean "already
// exists" or "still referenced" are conflicts.
func storageErr(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case isRepositoryError(err):
		return err
	}
	switch pgCode(err) {
	case "23505", "23503":
		return fmt.Errorf("%w: %s: %w", artifact.ErrConflict, op, err)
	}
	return fmt.Errorf("%w: %s: %w", artifact.ErrStorage, op, err)
}

func isRepositoryError(err error) bool {
	for _, target := range []error{context.Canceled, context.DeadlineExceeded, artifact.ErrInvalidRequest, artifact.ErrNotFound,
		artifact.ErrConflict, artifact.ErrLocked, artifact.ErrClosed, artifact.ErrCorrupt, artifact.ErrUnsupportedSchema,
		artifact.ErrLimit, artifact.ErrStorage} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func invalid(message string) error { return fmt.Errorf("%w: %s", artifact.ErrInvalidRequest, message) }

func newID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validID(id string) bool { return isLowerHex(id, 32) }

func validRunID(id string) bool { return strings.HasPrefix(id, "run-") && isLowerHex(id[4:], 16) }

func validSuiteID(id string) bool { return strings.HasPrefix(id, "suite-") && isLowerHex(id[6:], 16) }

func validEvaluationID(id string) bool {
	return strings.HasPrefix(id, "eval-") && isLowerHex(id[5:], 16)
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// micro is the precision PostgreSQL keeps; timestamps are truncated before
// they are returned, so a reopened store reads back what a write returned.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// validText reports whether s can be stored in a PostgreSQL text column:
// valid UTF-8 without NUL.
func validText(fields ...string) bool {
	for _, s := range fields {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}
