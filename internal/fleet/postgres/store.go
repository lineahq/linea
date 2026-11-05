// Package postgres owns the fleet schema; it never reads another product's data.
package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk/metrics"
)

//go:embed migrations/001_initial.sql
var migration string

//go:embed migrations/002_deployment_tokens.sql
var deploymentTokensMigration string

//go:embed migrations/003_extension_lifecycle.sql
var lifecycleMigration string

//go:embed migrations/004_replicas.sql
var replicasMigration string

//go:embed migrations/005_inventory.sql
var inventoryMigration string

//go:embed migrations/006_plugins.sql
var pluginsMigration string

//go:embed migrations/007_config_management.sql
var configMigration string

//go:embed migrations/008_rollouts.sql
var rolloutMigration string

//go:embed migrations/009_component_sets.sql
var componentsMigration string

//go:embed migrations/010_acting_for.sql
var actingMigration string

var migrations = []string{migration, deploymentTokensMigration, lifecycleMigration, replicasMigration, inventoryMigration, pluginsMigration, configMigration, rolloutMigration, componentsMigration, actingMigration}

// adminLock serializes offline administration (migrate, bootstrap, grants).
// v1 servers held the same lock, so a v1 server also blocks migration to v2.
const adminLock int64 = 731281604309
const schemaVersion = 10

// ErrOwnerBusy is retryable contention, distinct from schema/identity conflicts.
var ErrOwnerBusy = fmt.Errorf("fleet administration or a v1 server already active: %w", fleet.ErrConflict)

// ErrReplicasActive refuses migration while v2 replicas are alive.
var ErrReplicasActive = fmt.Errorf("fleet replicas are running; stop them before migrating: %w", fleet.ErrConflict)

type Options struct {
	ServerID             string
	MaxAgents, MaxEvents int
	// Heartbeat, Alive, Flush and Rollouts override the replica timing in
	// tests. Zero values select 2 s, 10 s, 10 s and 5 s.
	Heartbeat, Alive, Flush, Rollouts time.Duration
	// Retention deletes agents disconnected for longer; zero keeps them.
	Retention time.Duration
}

type Store struct {
	closeOnce sync.Once
	stopOnce  sync.Once
	closed    atomic.Bool
	stopped   atomic.Bool
	pool      *pgxpool.Pool
	options   Options
	replica   string
	ctx       context.Context
	cancel    context.CancelFunc
	loops     sync.WaitGroup
	writer    *writer

	heartbeat atomic.Int64 // UnixNano of the last successful heartbeat
	leader    atomic.Bool
	lost      chan struct{}
	lostOnce  sync.Once
	lostWhy   string // written once before lost closes

	seenMu sync.Mutex
	seen   map[string]seenAt

	signalMu   sync.Mutex
	onTakeover func(string, uint64)
	onRevoked  func(string)
	onConfig   func(string)
	eventWake  chan struct{}

	queryDuration metrics.Duration
	writeDuration metrics.Duration
	batches       atomic.Uint64
	batchItems    atomic.Uint64
	queryErrors   atomic.Uint64
	writeErrors   atomic.Uint64
	flushErrors   atomic.Uint64
	takeovers     atomic.Uint64
	reaped        atomic.Uint64
	reapBatch     int
	expired       atomic.Uint64
	rolloutErrors atomic.Uint64
	// uses caches the component types of rollout versions.
	uses *configmgmt.UsesCache
}

type seenAt struct {
	generation uint64
	at         time.Time
}

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 3*time.Second)
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system random source unavailable")
	}
	return hex.EncodeToString(b[:])
}

const lookupTimeout = time.Second

func connConfig(url string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	cfg.ConnectTimeout = 3 * time.Second
	// A stopped database host can take seconds to fail name resolution, which
	// would exhaust the connect timeout before the next host is tried.
	cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		return net.DefaultResolver.LookupHost(ctx, host)
	}
	cfg.RuntimeParams["application_name"] = "linea-fleet"
	cfg.RuntimeParams["statement_timeout"] = "2000"
	cfg.RuntimeParams["lock_timeout"] = "1000"
	cfg.RuntimeParams["idle_in_transaction_session_timeout"] = "5000"
	cfg.RuntimeParams["tcp_keepalives_idle"] = "2"
	cfg.RuntimeParams["tcp_keepalives_interval"] = "1"
	cfg.RuntimeParams["tcp_keepalives_count"] = "2"
	cfg.RuntimeParams["tcp_user_timeout"] = "5000"
	return cfg, nil
}

