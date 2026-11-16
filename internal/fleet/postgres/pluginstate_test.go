package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/fleettest"
	"github.com/lineahq/linea/pluginsdk"
)

const pluginInstanceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// pluginFixture is a migrated database with an administrator and a reader.
type pluginFixture struct {
	url    string
	store  *Store
	admin  string
	reader string
}

// newPluginFixture creates its own database on the disposable server given by
// scripts/fleet/test-postgres.ps1.
func newPluginFixture(t *testing.T, name string) pluginFixture {
	t.Helper()
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/"+name+"?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migrate:", err)
	}
	issued, err := Bootstrap(ctx, url, name, "bootstrap-admin", nil)
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: name, MaxAgents: 10, MaxEvents: 1000,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 50 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(s.Close)
	reader, err := (&auth.Service{Repository: s}).Issue(ctx, issued.Credential.ID, auth.CredentialRequest{ID: strings.Repeat("11", 16), Name: "reader", Role: "reader"})
	if err != nil {
		t.Fatal("issue reader:", err)
	}
	return pluginFixture{url: url, store: s, admin: issued.Credential.ID, reader: reader.Credential.ID}
}

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresPluginStateContract(t *testing.T) {
	f := newPluginFixture(t, "fleet_pluginstate")
	fleettest.RunPluginState(t, func(t *testing.T) fleettest.PluginSubject {
		ctx := context.Background()
		if _, err := f.store.pool.Exec(ctx, "DELETE FROM fleet.plugin_lifecycle"); err != nil {
			t.Fatal(err)
		}
		return fleettest.PluginSubject{
			Store: f.store, Admin: f.admin, Reader: f.reader,
			Age: func(d time.Duration) {
				if _, err := f.store.pool.Exec(ctx, "UPDATE fleet.plugin_lifecycle SET seen_at = seen_at - make_interval(secs => $1::float8) WHERE seen_at IS NOT NULL", d.Seconds()); err != nil {
					t.Fatal(err)
				}
			},
		}
	})
}

// Every successful change sends the plugin ID on fleet_plugin from its own
// transaction; a probe that writes nothing sends nothing.
func TestPostgresPluginStateNotifies(t *testing.T) {
	f := newPluginFixture(t, "fleet_pluginnotify")
	ctx := context.Background()
	listener, err := pgx.Connect(ctx, f.url)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(ctx)
	if _, err := listener.Exec(ctx, "LISTEN "+pluginChannel); err != nil {
		t.Fatal(err)
	}
	expect := func(step, want string) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		got, err := listener.WaitForNotification(wait)
		if want == "" {
			if err == nil {
				t.Fatalf("%s: unexpected notification %q", step, got.Payload)
			}
			return
		}
		if err != nil || got.Channel != pluginChannel || got.Payload != want {
			t.Fatalf("%s: notification %+v %v", step, got, err)
		}
	}
	quiet := func(step string) {
		t.Helper()
		wait, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		if got, err := listener.WaitForNotification(wait); err == nil {
			t.Fatalf("%s: unexpected notification %q", step, got.Payload)
		}
	}
	first, err := f.store.SeenPlugin(ctx, "demo", "", pluginInstanceA)
	if err != nil {
		t.Fatal(err)
	}
	expect("insert", "demo")
	if _, err := f.store.SeenPlugin(ctx, "demo", first.Epoch, pluginInstanceA); err != nil {
		t.Fatal(err)
	}
	quiet("a probe inside 500 ms")
	if _, err := f.store.SeenPlugin(ctx, "demo", "stale", pluginInstanceA); err != nil {
		t.Fatal(err)
	}
	quiet("a stale probe")
	_, err = f.store.FencePlugin(ctx, "demo", first.Epoch, false)
	if err != nil {
		t.Fatal(err)
	}
	expect("fence", "demo")
	if _, err := f.store.FencePlugin(ctx, "demo", first.Epoch, false); err != nil {
		t.Fatal(err)
	}
	quiet("a stale fence")
	if _, err := f.store.SetPluginLifecycle(ctx, f.admin, fleet.PluginLifecycle{ID: "demo", Desired: "draining"}, 0); err != nil {
		t.Fatal(err)
	}
	expect("lifecycle", "demo")
	if _, err := f.store.SetPluginLifecycle(ctx, f.admin, fleet.PluginLifecycle{ID: "demo", Desired: "disabled"}, 0); !errors.Is(err, fleet.ErrPrecondition) {
		t.Fatal(err)
	}
	quiet("a refused lifecycle change")
}

