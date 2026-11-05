package livecapture

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lineahq/linea/pluginsdk"
)

//go:embed migrations/001_initial.sql
var schema string

//go:embed migrations/002_core_credentials.sql
var coreCredentials string

//go:embed migrations/003_acting_for.sql
var actingFor string

// migrations are applied in order; version N is migrations[N-1].
var migrations = []string{schema, coreCredentials, actingFor}

func checksum(migration string) string {
	sum := sha256.Sum256([]byte(migration))
	return hex.EncodeToString(sum[:])
}

const ownerLock int64 = 734249871234

var ErrOwnerBusy = fmt.Errorf("plugin authority already active: %w", ErrConflict)

type Store struct {
	conn      *pgx.Conn
	gate      chan struct{}
	closed    atomic.Bool
	authority string
	observed  storageMetrics
}

func (s *Store) AuthorityID() string { return s.authority }

func connect(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, ErrUnavailable
	}
	cfg.ConnectTimeout = 3 * time.Second
	cfg.RuntimeParams["application_name"] = "linea-fleet-livecapture"
	cfg.RuntimeParams["statement_timeout"] = "1500"
	cfg.RuntimeParams["lock_timeout"] = "500"
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "3000"
	cfg.RuntimeParams["tcp_keepalives_idle"] = "2"
	cfg.RuntimeParams["tcp_keepalives_interval"] = "1"
	cfg.RuntimeParams["tcp_keepalives_count"] = "2"
	cfg.RuntimeParams["tcp_user_timeout"] = "5000"
	cfg.RuntimeParams["idle_session_timeout"] = "10000"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, ErrUnavailable
	}
	var locked bool
	if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", ownerLock).Scan(&locked); err != nil || !locked {
		_ = conn.Close(context.Background())
		if err != nil {
			return nil, ErrUnavailable
		}
		return nil, ErrOwnerBusy
	}
	return &Store{conn: conn, gate: make(chan struct{}, 1)}, nil
}
func (s *Store) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.gate <- struct{}{}
	defer func() { <-s.gate }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.conn.Close(ctx)
}
func (s *Store) storageObservations() *storageMetrics { return &s.observed }
func (s *Store) access(ctx context.Context, fn func(context.Context) error) (result error) {
	defer func() {
		if result != nil {
			s.observed.errors.Add(1)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	waiting := time.Now()
	s.observed.waiters.Add(1)
	select {
	case s.gate <- struct{}{}:
		s.observed.waiters.Add(-1)
		s.observed.wait.Observe(time.Since(waiting))
		defer func() { <-s.gate }()
	case <-ctx.Done():
		s.observed.waiters.Add(-1)
		s.observed.wait.Observe(time.Since(waiting))
		return ErrUnavailable
	}
	started := time.Now()
	defer func() { s.observed.duration.Observe(time.Since(started)) }()
	if s.closed.Load() {
		return ErrUnavailable
	}
	return normalized(fn(ctx))
}
func normalized(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrCapacity) || errors.Is(err, ErrPrecondition) || errors.Is(err, ErrForbidden) {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrConflict
	}
	return ErrUnavailable
}

// applied verifies that the recorded versions are an untampered prefix of
// migrations and returns how many are applied.
func applied(ctx context.Context, c *pgx.Conn) (int, error) {
	rows, err := c.Query(ctx, "SELECT version,checksum FROM capture.schema_version ORDER BY version LIMIT 64")
	if err != nil {
		return 0, ErrUnavailable
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var version int
		var sum string
		if rows.Scan(&version, &sum) != nil {
			return 0, ErrUnavailable
		}
		if count >= len(migrations) || version != count+1 || sum != checksum(migrations[count]) {
			return 0, ErrConflict
		}
		count++
	}
	if rows.Err() != nil {
		return 0, ErrUnavailable
	}
	return count, nil
}

func check(ctx context.Context, c *pgx.Conn) error {
	count, err := applied(ctx, c)
	if err == nil && count != len(migrations) {
		err = ErrConflict
	}
	return err
}
func Migrate(ctx context.Context, url, runtimeRole string) error {
	s, err := connect(ctx, url)
	if err != nil {
		return err
	}
	defer s.Close()
	var exists bool
	if s.conn.QueryRow(ctx, "SELECT to_regclass('capture.schema_version') IS NOT NULL").Scan(&exists) != nil {
		return ErrUnavailable
	}
	done := 0
	if exists {
		if done, err = applied(ctx, s.conn); err != nil {
			return err
		}
	}
	if done < len(migrations) {
		tx, err := s.conn.Begin(ctx)
		if err != nil {
			return ErrUnavailable
		}
		defer tx.Rollback(context.Background())
		for i := done; i < len(migrations); i++ {
			if _, err = tx.Exec(ctx, migrations[i]); err != nil {
				return ErrUnavailable
			}
			if _, err = tx.Exec(ctx, "INSERT INTO capture.schema_version(version,checksum) VALUES($1,$2)", i+1, checksum(migrations[i])); err != nil {
				return ErrUnavailable
			}
		}
		if tx.Commit(ctx) != nil {
			return ErrUnavailable
		}
	}
	if runtimeRole != "" {
		role := pgx.Identifier{runtimeRole}.Sanitize()
		if _, err = s.conn.Exec(ctx, "GRANT USAGE ON SCHEMA capture TO "+role+"; GRANT SELECT ON capture.schema_version TO "+role+"; GRANT SELECT,INSERT ON capture.metadata TO "+role+"; GRANT SELECT,INSERT,UPDATE ON capture.peers,capture.operations TO "+role+"; GRANT SELECT,INSERT,DELETE ON capture.leases TO "+role); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}
func Open(ctx context.Context, url, pluginID string) (*Store, error) {
	s, err := connect(ctx, url)
	if err != nil {
		return nil, err
	}
	if err = check(ctx, s.conn); err == nil {
		_, err = s.conn.Exec(ctx, "INSERT INTO capture.metadata(singleton,plugin_id,authority_id) VALUES(true,$1,$2) ON CONFLICT DO NOTHING", pluginID, pluginsdk.RandomID())
		if err == nil {
			var storedID string
			err = s.conn.QueryRow(ctx, "SELECT plugin_id,authority_id FROM capture.metadata WHERE singleton").Scan(&storedID, &s.authority)
			if err == nil && storedID != pluginID {
				err = ErrConflict
			}
		}
	}
	if err == nil {
		_, err = s.conn.Exec(ctx, `UPDATE capture.operations SET state='interrupted',reason='authority_restarted',revision=revision+1,
targets=(SELECT jsonb_agg(jsonb_set(x,'{state}','"unknown"')) FROM jsonb_array_elements(targets) x)
WHERE state IN ('starting','active','stopping')`)
	}
	if err != nil {
		s.Close()
		return nil, normalized(err)
	}
	return s, nil
}
func (s *Store) Ready(ctx context.Context) error {
	return s.access(ctx, func(ctx context.Context) error { return s.conn.Ping(ctx) })
}
func (s *Store) NextGeneration(ctx context.Context, id string) (uint64, error) {
	var gen uint64
	err := s.access(ctx, func(ctx context.Context) error {
		var count int
		if err := s.conn.QueryRow(ctx, "SELECT count(*) FROM capture.peers").Scan(&count); err != nil {
			return err
		}
		var exists bool
		if err := s.conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM capture.peers WHERE id=$1)", id).Scan(&exists); err != nil {
			return err
		}
		if !exists && count >= 10000 {
			return ErrCapacity
		}
		return s.conn.QueryRow(ctx, "INSERT INTO capture.peers(id,generation) VALUES($1,1) ON CONFLICT(id) DO UPDATE SET generation=capture.peers.generation+1 RETURNING generation", id).Scan(&gen)
	})
	return gen, err
}
func scanOperation(row pgx.Row) (Operation, error) {
	var op Operation
	var encoded, acting []byte
	err := row.Scan(&op.ID, &op.Owner, &op.Hash, &op.State, &op.Reason, &op.Revision, &op.ExpiresAt, &op.ChannelID, &encoded, &acting)
	if err != nil {
		return op, err
	}
	if acting != nil && json.Unmarshal(acting, &op.ActingFor) != nil {
		return op, ErrUnavailable
	}
	var targets []StoredTarget
	if json.Unmarshal(encoded, &targets) != nil {
		return op, ErrUnavailable
	}
	for _, t := range targets {
		op.Targets = append(op.Targets, Target(t))
	}
	return op, nil
}