// openAdmin returns one session holding the administration lock.
func openAdmin(ctx context.Context, url string) (*pgx.Conn, error) {
	cfg, err := connConfig(url)
	if err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", adminLock).Scan(&locked); err != nil || !locked {
		closeAdmin(conn)
		if err != nil {
			return nil, fleet.ErrUnavailable
		}
		return nil, ErrOwnerBusy
	}
	return conn, nil
}

// closeAdmin destroys the session rather than leaving its lock behind.
func closeAdmin(conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// adminTx runs one offline administration transaction on the locked session.
func adminTx(ctx context.Context, url string, work func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := openAdmin(ctx, url)
	if err != nil {
		return err
	}
	defer closeAdmin(conn)
	if err := checkSchema(ctx, conn); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fleet.ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if err := work(ctx, tx); err != nil {
		return normalize(err)
	}
	return normalize(tx.Commit(ctx))
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// installedVersion verifies the complete ledger before any schema mutation.
func installedVersion(ctx context.Context, c querier) (int, error) {
	rows, err := c.Query(ctx, "SELECT version,checksum FROM fleet.schema_version ORDER BY version")
	if err != nil {
		return 0, fleet.ErrUnavailable
	}
	defer rows.Close()
	installed := 0
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return 0, fleet.ErrUnavailable
		}
		if version != installed+1 || version > len(migrations) {
			return 0, fleet.ErrConflict
		}
		sum := sha256.Sum256([]byte(migrations[version-1]))
		if checksum != hex.EncodeToString(sum[:]) {
			return 0, fleet.ErrConflict
		}
		installed = version
	}
	if err := rows.Err(); err != nil {
		return 0, fleet.ErrUnavailable
	}
	if installed == 0 {
		return 0, fleet.ErrConflict
	}
	return installed, nil
}

func checkSchema(ctx context.Context, c querier) error {
	version, err := installedVersion(ctx, c)
	if err != nil {
		return err
	}
	if version != schemaVersion {
		return fleet.ErrConflict
	}
	return nil
}

func Migrate(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := openAdmin(ctx, url)
	if err != nil {
		return err
	}
	defer closeAdmin(conn)
	var exists, replicas bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('fleet.schema_version') IS NOT NULL, to_regclass('fleet.replicas') IS NOT NULL").Scan(&exists, &replicas); err != nil {
		return fleet.ErrUnavailable
	}
	installed := 0
	if exists {
		installed, err = installedVersion(ctx, conn)
		if err != nil {
			return err
		}
	}
	if installed == schemaVersion {
		return nil
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fleet.ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if replicas {
		// Lock out replica registration for the rest of the migration.
		if _, err := tx.Exec(ctx, "LOCK TABLE fleet.replicas IN EXCLUSIVE MODE"); err != nil {
			return fleet.ErrUnavailable
		}
		var alive bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.replicas WHERE heartbeat_at > now() - interval '10 seconds')").Scan(&alive); err != nil {
			return fleet.ErrUnavailable
		}
		if alive {
			return ErrReplicasActive
		}
	}
	for version := installed + 1; version <= schemaVersion; version++ {
		source := migrations[version-1]
		if _, err = tx.Exec(ctx, source); err != nil {
			return fleet.ErrUnavailable
		}
		sum := sha256.Sum256([]byte(source))
		if _, err = tx.Exec(ctx, "INSERT INTO fleet.schema_version(version,checksum) VALUES($1,$2)", version, hex.EncodeToString(sum[:])); err != nil {
			return fleet.ErrUnavailable
		}
	}
	return normalize(tx.Commit(ctx))
}