// A probe, a fence and lifecycle changes race on one row. Whatever the order,
// lifecycle revisions stay gap free, no caller sees its revision go back, and
// a prober holding an epoch from before a change never overwrites it.
func TestPostgresPluginStateConcurrentProbeAndLifecycle(t *testing.T) {
	f := newPluginFixture(t, "fleet_pluginrace")
	ctx := context.Background()
	if _, err := f.store.SeenPlugin(ctx, "demo", "", pluginInstanceA); err != nil {
		t.Fatal(err)
	}
	const setters, changes = 2, 10
	var done atomic.Int32
	var wg, probers sync.WaitGroup
	errs := make(chan error, 64)
	fail := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	for i := range setters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			desired := []string{"draining", "enabled"}
			for n := 0; n < changes; {
				items, err := f.store.PluginLifecycles(ctx)
				if err != nil || len(items) != 1 {
					fail(errors.New("lifecycle read failed"))
					return
				}
				_, err = f.store.SetPluginLifecycle(ctx, f.admin, fleet.PluginLifecycle{ID: "demo", Desired: desired[(i+n)%2]}, items[0].Revision)
				switch {
				case err == nil:
					n++
				case errors.Is(err, fleet.ErrPrecondition):
				default:
					fail(err)
					return
				}
			}
		}()
	}
	for _, instance := range []string{pluginInstanceA, pluginInstanceA, strings.Repeat("b", 32)} {
		probers.Add(1)
		go func() {
			defer probers.Done()
			epoch := ""
			var revision int64
			for done.Load() == 0 {
				got, err := f.store.SeenPlugin(ctx, "demo", epoch, instance)
				if err != nil {
					fail(err)
					return
				}
				if got.Revision < revision || !pluginsdk.ValidUID(got.Epoch) {
					fail(errors.New("a probe saw an older revision or a bad epoch"))
					return
				}
				epoch, revision = got.Epoch, got.Revision
			}
		}()
	}
	// One fencer that refuses and reports unreachability.
	probers.Add(1)
	go func() {
		defer probers.Done()
		epoch := ""
		for n := 0; done.Load() == 0; n++ {
			got, err := f.store.FencePlugin(ctx, "demo", epoch, n%2 == 0)
			if err != nil {
				fail(err)
				return
			}
			epoch = got.Epoch
		}
	}()
	wg.Wait()
	done.Store(1)
	probers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	items, err := f.store.PluginLifecycles(ctx)
	if err != nil || len(items) != 1 || items[0].Revision != setters*changes {
		t.Fatalf("revisions are not gap free: %+v %v", items, err)
	}
	// After the last change, an epoch read before it is stale for every
	// transition, and the change leaves the plugin unseen.
	before, err := f.store.PluginStates(ctx)
	if err != nil || len(before) != 1 {
		t.Fatal(err)
	}
	if _, err := f.store.SetPluginLifecycle(ctx, f.admin, fleet.PluginLifecycle{ID: "demo", Desired: "enabled"}, items[0].Revision); err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() (fleet.PluginState, error){
		func() (fleet.PluginState, error) {
			return f.store.SeenPlugin(ctx, "demo", before[0].Epoch, before[0].InstanceID)
		},
		func() (fleet.PluginState, error) { return f.store.FencePlugin(ctx, "demo", before[0].Epoch, false) },
	} {
		got, err := step()
		if err != nil || got.Epoch == before[0].Epoch || got.Seen() || got.Revision != setters*changes+1 {
			t.Fatalf("stale transition after a lifecycle change: %+v %v", got, err)
		}
	}
}

// Migration 013 gives each existing row its own epoch, no instance and no
// seen time, and admits revision 0 and nothing that breaks the row's shape.
func TestPostgresPluginStateMigration(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_pluginmigrate"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_pluginmigrate?", 1)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	// Install the schema as release 0.2.x left it: migrations 1 to 12.
	for i, source := range migrations[:12] {
		if _, err := conn.Exec(ctx, source); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
		sum := sha256.Sum256([]byte(source))
		if _, err := conn.Exec(ctx, "INSERT INTO fleet.schema_version(version,checksum) VALUES($1,$2)", i+1, hex.EncodeToString(sum[:])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, "INSERT INTO fleet.plugin_lifecycle(id,desired,revision) VALUES('one','draining',2),('two','disabled',1)"); err != nil {
		t.Fatal("seed rows:", err)
	}
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migrate:", err)
	}
	rows, err := conn.Query(ctx, "SELECT id,desired,revision,epoch,instance_id,seen_at FROM fleet.plugin_lifecycle ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	epochs := map[string]bool{}
	for rows.Next() {
		var id, desired, epoch string
		var revision int64
		var instance *string
		var seen *time.Time
		if err := rows.Scan(&id, &desired, &revision, &epoch, &instance, &seen); err != nil {
			t.Fatal(err)
		}
		if !pluginsdk.ValidUID(epoch) || instance != nil || seen != nil || revision == 0 {
			t.Fatalf("upgraded row %s: %s %d %q %v %v", id, desired, revision, epoch, instance, seen)
		}
		epochs[epoch] = true
	}
	rows.Close()
	if len(epochs) != 2 {
		t.Fatal("upgraded rows share an epoch")
	}
	for name, statement := range map[string]string{
		"revision 0 is a probe-created row":     "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch,instance_id,seen_at) VALUES('probe','enabled',0,repeat('a',32),repeat('b',32),now())",
		"negative revision":                     "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch) VALUES('negative','enabled',-1,repeat('a',32))",
		"short epoch":                           "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch) VALUES('short','enabled',1,'abc')",
		"uppercase instance":                    "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch,instance_id) VALUES('upper','enabled',1,repeat('a',32),repeat('B',32))",
		"seen without an instance":              "INSERT INTO fleet.plugin_lifecycle(id,desired,revision,epoch,seen_at) VALUES('ghost','enabled',1,repeat('a',32),now())",
		"missing epoch (no default any longer)": "INSERT INTO fleet.plugin_lifecycle(id,desired,revision) VALUES('bare','enabled',1)",
	} {
		_, err := conn.Exec(ctx, statement)
		if (name == "revision 0 is a probe-created row") != (err == nil) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
