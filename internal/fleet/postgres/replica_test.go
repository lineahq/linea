package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
// It creates a second, fresh database in that fixture.
func TestPostgresReplicas(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_replicas"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_replicas?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "replica-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	options := Options{ServerID: "replica-fleet", MaxAgents: 10, MaxEvents: 1000, Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 100 * time.Millisecond}
	open := func() *Store {
		t.Helper()
		s, err := Open(ctx, url, options)
		if err != nil {
			t.Fatal("open replica:", err)
		}
		t.Cleanup(s.Close)
		return s
	}
	a, b := open(), open()
	takeovers := make(chan string, 16)
	revoked := make(chan string, 16)
	a.SetSignals(func(id string, generation uint64) { takeovers <- id }, func(string) {})
	b.SetSignals(func(string, uint64) {}, func(id string) { revoked <- id })
	credential := func(id string) string {
		t.Helper()
		c, err := auth.Generate(auth.CredentialRequest{ID: id, Name: "replica-agent", Role: "agent", ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := a.InsertCredential(ctx, issued.Credential.ID, c.Credential); err != nil {
			t.Fatal(err)
		}
		return id
	}
	agent := credential("00000000000000000000000000000031")
	start, err := a.Events(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}

	// Cross-replica takeover: the newest connection wins, the old owner is
	// fenced and hinted to close its socket.
	id := strings.Repeat("31", 16)
	first, _, err := a.ConnectAgent(ctx, id, agent, fleet.Report{Capabilities: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := b.ConnectAgent(ctx, id, agent, fleet.Report{Capabilities: 1}, time.Now())
	if err != nil || second != first+1 {
		t.Fatal("takeover generation:", second, err)
	}
	select {
	case got := <-takeovers:
		if got != id {
			t.Fatal("takeover hint for wrong agent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old owner was not hinted")
	}
	if err := a.ObserveAgent(ctx, id, agent, first, fleet.Report{Capabilities: 9}, time.Now()); !errors.Is(err, fleet.ErrStale) {
		t.Fatal("fenced generation published:", err)
	}
	if err := a.DisconnectAgent(ctx, id, first, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetAgent(ctx, id); err != nil || !got.Connected || got.Generation != second || got.Capabilities != 1 {
		t.Fatal("fenced owner changed the agent")
	}

	// Heartbeats stay in memory and are flushed in bulk.
	seen := time.Now().Add(time.Second).Truncate(time.Microsecond)
	b.SeenAgent(id, second, seen)
	waitUntil(t, func() bool {
		got, err := a.GetAgent(ctx, id)
		return err == nil && got.LastSeenAt != nil && got.LastSeenAt.Equal(seen)
	})

	// Repeated takeovers mark an identity conflict once.
	twin := strings.Repeat("32", 16)
	for i := range fleet.ConflictTakeovers + 1 {
		owner := a
		if i%2 == 1 {
			owner = b
		}
		if _, _, err := owner.ConnectAgent(ctx, twin, agent, fleet.Report{}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := a.GetAgent(ctx, twin); err != nil || !got.IdentityConflict {
		t.Fatal("duplicate identity was not flagged")
	}

	// Revocation reaches every replica as a hint.
	other := credential("00000000000000000000000000000032")
	if err := a.RevokeCredential(ctx, issued.Credential.ID, other); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-revoked:
		if got != other {
			t.Fatal("revocation hint for wrong credential")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revocation hint missing")
	}

	// Event readers wake on commits from another replica.
	position, err := a.Events(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	signal := a.EventSignal()
	victim := strings.Repeat("33", 16)
	if _, _, err := b.ConnectAgent(ctx, victim, agent, fleet.Report{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("event hint missing")
	}

	// A crashed replica's agents read as disconnected once its heartbeat
	// expires, and the leader reaps its ownership with events.
	b.Close()
	waitUntil(t, func() bool { got, err := a.GetAgent(ctx, victim); return err == nil && !got.Connected })
	waitUntil(t, func() bool {
		var owned int
		err := a.pool.QueryRow(ctx, "SELECT count(*) FROM fleet.agents WHERE owner_replica=$1", b.ReplicaID()).Scan(&owned)
		return err == nil && owned == 0
	})
	waitUntil(t, func() bool {
		var exists bool
		err := a.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.replicas WHERE id=$1)", b.ReplicaID()).Scan(&exists)
		return err == nil && !exists
	})
	var reaped bool
	waitUntil(t, func() bool {
		page, err := a.Events(ctx, position.NextCursor, 500)
		if err != nil {
			t.Fatal("cursor broke across a crash:", err)
		}
		for _, e := range page.Items {
			reaped = reaped || (e.Kind == "agent.disconnected" && e.SubjectID == victim && e.ActorID == "server")
		}
		return reaped
	})
	if !a.leader.Load() {
		t.Fatal("surviving replica did not take the leader lease")
	}

	// The stream is ordered and complete from the first cursor.
	all, err := a.Events(ctx, start.NextCursor, 500)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range all.Items {
		kinds[e.Kind]++
	}
	if kinds["agent.registered"] != 3 || kinds["agent.identity_conflict"] != 1 || kinds["replica.started"] != 0 || kinds["credential.revoked"] != 1 {
		t.Fatalf("unexpected event stream: %v", kinds)
	}

	// A release whose transaction fails is retried; otherwise the agent would
	// stay owned by this live replica and read as connected.
	r := open()
	generation, _, err := r.ConnectAgent(ctx, victim, agent, fleet.Report{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, "LOCK TABLE fleet.agents IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithTimeout(ctx, time.Second)
	if err := r.DisconnectAgent(gone, victim, generation, time.Now()); err == nil {
		t.Fatal("release committed through the lock")
	}
	cancel()
	// Hold the lock past the pool's lock_timeout, so the release fails.
	time.Sleep(2 * time.Second)
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	_ = blocker.Close(ctx)
	waitUntil(t, func() bool {
		got, err := a.GetAgent(ctx, victim)
		return err == nil && !got.Connected
	})
	// A release that finds the queue full is deferred, not dropped.
	generation, _, err = r.ConnectAgent(ctx, victim, agent, fleet.Report{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.writer.overflowRelease(&op{kind: opDisconnect, id: victim, generation: generation, now: time.Now()})
	waitUntil(t, func() bool {
		got, err := a.GetAgent(ctx, victim)
		return err == nil && !got.Connected
	})
	r.Close()

	// Graceful stop releases ownership at once and deregisters.
	c := open()
	if _, _, err := c.ConnectAgent(ctx, victim, agent, fleet.Report{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.StopAgents(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := a.GetAgent(ctx, victim); err != nil || got.Connected {
		t.Fatal("graceful stop left agent connected")
	}
	if _, _, err := c.ConnectAgent(ctx, victim, agent, fleet.Report{}, time.Now()); !errors.Is(err, fleet.ErrUnavailable) {
		t.Fatal("stopped replica accepted a session")
	}
	c.Close()

	// A restore into another cluster rotates the epoch once and expires
	// cursors; history stays readable from the floor.
	old := all.NextCursor
	if err := a.StopAgents(ctx); err != nil {
		t.Fatal(err)
	}
	a.Close()
	maintenance, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close(ctx)
	if _, err := maintenance.Exec(ctx, "UPDATE fleet.metadata SET system_identifier='1'"); err != nil {
		t.Fatal(err)
	}
	d := open()
	if _, err := d.Events(ctx, old, 10); !errors.Is(err, fleet.ErrCursorExpired) {
		t.Fatal("restored stream accepted an old cursor:", err)
	}
	if page, err := d.Events(ctx, "", 500); err != nil || len(page.Items) == 0 {
		t.Fatal("restored history unreadable:", err)
	}
	// Live and starting replicas both refuse migration.
	if _, err := maintenance.Exec(ctx, "DELETE FROM fleet.schema_version WHERE version=10"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, url); !errors.Is(err, ErrReplicasActive) {
		t.Fatal("migration ran beside a live replica:", err)
	}

	// Positions order numerically, not as text: xid 9 precedes 10 and the
	// live events above them, and a trim moves the floor to the highest
	// deleted position. The restore left history at xid 0.
	var epoch string
	var restored int64
	if err := maintenance.QueryRow(ctx, "SELECT event_epoch, (SELECT max(id) FROM fleet.events WHERE xid='0') FROM fleet.metadata").Scan(&epoch, &restored); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Exec(ctx, `INSERT INTO fleet.events(xid,kind,subject_id,actor_id)
		VALUES('10','test.second','x','server'),('9','test.first','x','server')`); err != nil {
		t.Fatal(err)
	}
	head, err := d.Events(ctx, cursor(epoch, 0, restored), 1)
	if err != nil || len(head.Items) != 1 || head.Items[0].Kind != "test.first" {
		t.Fatal("event positions ordered as text:", head.Items, err)
	}
	if err := d.trimEvents(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Events(ctx, head.NextCursor, 10); !errors.Is(err, fleet.ErrCursorExpired) {
		t.Fatal("trim left the floor below deleted events:", err)
	}
}

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresReapBudget(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_reap"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_reap?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	issued, err := Bootstrap(ctx, url, "reap-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal(err)
	}
	// The leader loop never ticks; the test drives reaping itself.
	s, err := Open(ctx, url, Options{ServerID: "reap-fleet", MaxAgents: 20, MaxEvents: 1000, Heartbeat: time.Hour, Alive: time.Minute, Flush: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	c, err := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000051", Name: "reap-agent", Role: "agent", ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, issued.Credential.ID, c.Credential); err != nil {
		t.Fatal(err)
	}
	dead := strings.Repeat("de", 16)
	crash := func(n int) {
		t.Helper()
		for i := range n {
			id := strings.Repeat(string("0123456789abcdef"[i]), 31) + "5"
			if _, _, err := s.ConnectAgent(ctx, id, c.Credential.ID, fleet.Report{}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO fleet.replicas(id,heartbeat_at) VALUES($1, now() - interval '1 hour') ON CONFLICT(id) DO NOTHING`, dead); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(ctx, "UPDATE fleet.agents SET owner_replica=$1", dead); err != nil {
			t.Fatal(err)
		}
	}
	s.reapBatch = 2

	// Full batches repeat within the budget: five agents in one tick.
	crash(5)
	if n, err := s.reapAll(ctx, time.Minute); err != nil || n != 5 {
		t.Fatalf("reaped %d, err %v", n, err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM fleet.replicas WHERE id=$1)", dead).Scan(&exists); err != nil || exists {
		t.Fatal("dead replica outlived its agents:", err)
	}

	// A spent budget stops after the batch in progress.
	crash(5)
	if n, err := s.reapAll(ctx, 0); err != nil || n != 2 {
		t.Fatalf("spent budget reaped %d, err %v", n, err)
	}

	// An agent of a dead replica that reconnects before the reap is not a
	// takeover: its disconnect is recorded first, with no takeover hint.
	orphan := strings.Repeat("3", 31) + "5"
	var owner string
	if err := s.pool.QueryRow(ctx, "SELECT owner_replica FROM fleet.agents WHERE id=$1", orphan).Scan(&owner); err != nil || owner != dead {
		t.Fatal("orphan is not owned by the dead replica:", owner, err)
	}
	position, err := s.Events(ctx, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	takeovers := s.takeovers.Load()
	if _, _, err := s.ConnectAgent(ctx, orphan, c.Credential.ID, fleet.Report{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(ctx, position.NextCursor, 10)
	if err != nil || len(page.Items) != 2 || page.Items[0].Kind != "agent.disconnected" || page.Items[0].ActorID != "server" ||
		page.Items[1].Kind != "agent.connected" || len(page.Items[1].Detail) != 0 || s.takeovers.Load() != takeovers {
		t.Fatalf("orphan reconnect events = %+v, takeovers %d -> %d", page.Items, takeovers, s.takeovers.Load())
	}
	if got, err := s.GetAgent(ctx, orphan); err != nil || !got.Connected || got.Generation != 3 {
		t.Fatalf("orphan reconnect = %+v, err %v", got, err)
	}
}
