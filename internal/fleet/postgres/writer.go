package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Writer bounds: a batch commits after maxBatch items or batchWait, and a
// queue deeper than the expected-wait budget refuses new sessions.
const (
	queueSize   = 4096
	maxBatch    = 200
	batchWait   = 10 * time.Millisecond
	waitBudget  = 500 * time.Millisecond
	batchBudget = 5 * time.Second
	retryWait   = time.Second
	retryMax    = 10 * time.Second
)

type opKind uint8

const (
	opConnect opKind = iota
	opObserve
	opDisconnect
)

// op states: pending until the writer delivers it or the caller abandons it.
const (
	opPending int32 = iota
	opDelivered
	opAbandoned
)

type op struct {
	kind       opKind
	id         string
	credential string
	generation uint64
	report     fleet.Report
	now        time.Time
	seen       *time.Time
	state      atomic.Int32
	done       chan opResult
}

type opResult struct {
	generation uint64
	report     fleet.Report // stored state after a connect
	err        error
}

type writer struct {
	s     *Store
	queue chan *op
	carry []*op
	// retry holds releases whose transaction failed. A dropped disconnect
	// would leave the agent owned by this live replica, so it is retried
	// until it commits or the replica is fenced.
	retry   map[string]*op
	retryAt time.Time
	// overflow holds releases that found the queue full, one per agent, so
	// it is bounded by this replica's sessions. run merges it into retry.
	overflowMu sync.Mutex
	overflow   map[string]*op
	wake       chan struct{}
	backoff    time.Duration
	inflight   atomic.Int64
	lastBatch  atomic.Int64
}

func newWriter(s *Store) *writer {
	return &writer{s: s, queue: make(chan *op, queueSize), retry: map[string]*op{}, overflow: map[string]*op{}, wake: make(chan struct{}, 1)}
}

// submit enqueues without blocking and waits for the commit. A caller that
// gives up abandons the op; an abandoned connect is released by the writer.
func (w *writer) submit(ctx context.Context, o *op) opResult {
	if w.s.closed.Load() || w.s.stopped.Load() {
		return opResult{err: fleet.ErrUnavailable}
	}
	select {
	case <-w.s.lost:
		// Another replica may already own these agents.
		return opResult{err: fleet.ErrUnavailable}
	default:
	}
	o.done = make(chan opResult, 1)
	w.inflight.Add(1)
	select {
	case w.queue <- o:
	default:
		w.inflight.Add(-1)
		if o.kind == opDisconnect {
			w.overflowRelease(o)
			return opResult{}
		}
		return opResult{err: fleet.ErrBusy}
	}
	select {
	case r := <-o.done:
		return r
	case <-ctx.Done():
		if o.state.CompareAndSwap(opPending, opAbandoned) {
			return opResult{err: fleet.ErrUnavailable}
		}
		return <-o.done
	}
}

// Busy estimates whether new sessions would wait longer than the budget.
// overflowRelease defers a release instead of dropping it: a lost release
// would leave the agent owned by this live replica.
func (w *writer) overflowRelease(o *op) {
	w.overflowMu.Lock()
	if prev, ok := w.overflow[o.id]; !ok || prev.generation <= o.generation {
		w.overflow[o.id] = &op{kind: opDisconnect, id: o.id, generation: o.generation, now: o.now, seen: o.seen}
	}
	w.overflowMu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *writer) Busy() bool {
	depth := len(w.queue)
	if depth > queueSize*3/4 {
		return true
	}
	expected := time.Duration(w.lastBatch.Load()) * time.Duration(depth/maxBatch+1)
	return depth > 0 && expected > waitBudget
}

// drain waits until every submitted op was delivered.
func (w *writer) drain(ctx context.Context) {
	for w.inflight.Load() > 0 && ctx.Err() == nil && w.s.ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
}