const operationColumns = "id,owner_id,request_hash,state,reason,revision,expires_at,channel_id,targets,acting_for"

func (s *Store) Get(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := s.access(ctx, func(ctx context.Context) error {
		var err error
		op, err = scanOperation(s.conn.QueryRow(ctx, "SELECT "+operationColumns+" FROM capture.operations WHERE id=$1", id))
		op.AuthorityID = s.authority
		return err
	})
	return op, err
}
func (s *Store) Insert(ctx context.Context, op Operation) error {
	return s.access(ctx, func(ctx context.Context) error {
		tx, err := s.conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		var count int
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM capture.operations").Scan(&count); err != nil {
			return err
		}
		if count >= 10000 {
			return ErrCapacity
		}
		if _, err = tx.Exec(ctx, "DELETE FROM capture.leases WHERE expires_at<=now()"); err != nil {
			return err
		}
		encoded, _ := json.Marshal(storedTargets(op.Targets))
		var acting []byte
		if op.ActingFor != nil {
			acting, _ = json.Marshal(op.ActingFor)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO capture.operations("+operationColumns+") VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", op.ID, op.Owner, op.Hash, op.State, op.Reason, op.Revision, op.ExpiresAt, op.ChannelID, encoded, acting); err != nil {
			return err
		}
		for _, t := range op.Targets {
			var gen uint64
			if err = tx.QueryRow(ctx, "SELECT generation FROM capture.peers WHERE id=$1", t.PeerID).Scan(&gen); err != nil {
				return err
			}
			if gen != t.Generation {
				return ErrConflict
			}
			if _, err = tx.Exec(ctx, "INSERT INTO capture.leases(peer_id,operation_id,expires_at) VALUES($1,$2,$3)", t.PeerID, op.ID, op.ExpiresAt.Add(5*time.Second)); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
}
func (s *Store) Save(ctx context.Context, op Operation, previous int64) error {
	return s.access(ctx, func(ctx context.Context) error {
		tx, err := s.conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		encoded, _ := json.Marshal(storedTargets(op.Targets))
		result, err := tx.Exec(ctx, "UPDATE capture.operations SET state=$2,revision=$3,targets=$4,reason=$6 WHERE id=$1 AND revision=$5", op.ID, op.State, op.Revision, encoded, previous, op.Reason)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return ErrPrecondition
		}
		for _, t := range op.Targets {
			if t.Released {
				if _, err = tx.Exec(ctx, "DELETE FROM capture.leases WHERE peer_id=$1 AND operation_id=$2", t.PeerID, op.ID); err != nil {
					return err
				}
			}
		}
		return tx.Commit(ctx)
	})
}

// Purge is an offline operator job. It acquires the same exclusive authority
// lock and removes metadata only after the full expiry guard, never live leases.
func Purge(ctx context.Context, url string, before time.Time) (int64, error) {
	if before.IsZero() || before.After(time.Now()) {
		return 0, ErrInvalid
	}
	s, err := connect(ctx, url)
	if err != nil {
		return 0, err
	}
	defer s.Close()
	if err := check(ctx, s.conn); err != nil {
		return 0, err
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "DELETE FROM capture.leases WHERE expires_at<=$1", before); err != nil {
		return 0, ErrUnavailable
	}
	result, err := tx.Exec(ctx, "DELETE FROM capture.operations op WHERE op.expires_at+interval '5 seconds'<=$1 AND NOT EXISTS(SELECT 1 FROM capture.leases l WHERE l.operation_id=op.id)", before)
	if err != nil {
		return 0, ErrUnavailable
	}
	return result.RowsAffected(), normalized(tx.Commit(ctx))
}