// Open registers a new replica. Any number of replicas may share a database.
func Open(ctx context.Context, url string, options Options) (*Store, error) {
	if !auth.SafeName(options.ServerID) || options.MaxAgents < 1 || options.MaxEvents < 1 {
		return nil, fleet.ErrInvalid
	}
	options.Heartbeat = cmp(options.Heartbeat, 2*time.Second)
	options.Alive = cmp(options.Alive, 10*time.Second)
	options.Flush = cmp(options.Flush, 10*time.Second)
	options.Rollouts = cmp(options.Rollouts, 5*time.Second)
	cfg, err := connConfig(url)
	if err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	poolConfig.ConnConfig = cfg
	poolConfig.MaxConns, poolConfig.MinConns = 8, 1
	s := &Store{options: options, replica: randomID(), lost: make(chan struct{}), seen: map[string]seenAt{}, eventWake: make(chan struct{}), reapBatch: reapBatch,
		uses: configmgmt.NewUsesCache()}
	cfg.Tracer = s
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fleet.ErrUnavailable
	}
	s.pool = pool
	s.ctx, s.cancel = context.WithCancel(context.Background())
	if err := s.register(ctx); err != nil {
		s.cancel()
		pool.Close()
		return nil, err
	}
	s.heartbeat.Store(time.Now().UnixNano())
	s.writer = newWriter(s)
	s.loops.Add(5)
	go s.writer.run()
	go s.heartbeatLoop()
	go s.flushLoop()
	go s.listen()
	go s.rolloutLoop()
	return s, nil
}

func cmp(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func (s *Store) register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := checkSchema(ctx, s.pool); err != nil {
		return err
	}
	return s.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := identity(ctx, tx, s.options.ServerID); err != nil {
			return err
		}
		var admins int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.credentials WHERE role='admin' AND revoked_at IS NULL AND expires_at>now()").Scan(&admins); err != nil {
			return err
		}
		if admins == 0 {
			return auth.ErrUnauthorized
		}
		if err := s.checkStream(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO fleet.replicas(id) VALUES($1)", s.replica); err != nil {
			return err
		}
		return s.event(ctx, tx, "replica.started", s.replica, "server", nil)
	})
}

// checkStream rotates the event epoch once when the database is restored into
// another cluster, detected by system identifier or a transaction counter
// that moved backwards. Old events keep their order ahead of new ones.
func (s *Store) checkStream(ctx context.Context, tx pgx.Tx) error {
	var stored *string
	if err := tx.QueryRow(ctx, "SELECT system_identifier FROM fleet.metadata WHERE singleton FOR UPDATE").Scan(&stored); err != nil {
		return err
	}
	// pg_control_system() may not be granted to the runtime role; the counter
	// check alone still detects restores into a younger cluster.
	var current *string
	if sp, err := tx.Begin(ctx); err == nil {
		if err := sp.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&current); err != nil {
			current = nil
			_ = sp.Rollback(ctx)
		} else if err := sp.Commit(ctx); err != nil {
			return err
		}
	} else {
		return err
	}
	var regressed bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM (SELECT xid FROM fleet.events ORDER BY xid DESC, id DESC LIMIT 1) l WHERE l.xid >= pg_current_xact_id())").Scan(&regressed); err != nil {
		return err
	}
	changed := stored != nil && current != nil && *stored != *current
	if regressed || changed {
		if _, err := tx.Exec(ctx, "UPDATE fleet.events SET xid='0'"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE fleet.metadata SET event_epoch=$1, event_floor_xid='0' WHERE singleton", randomID()); err != nil {
			return err
		}
	}
	if current != nil && (stored == nil || changed) {
		if _, err := tx.Exec(ctx, "UPDATE fleet.metadata SET system_identifier=$1 WHERE singleton", *current); err != nil {
			return err
		}
	}
	return nil
}

func identity(ctx context.Context, tx pgx.Tx, serverID string) error {
	_, err := tx.Exec(ctx, "INSERT INTO fleet.metadata(singleton,server_id,event_epoch) VALUES(true,$1,$2) ON CONFLICT(singleton) DO NOTHING", serverID, randomID())
	if err != nil {
		return err
	}
	var id string
	if err = tx.QueryRow(ctx, "SELECT server_id FROM fleet.metadata WHERE singleton").Scan(&id); err != nil {
		return err
	}
	if id != serverID {
		return fleet.ErrConflict
	}
	return nil
}

// Close stops background work without deregistering: a replica that did not
// run StopAgents is reaped by the leader once its heartbeat expires.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		s.loops.Wait()
		s.pool.Close()
	})
}

// ReplicaID identifies this process in fleet.replicas; it changes per start.
func (s *Store) ReplicaID() string { return s.replica }

// Lost closes when this replica can no longer prove liveness. Other replicas
// treat its agents as disconnected by then, so the process must shut down.
func (s *Store) Lost() <-chan struct{} { return s.lost }

