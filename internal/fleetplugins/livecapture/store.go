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

// maxPeerIdentities caps the durable peer identities (capture.peers rows).
const maxPeerIdentities = 10000

//go:embed migrations/001_initial.sql
var schema string

//go:embed migrations/002_core_credentials.sql
var coreCredentials string

//go:embed migrations/003_acting_for.sql
var actingFor string

//go:embed migrations/004_peer_retention.sql
var peerRetentionMigration string

// migrations are applied in order; version N is migrations[N-1].
var migrations = []string{schema, coreCredentials, actingFor, peerRetentionMigration}

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

// dial opens a session with the plugin's timeouts and without the authority
// lock. Only the online purge uses it on its own.
func dial(ctx context.Context, url string) (*pgx.Conn, error) {
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
	return conn, nil
}
func connect(ctx context.Context, url string) (*Store, error) {
	conn, err := dial(ctx, url)
	if err != nil {
		return nil, err
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

// enter waits for the connection until ctx ends and returns its release.
func (s *Store) enter(ctx context.Context) (func(), error) {
	waiting := time.Now()
	s.observed.waiters.Add(1)
	select {
	case s.gate <- struct{}{}:
		s.observed.waiters.Add(-1)
		s.observed.wait.Observe(time.Since(waiting))
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		s.observed.waiters.Add(-1)
		s.observed.wait.Observe(time.Since(waiting))
		return nil, ErrUnavailable
	}
}
func (s *Store) access(ctx context.Context, fn func(context.Context) error) (result error) {
	defer func() {
		if result != nil {
			s.observed.errors.Add(1)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	release, err := s.enter(ctx)
	if err != nil {
		return err
	}
	defer release()
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
		if _, err = s.conn.Exec(ctx, runtimeGrants(pgx.Identifier{runtimeRole}.Sanitize())); err != nil {
			return ErrUnavailable
		}
	}
	return nil
}

// runtimeGrants is what the restricted runtime role may do. role is already
// quoted. Operations are never deleted by the runtime role (the purge uses the
// migration role); peers are, by the plugin's own retention.
func runtimeGrants(role string) string {
	return "GRANT USAGE ON SCHEMA capture TO " + role +
		"; GRANT SELECT ON capture.schema_version TO " + role +
		"; GRANT SELECT,INSERT ON capture.metadata TO " + role +
		"; GRANT UPDATE (generation_floor) ON capture.metadata TO " + role +
		"; GRANT SELECT,INSERT,UPDATE ON capture.operations TO " + role +
		"; GRANT SELECT,INSERT,UPDATE,DELETE ON capture.peers TO " + role +
		"; GRANT SELECT,INSERT,DELETE ON capture.leases TO " + role
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

// probeQueueWait and probeTimeout bound the liveness probe. The probe waits
// behind other statements for at most probeQueueWait; that wait is not part of
// the sample. Only the ping itself, given probeTimeout from the moment it owns
// the connection, can fail it.
const (
	probeQueueWait = time.Second
	probeTimeout   = 2 * time.Second
)

// Ready probes the database through the working connection. It returns
// ErrStorageBusy when the connection stayed occupied (no sample), ErrStorageLost
// when it is closed, and ErrUnavailable when the ping failed on a live one.
// Only the server's maintenance loop calls it.
func (s *Store) Ready(ctx context.Context) error {
	queue, cancel := context.WithTimeout(ctx, probeQueueWait)
	release, err := s.enter(queue)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ErrUnavailable
		}
		return ErrStorageBusy
	}
	defer release()
	if s.closed.Load() || s.conn.IsClosed() {
		s.observed.errors.Add(1)
		return ErrStorageLost
	}
	started := time.Now()
	defer func() { s.observed.duration.Observe(time.Since(started)) }()
	ping, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := s.conn.Ping(ping); err != nil {
		s.observed.errors.Add(1)
		if s.conn.IsClosed() {
			return ErrStorageLost
		}
		return ErrUnavailable
	}
	return nil
}
func (s *Store) NextGeneration(ctx context.Context, id string) (uint64, error) {
	var gen uint64
	err := s.access(ctx, func(ctx context.Context) error {
		// One statement: a known peer gets its next generation; a new one is
		// admitted below the identity cap and starts above every generation
		// ever deleted (the floor), so a peer that comes back after retention
		// never repeats a generation. No row means the cap refused it.
		err := s.conn.QueryRow(ctx, `INSERT INTO capture.peers(id,generation)
SELECT $1,(SELECT generation_floor FROM capture.metadata WHERE singleton)+1
WHERE EXISTS(SELECT 1 FROM capture.peers WHERE id=$1) OR (SELECT count(*) FROM capture.peers)<$2
ON CONFLICT(id) DO UPDATE SET generation=capture.peers.generation+1,seen_at=now()
RETURNING generation`, id, maxPeerIdentities).Scan(&gen)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapacity
		}
		return err
	})
	return gen, err
}

// MaintainPeers refreshes the connected peers and deletes at most limit peers
// that have been unused for idle: not connected, not seen since the cutoff, no
// lease and no retained operation refers to them. The highest deleted
// generation is recorded in generation_floor in the same statement. It returns
// how many peers were deleted.
func (s *Store) MaintainPeers(ctx context.Context, connected []string, idle time.Duration, limit int) (int, error) {
	var deleted int
	err := s.access(ctx, func(ctx context.Context) error {
		if len(connected) > 0 {
			if _, err := s.conn.Exec(ctx, "UPDATE capture.peers SET seen_at=now() WHERE id=ANY($1)", connected); err != nil {
				return err
			}
		}
		if connected == nil {
			connected = []string{}
		}
		return s.conn.QueryRow(ctx, `WITH gone AS (
 DELETE FROM capture.peers WHERE id IN (
  SELECT p.id FROM capture.peers p
  WHERE p.seen_at < now()-make_interval(secs=>$1)
   AND NOT p.id=ANY($2)
   AND NOT EXISTS(SELECT 1 FROM capture.leases l WHERE l.peer_id=p.id)
   AND p.id NOT IN (SELECT t->>'peer_id' FROM capture.operations o, jsonb_array_elements(o.targets) t WHERE t->>'peer_id' IS NOT NULL)
  ORDER BY p.seen_at LIMIT $3)
 RETURNING generation
), bump AS (
 UPDATE capture.metadata SET generation_floor=GREATEST(generation_floor,(SELECT max(generation) FROM gone))
 WHERE singleton AND EXISTS(SELECT 1 FROM gone)
)
SELECT count(*) FROM gone`, idle.Seconds(), connected, limit).Scan(&deleted)
	})
	return deleted, err
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
			// A lease left by an earlier operation, not yet released or
			// expired, means the Collector may still run that capture.
			leased, err := tx.Exec(ctx, "INSERT INTO capture.leases(peer_id,operation_id,expires_at) VALUES($1,$2,$3) ON CONFLICT (peer_id) DO NOTHING", t.PeerID, op.ID, op.ExpiresAt.Add(5*time.Second))
			if err != nil {
				return err
			}
			if leased.RowsAffected() == 0 {
				return ErrPeerBusy
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

// Purge is an operator job that runs beside the running plugin and takes no
// authority lock. It removes an operation only when all of these hold: it is
// in a terminal state, its absolute expiry plus the 5 s guard is not after
// before, and no lease refers to it. A running plugin writes an operation row
// only while it is not terminal or while one of its targets still holds a
// lease, so the purge never deletes a row the plugin will save again. Leases
// are removed only after their own expiry. Peers are not touched; the plugin
// retires unused peers itself.
func Purge(ctx context.Context, url string, before time.Time) (int64, error) {
	if before.IsZero() || before.After(time.Now()) {
		return 0, ErrInvalid
	}
	conn, err := dial(ctx, url)
	if err != nil {
		return 0, err
	}
	defer func() {
		closing, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()
	if err := check(ctx, conn); err != nil {
		return 0, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "DELETE FROM capture.leases WHERE expires_at<=$1", before); err != nil {
		return 0, ErrUnavailable
	}
	result, err := tx.Exec(ctx, "DELETE FROM capture.operations op WHERE op.state IN ('completed','stopped','failed','expired','interrupted') AND op.expires_at+interval '5 seconds'<=$1 AND NOT EXISTS(SELECT 1 FROM capture.leases l WHERE l.operation_id=op.id)", before)
	if err != nil {
		return 0, ErrUnavailable
	}
	return result.RowsAffected(), normalized(tx.Commit(ctx))
}