func (w *writer) run() {
	defer w.s.loops.Done()
	for {
		batch := w.carry
		w.carry = nil
		if len(batch) == 0 {
			due := time.NewTimer(time.Until(w.retryAt))
			if len(w.retry) == 0 {
				due.Stop()
			}
			select {
			case o := <-w.queue:
				batch = append(batch, o)
			case <-due.C:
			case <-w.wake:
			case <-w.s.ctx.Done():
				due.Stop()
				w.fail(nil)
				return
			}
			due.Stop()
		}
		w.overflowMu.Lock()
		for _, o := range w.overflow {
			w.keep(o)
		}
		clear(w.overflow)
		w.overflowMu.Unlock()
		batch = w.takeRetries(batch)
		if len(batch) == 0 {
			continue
		}
		timer := time.NewTimer(batchWait)
	collect:
		for len(batch) < maxBatch {
			select {
			case o := <-w.queue:
				batch = append(batch, o)
			case <-timer.C:
				break collect
			case <-w.s.ctx.Done():
				break collect
			}
		}
		timer.Stop()
		if w.s.ctx.Err() != nil {
			w.fail(batch)
			return
		}
		w.execute(batch)
	}
}

// takeRetries adds due releases to the batch, at most a batch at a time. A
// fenced replica drops them: the reaper releases its agents.
func (w *writer) takeRetries(batch []*op) []*op {
	if len(w.retry) == 0 || time.Now().Before(w.retryAt) {
		return batch
	}
	select {
	case <-w.s.lost:
		clear(w.retry)
		return batch
	default:
	}
	for id, o := range w.retry {
		if len(batch) >= maxBatch {
			break
		}
		batch = append(batch, o)
		delete(w.retry, id)
	}
	return batch
}

// keep schedules a failed release for another attempt. Releases are
// idempotent: the generation and owner checks skip a newer session.
func (w *writer) keep(o *op) {
	if prev, ok := w.retry[o.id]; ok && prev.generation > o.generation {
		return
	}
	w.retry[o.id] = &op{kind: opDisconnect, id: o.id, generation: o.generation, now: o.now, seen: o.seen}
}

// fail rejects a batch and everything still queued at shutdown.
func (w *writer) fail(batch []*op) {
	for {
		select {
		case o := <-w.queue:
			batch = append(batch, o)
			continue
		default:
		}
		break
	}
	for _, o := range batch {
		w.deliver(o, opResult{err: fleet.ErrUnavailable})
	}
}

func (w *writer) deliver(o *op, r opResult) {
	if o.done == nil {
		return // internal follow-up without a waiting caller
	}
	defer w.inflight.Add(-1)
	if o.state.CompareAndSwap(opPending, opDelivered) {
		o.done <- r
		return
	}
	// The caller gave up. Release ownership it can no longer use.
	if o.kind == opConnect && r.err == nil {
		w.carry = append(w.carry, &op{kind: opDisconnect, id: o.id, generation: r.generation, now: time.Now()})
	}
}

// batchState caches per-transaction lookups.
type batchState struct {
	credentials map[string]error
	agents      int
	counted     bool
	events      bool
}