func (s *Store) markLost(why string) {
	s.lostOnce.Do(func() {
		s.lostWhy = why
		close(s.lost)
	})
}

// LostReason says why the replica fenced itself; valid once Lost is closed.
// It names an error class, never connection details.
func (s *Store) LostReason() string { return s.lostWhy }

// SetSignals binds cross-replica hints before serving. Both callbacks must
// not block; correctness never depends on their delivery.
func (s *Store) SetSignals(takeover func(id string, generation uint64), revoked func(credential string)) {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	s.onTakeover, s.onRevoked = takeover, revoked
}

// SetConfigSignal binds the config management hint: an empty agent ID
// means desired state changed, otherwise that agent's hold was cleared. It
// also fires with an empty ID after the listener reconnects. The callback
// must not block.
func (s *Store) SetConfigSignal(changed func(agentID string)) {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	s.onConfig = changed
}

// EventSignal closes on the next event notification.
func (s *Store) EventSignal() <-chan struct{} {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	return s.eventWake
}

func (s *Store) wakeEvents() {
	s.signalMu.Lock()
	defer s.signalMu.Unlock()
	close(s.eventWake)
	s.eventWake = make(chan struct{})
}

// write runs one short administrative transaction on the pool. Agent reports
// use the batched writer instead.
func (s *Store) write(ctx context.Context, work func(context.Context, pgx.Tx) error) (result error) {
	defer func() {
		if result != nil {
			s.writeErrors.Add(1)
		}
	}()
	if ctx.Err() != nil || s.closed.Load() {
		return fleet.ErrUnavailable
	}
	started := time.Now()
	defer func() { s.writeDuration.Observe(time.Since(started)) }()
	// Once admitted, finish independently of HTTP cancellation.
	ctx, finish := bounded(context.WithoutCancel(ctx))
	defer finish()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fleet.ErrUnavailable
	}
	defer tx.Rollback(context.Background())
	if err = work(ctx, tx); err != nil {
		return normalize(err)
	}
	return normalize(tx.Commit(ctx))
}

func normalize(err error) error {
	if err == nil {
		return nil
	}
	// A reason names a field and a rule, never stored data.
	var invalid *fleet.InvalidError
	if errors.As(err, &invalid) {
		return invalid
	}
	for _, known := range []error{configmgmt.ErrRolloutActive, configmgmt.ErrRolloutState, configmgmt.ErrTooManyTargets, fleet.ErrUnavailable, fleet.ErrNotFound, fleet.ErrConflict, fleet.ErrInvalid, fleet.ErrCapacity, fleet.ErrStale, fleet.ErrBusy, fleet.ErrPrecondition, fleet.ErrCursorExpired, auth.ErrUnauthorized, auth.ErrForbidden} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fleet.ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505":
			return fleet.ErrConflict
		case "23503", "23514":
			return fleet.ErrInvalid
		}
	}
	// Never return driver errors: they may contain SQL data or connection secrets.
	return fleet.ErrUnavailable
}

// Ready reports whether this replica's own heartbeat is fresh. It never waits
// behind writes or the database.
func (s *Store) Ready(context.Context) error {
	if s.closed.Load() || s.stopped.Load() {
		return fleet.ErrUnavailable
	}
	if time.Since(time.Unix(0, s.heartbeat.Load())) > 2*s.options.Heartbeat+time.Second {
		return fleet.ErrUnavailable
	}
	select {
	case <-s.lost:
		return fleet.ErrUnavailable
	default:
		return nil
	}
}

// event appends to the outbox inside the caller's transaction and hints
// long-polling readers on commit.
func (s *Store) event(ctx context.Context, tx pgx.Tx, kind, subject, actor string, detail []byte) error {
	return appendEvent(ctx, tx, kind, subject, actor, detail)
}

// appendEvent records the request's acting-for value with the event.
func appendEvent(ctx context.Context, tx pgx.Tx, kind, subject, actor string, detail []byte) error {
	var acting []byte
	if a := fleet.ActingFrom(ctx); a != nil {
		var err error
		if acting, err = json.Marshal(a); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, "WITH e AS (INSERT INTO fleet.events(kind,subject_id,actor_id,detail,acting_for) VALUES($1,$2,$3,$4,$5) RETURNING 1) SELECT pg_notify('fleet_events','') FROM e", kind, subject, actor, detail, acting)
	return err
}

type scanner interface{ Scan(...any) error }