// execute writes one transaction in UID order so concurrent replicas never
// deadlock. Each op runs in a savepoint; a failing op does not abort others.
func (w *writer) execute(batch []*op) {
	started := time.Now()
	defer func() {
		elapsed := time.Since(started)
		w.lastBatch.Store(int64(elapsed))
		w.s.writeDuration.Observe(elapsed)
		w.s.batches.Add(1)
		w.s.batchItems.Add(uint64(len(batch)))
	}()
	slices.SortStableFunc(batch, func(a, b *op) int { return strings.Compare(a.id, b.id) })
	results := make([]opResult, len(batch))
	ctx, cancel := context.WithTimeout(context.Background(), batchBudget)
	defer cancel()
	commit := func() error {
		tx, err := w.s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		state := &batchState{credentials: map[string]error{}}
		for i, o := range batch {
			sp, err := tx.Begin(ctx)
			if err != nil {
				return err
			}
			r := w.apply(ctx, sp, o, state)
			if r.err != nil {
				if err := sp.Rollback(ctx); err != nil {
					return err
				}
			} else if err := sp.Commit(ctx); err != nil {
				return err
			}
			results[i] = r
		}
		if state.events {
			if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_events','')"); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}
	if err := commit(); err != nil {
		w.s.writeErrors.Add(1)
		for i := range results {
			results[i] = opResult{err: fleet.ErrUnavailable}
		}
	}
	failed := false
	for i, o := range batch {
		// A release fails only on database errors (timeouts, a lost
		// connection), whether the whole batch or just its savepoint failed.
		if o.kind == opDisconnect && results[i].err != nil {
			w.keep(o)
			failed = true
		}
		results[i].err = normalize(results[i].err)
		w.deliver(o, results[i])
	}
	if failed {
		w.backoff = min(max(2*w.backoff, retryWait), retryMax)
		w.retryAt = time.Now().Add(w.backoff)
	} else {
		w.backoff = 0
	}
}

func (w *writer) apply(ctx context.Context, tx pgx.Tx, o *op, state *batchState) opResult {
	switch o.kind {
	case opConnect:
		if err := w.credential(ctx, tx, o.credential, state); err != nil {
			return opResult{err: err}
		}
		generation, report, err := w.connect(ctx, tx, o, state)
		return opResult{generation: generation, report: report, err: err}
	case opObserve:
		if err := w.credential(ctx, tx, o.credential, state); err != nil {
			return opResult{err: err}
		}
		return opResult{err: w.observe(ctx, tx, o, state)}
	default:
		return opResult{err: w.disconnect(ctx, tx, o, state)}
	}
}

func (w *writer) credential(ctx context.Context, tx pgx.Tx, id string, state *batchState) error {
	if err, ok := state.credentials[id]; ok {
		return err
	}
	err := requireAgent(ctx, tx, id)
	if err == nil || errors.Is(err, auth.ErrUnauthorized) {
		state.credentials[id] = err
	}
	return err
}

// row holds the SQL parameters of one report. Nil values keep the stored
// section: an agent that never reported it keeps the column default.
type row struct {
	caps       string
	healthy    *bool
	status     *string
	error      *string
	attributes *string
	components *string
	unhealthy  int
	hash       *string
	available  *string
	// remote config status; rcSet false keeps the stored columns
	rcSet                   bool
	rcHash, rcStatus, rcErr *string
}

func reportRow(r fleet.Report) row {
	out := row{caps: strconv.FormatUint(r.Capabilities, 10), healthy: r.Healthy, unhealthy: fleet.Unhealthy(r.Components)}
	if r.Healthy != nil && r.Status != nil {
		out.status, out.error = nonEmpty(r.Status.Status), nonEmpty(r.Status.Error)
	}
	if r.Attributes != nil {
		b, _ := json.Marshal(r.Attributes)
		v := string(b)
		out.attributes = &v
	}
	if r.Components != nil {
		b, _ := json.Marshal(r.Components)
		v := string(b)
		out.components = &v
	}
	out.hash, out.available = nonEmpty(r.ConfigHash), nonEmpty(r.AvailableComponents)
	if rc := r.RemoteConfig; rc != nil {
		out.rcSet = true
		out.rcHash, out.rcStatus, out.rcErr = nonEmpty(rc.Hash), nonEmpty(rc.Status), nonEmpty(rc.Error)
	}
	return out
}

func nonEmpty(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// connect returns the stored report, so the session cache starts from what
// the database holds rather than from a reconnect's empty first message.
func (w *writer) connect(ctx context.Context, tx pgx.Tx, o *op, state *batchState) (uint64, fleet.Report, error) {
	s := w.s
	r := reportRow(o.report)
	for attempt := 0; attempt < 2; attempt++ {
		var owner *string
		var generation int64
		var takeovers int
		var takeoverAt *time.Time
		var conflict bool
		// orphaned: the owner replica is dead, so its session ended with the
		// replica, not because of this connection.
		var orphaned bool
		err := tx.QueryRow(ctx, `SELECT a.owner_replica,a.generation,a.takeover_count,a.takeover_at,a.identity_conflict,
				a.owner_replica IS NOT NULL AND (o.id IS NULL OR o.heartbeat_at < now() - $2 * interval '1 millisecond')
			FROM fleet.agents a LEFT JOIN fleet.replicas o ON o.id=a.owner_replica WHERE a.id=$1 FOR UPDATE OF a`,
			o.id, s.aliveMillis()).Scan(&owner, &generation, &takeovers, &takeoverAt, &conflict, &orphaned)
		if errors.Is(err, pgx.ErrNoRows) {
			if !state.counted {
				if err := tx.QueryRow(ctx, "SELECT count(*) FROM fleet.agents").Scan(&state.agents); err != nil {
					return 0, fleet.Report{}, err
				}
				state.counted = true
			}
			if state.agents >= s.options.MaxAgents {
				// Make room by evicting the agent disconnected longest.
				evicted, err := expire(ctx, tx, nil, 1, "capacity")
				if err != nil {
					return 0, fleet.Report{}, err
				}
				if evicted == 0 {
					return 0, fleet.Report{}, fleet.ErrCapacity
				}
				s.expired.Add(1)
				state.agents--
			}
			tag, err := tx.Exec(ctx, `INSERT INTO fleet.agents(id,generation,owner_replica,first_seen_at,connected_at,last_seen_at,credential_id,capabilities,healthy,health_observed_at,
					health_status,health_error,attributes,components,components_unhealthy,config_hash,
					remote_config_hash,remote_config_status,remote_config_error,remote_config_at,available_components_hash)
				VALUES($1,1,$2,$3::timestamptz,$3,$3,$4,$5::numeric,$6::boolean,CASE WHEN $6::boolean IS NULL THEN NULL ELSE $3::timestamptz END,
					$7::text,$8::text,COALESCE($9::jsonb,'{}'),$10::jsonb,$11::integer,$12::text,
					$14::text,$15::text,$16::text,CASE WHEN $13::boolean THEN $3::timestamptz END,$17::text) ON CONFLICT(id) DO NOTHING`,
				o.id, s.replica, o.now, o.credential, r.caps, r.healthy, r.status, r.error, r.attributes, r.components, r.unhealthy, r.hash,
				r.rcSet, r.rcHash, r.rcStatus, r.rcErr, r.available)
			if err != nil {
				return 0, fleet.Report{}, err
			}
			if tag.RowsAffected() == 0 {
				continue // registered concurrently by another replica; take it over
			}
			state.agents++
			state.events = true
			if err := s.event(ctx, tx, "agent.registered", o.id, o.credential, nil); err != nil {
				return 0, fleet.Report{}, err
			}
			stored := o.report
			if stored.Attributes == nil {
				stored.Attributes = map[string]string{}
			}
			return 1, stored, s.event(ctx, tx, "agent.connected", o.id, o.credential, nil)
		}
		if err != nil {
			return 0, fleet.Report{}, err
		}
		stored := fleet.Report{Capabilities: o.report.Capabilities}
		var status, healthError, hash *string
		var rcHash, rcStatus, rcError, rcHeld, available *string
		var attributes, components []byte
		if orphaned {
			// Record the disconnect the leader would reap, so the stream
			// stays paired and a replica crash is not counted as takeovers.
			state.events = true
			if err := s.event(ctx, tx, "agent.disconnected", o.id, "server", nil); err != nil {
				return 0, fleet.Report{}, err
			}
			owner = nil
		}
		takeover := owner != nil
		if takeover {
			if takeoverAt != nil && o.now.Sub(*takeoverAt) < fleet.ConflictWindow {
				takeovers++
			} else {
				takeovers, takeoverAt = 1, &o.now
			}
		}
		flagged := !conflict && takeovers >= fleet.ConflictTakeovers
		err = tx.QueryRow(ctx, `UPDATE fleet.agents SET generation=generation+1,owner_replica=$2,connected_at=$3::timestamptz,last_seen_at=$3,disconnected_at=NULL,
				credential_id=$4,capabilities=$5::numeric,healthy=COALESCE($6::boolean,healthy),
				health_observed_at=CASE WHEN $6::boolean IS NULL OR (healthy,health_status,health_error) IS NOT DISTINCT FROM ($6::boolean,$10::text,$11::text)
					THEN health_observed_at ELSE $3::timestamptz END,
				takeover_count=$7,takeover_at=$8::timestamptz,identity_conflict=identity_conflict OR $9::boolean,
				`+inventorySet("", 6, 10)+`,`+remoteConfigSet("", 16, 3)+`,available_components_hash=COALESCE($20::text,available_components_hash)
			WHERE id=$1 RETURNING generation,healthy,health_status,health_error,attributes,components,config_hash,
				remote_config_hash,remote_config_status,remote_config_error,config_hold_hash,available_components_hash`,
			o.id, s.replica, o.now, o.credential, r.caps, r.healthy, takeovers, takeoverAt, flagged,
			r.status, r.error, r.attributes, r.components, r.unhealthy, r.hash,
			r.rcSet, r.rcHash, r.rcStatus, r.rcErr, r.available).Scan(&generation, &stored.Healthy, &status, &healthError, &attributes, &components, &hash,
			&rcHash, &rcStatus, &rcError, &rcHeld, &available)
		if err != nil {
			return 0, fleet.Report{}, err
		}
		state.events = true
		var detail []byte
		if takeover {
			detail = []byte(`{"takeover":true}`)
			s.takeovers.Add(1)
			// The previous owner, possibly this replica, closes its socket.
			if _, err := tx.Exec(ctx, "SELECT pg_notify('fleet_agent_takeover', $1)", o.id+":"+strconv.FormatInt(generation, 10)); err != nil {
				return 0, fleet.Report{}, err
			}
		}
		if err := s.event(ctx, tx, "agent.connected", o.id, o.credential, detail); err != nil {
			return 0, fleet.Report{}, err
		}
		if flagged {
			if err := s.event(ctx, tx, "agent.identity_conflict", o.id, "server", nil); err != nil {
				return 0, fleet.Report{}, err
			}
		}
		if stored.Healthy != nil {
			stored.Status = &fleet.Status{Status: deref(status), Error: deref(healthError)}
		}
		stored.ConfigHash, stored.AvailableComponents = deref(hash), deref(available)
		stored.RemoteConfig = remoteConfig(rcHash, rcStatus, rcError, rcHeld)
		if err := json.Unmarshal(attributes, &stored.Attributes); err != nil {
			return 0, fleet.Report{}, err
		}
		if components != nil {
			if err := json.Unmarshal(components, &stored.Components); err != nil {
				return 0, fleet.Report{}, err
			}
		}
		return uint64(generation), stored, nil
	}
	return 0, fleet.Report{}, fleet.ErrConflict
}

func (w *writer) observe(ctx context.Context, tx pgx.Tx, o *op, state *batchState) error {
	s := w.s
	r := reportRow(o.report)
	var caps, health, attributes, components, config bool
	err := tx.QueryRow(ctx, `UPDATE fleet.agents a SET capabilities=$4::numeric,healthy=COALESCE($5::boolean,a.healthy),
			health_observed_at=CASE WHEN $5::boolean IS NULL OR (a.healthy,a.health_status,a.health_error) IS NOT DISTINCT FROM ($5::boolean,$7::text,$8::text)
				THEN a.health_observed_at ELSE $6::timestamptz END,last_seen_at=$6::timestamptz,
			`+inventorySet("a.", 5, 7)+`,`+remoteConfigSet("a.", 13, 6)+`,available_components_hash=COALESCE($17::text,a.available_components_hash)
		FROM (SELECT id,capabilities,healthy,health_status,health_error,attributes,components,config_hash,available_components_hash,
				remote_config_hash,remote_config_status,remote_config_error FROM fleet.agents WHERE id=$1 FOR UPDATE) old
		WHERE a.id=old.id AND a.generation=$2 AND a.owner_replica=$3
		RETURNING old.capabilities IS DISTINCT FROM a.capabilities,
			(old.healthy, old.health_status, old.health_error) IS DISTINCT FROM (a.healthy, a.health_status, a.health_error),
			old.attributes IS DISTINCT FROM a.attributes,
			(old.components, old.config_hash, old.available_components_hash) IS DISTINCT FROM (a.components, a.config_hash, a.available_components_hash),
			(old.remote_config_hash, old.remote_config_status, old.remote_config_error)
				IS DISTINCT FROM (a.remote_config_hash, a.remote_config_status, a.remote_config_error)`,
		o.id, int64(o.generation), s.replica, r.caps, r.healthy, o.now,
		r.status, r.error, r.attributes, r.components, r.unhealthy, r.hash,
		r.rcSet, r.rcHash, r.rcStatus, r.rcErr, r.available).Scan(&caps, &health, &attributes, &components, &config)
	if errors.Is(err, pgx.ErrNoRows) {
		return fleet.ErrStale
	}
	if err != nil {
		return err
	}
	changed := []string{}
	for _, c := range []struct {
		name string
		ok   bool
	}{{"capabilities", caps}, {"health", health}, {"attributes", attributes}, {"components", components}, {"config", config}} {
		if c.ok {
			changed = append(changed, c.name)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	detail, _ := json.Marshal(map[string][]string{"changed": changed})
	state.events = true
	return s.event(ctx, tx, "agent.changed", o.id, o.credential, detail)
}

// inventorySet assigns the inventory sections from six parameters starting at
// first, after the healthy flag parameter: status, error, attributes, components, unhealthy count and hash.
// Health text follows the healthy flag; nil sections keep stored values.
func inventorySet(alias string, healthy, first int) string {
	p := func(i int) string { return "$" + strconv.Itoa(first+i) }
	return "health_status=CASE WHEN " + "$" + strconv.Itoa(healthy) + "::boolean IS NULL THEN " + alias + "health_status ELSE " + p(0) + "::text END," +
		"health_error=CASE WHEN " + "$" + strconv.Itoa(healthy) + "::boolean IS NULL THEN " + alias + "health_error ELSE " + p(1) + "::text END," +
		"attributes=COALESCE(" + p(2) + "::jsonb," + alias + "attributes)," +
		"components=COALESCE(" + p(3) + "::jsonb," + alias + "components)," +
		"components_unhealthy=CASE WHEN " + p(3) + "::jsonb IS NULL THEN " + alias + "components_unhealthy ELSE " + p(4) + "::integer END," +
		"config_hash=COALESCE(" + p(5) + "::text," + alias + "config_hash)"
}

// remoteConfigSet assigns the remote config status from four parameters
// starting at first: whether it was reported, hash, status and error. The
// change time moves only when the reported status changes. The failure hold
// is written only by SetConfigHold and ClearHold.
func remoteConfigSet(alias string, first, now int) string {
	p := func(i int) string { return "$" + strconv.Itoa(first+i) }
	set := p(0) + "::boolean"
	column := func(name string, i int) string {
		return name + "=CASE WHEN " + set + " THEN " + p(i) + "::text ELSE " + alias + name + " END"
	}
	return "remote_config_at=CASE WHEN " + set + " AND (" + alias + "remote_config_hash," + alias + "remote_config_status," + alias + "remote_config_error) IS DISTINCT FROM (" +
		p(1) + "::text," + p(2) + "::text," + p(3) + "::text) THEN $" + strconv.Itoa(now) + "::timestamptz ELSE " + alias + "remote_config_at END," +
		column("remote_config_hash", 1) + "," + column("remote_config_status", 2) + "," + column("remote_config_error", 3)
}

func (w *writer) disconnect(ctx context.Context, tx pgx.Tx, o *op, state *batchState) error {
	s := w.s
	tag, err := tx.Exec(ctx, `UPDATE fleet.agents SET owner_replica=NULL,disconnected_at=$3::timestamptz,last_seen_at=GREATEST(last_seen_at,$4::timestamptz)
		WHERE id=$1 AND generation=$2 AND owner_replica=$5`, o.id, int64(o.generation), o.now, o.seen, s.replica)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	state.events = true
	return s.event(ctx, tx, "agent.disconnected", o.id, "server", nil)
}

// ConnectAgent takes ownership for this replica. The newest connection wins:
// an older generation anywhere is fenced and its owner is hinted to close it.
func (s *Store) ConnectAgent(ctx context.Context, id, credential string, report fleet.Report, now time.Time) (uint64, fleet.Report, error) {
	r := s.writer.submit(ctx, &op{kind: opConnect, id: id, credential: credential, report: report, now: now})
	return r.generation, r.report, r.err
}

// ObserveAgent commits a changed report. Unchanged heartbeats use SeenAgent.
func (s *Store) ObserveAgent(ctx context.Context, id, credential string, generation uint64, report fleet.Report, now time.Time) error {
	return s.writer.submit(ctx, &op{kind: opObserve, id: id, credential: credential, generation: generation, report: report, now: now}).err
}

func (s *Store) DisconnectAgent(ctx context.Context, id string, generation uint64, now time.Time) error {
	seen := s.takeSeen(id, generation)
	return s.writer.submit(ctx, &op{kind: opDisconnect, id: id, generation: generation, now: now, seen: seen}).err
}

// Busy reports writer backpressure so the gateway can refuse new sessions.
func (s *Store) Busy() bool { return s.writer.Busy() }
