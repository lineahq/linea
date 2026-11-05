package livecapture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/pluginsdk"
)

// This test accepts only the explicit disposable-fixture URL from its script.
func TestPostgresReservationRecoveryAndGrants(t *testing.T) {
	url := os.Getenv("OTEL_CAPTURE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("use the plugin's disposable PostgreSQL script")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal("fixture database connection failed")
	}
	defer admin.Close(context.Background())
	var database string
	if admin.QueryRow(ctx, "SELECT current_database()").Scan(&database) != nil || database != "capture_fixture" {
		t.Fatal("refusing a non-fixture database")
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	password := hex.EncodeToString(secret)
	if _, err = admin.Exec(ctx, "CREATE ROLE capture_fixture_app LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal("fixture role setup failed")
	}
	// Start from a 0.3 (schema 1) database so the upgrade path is exercised.
	if _, err = admin.Exec(ctx, migrations[0]); err != nil {
		t.Fatal("schema 1 fixture failed")
	}
	if _, err = admin.Exec(ctx, "INSERT INTO capture.schema_version(version,checksum) VALUES(1,$1)", checksum(migrations[0])); err != nil {
		t.Fatal("schema 1 ledger fixture failed")
	}
	if err = Migrate(ctx, url, "capture_fixture_app"); err != nil {
		t.Fatal("plugin migration failed", err)
	}
	if err = Migrate(ctx, url, "capture_fixture_app"); err != nil {
		t.Fatal("repeat migration failed", err)
	}
	var versions int
	var revocations bool
	if admin.QueryRow(ctx, "SELECT count(*),to_regclass('capture.revoked_deployments') IS NOT NULL FROM capture.schema_version").Scan(&versions, &revocations) != nil || versions != len(migrations) || revocations {
		t.Fatal("schema 1 was not upgraded")
	}
	cfg, _ := pgx.ParseConfig(url)
	cfg.User = "capture_fixture_app"
	cfg.Password = password
	runtimeURL := cfg.ConnString()
	store, err := Open(ctx, runtimeURL, "test-plugin")
	if err != nil {
		t.Fatal("restricted runtime startup failed", err)
	}
	defer func() { store.Close() }()
	authority := store.AuthorityID()
	if len(authority) != 32 {
		t.Fatal("missing durable authority identity")
	}
	if _, err := Purge(ctx, url, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatal("purge ignored active authority")
	}
	if other, err := Open(ctx, runtimeURL, "test-plugin"); !errors.Is(err, ErrConflict) {
		if other != nil {
			other.Close()
		}
		t.Fatal("competing authority accepted")
	}
	if err := Migrate(ctx, url, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("migration ignored active owner")
	}
	var ddl, ledger, removeOps bool
	if admin.QueryRow(ctx, "SELECT has_schema_privilege('capture_fixture_app','capture','CREATE'),has_table_privilege('capture_fixture_app','capture.schema_version','UPDATE'),has_table_privilege('capture_fixture_app','capture.operations','DELETE')").Scan(&ddl, &ledger, &removeOps) != nil || ddl || ledger || removeOps {
		t.Fatal("runtime grants are too broad")
	}
	one, two := strings.Repeat("a", 64), strings.Repeat("b", 64)
	g1, err := store.NextGeneration(ctx, one)
	if err != nil || g1 != 1 {
		t.Fatal("first generation failed")
	}
	g2, err := store.NextGeneration(ctx, two)
	if err != nil || g2 != 1 {
		t.Fatal("second peer generation failed")
	}
	makeOp := func() Operation {
		return Operation{ID: pluginsdk.RandomID(), Owner: "owner", Hash: make([]byte, 32), ChannelID: pluginsdk.RandomID(), State: "starting", Revision: 1, ExpiresAt: time.Now().Add(time.Minute), Targets: []Target{{PeerID: one, Generation: 1, WireID: pluginsdk.RandomID(), State: "pending"}, {PeerID: two, Generation: 1, WireID: pluginsdk.RandomID(), State: "pending"}}}
	}
	bad := makeOp()
	bad.Targets[1].Generation = 2
	if err := store.Insert(ctx, bad); !errors.Is(err, ErrConflict) {
		t.Fatal("generation mismatch admitted", err)
	}
	if _, err := store.Get(ctx, bad.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("failed admission left operation metadata")
	}
	var count int
	if admin.QueryRow(ctx, "SELECT count(*) FROM capture.leases").Scan(&count) != nil || count != 0 {
		t.Fatal("failed admission left partial reservations")
	}
	op := makeOp()
	op.ActingFor = &pluginsdk.Acting{ID: "user:alice", Name: "Alice", RequestID: "req-1"}
	if err := store.Insert(ctx, op); err != nil {
		t.Fatal("admission failed", err)
	}
	overlap := makeOp()
	if err := store.Insert(ctx, overlap); !errors.Is(err, ErrConflict) {
		t.Fatal("overlapping clients admitted", err)
	}
	if _, err := store.Get(ctx, overlap.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("overlap rollback incomplete")
	}
	op.State = "stopping"
	op.Revision++
	if err := store.Save(ctx, op, 1); err != nil {
		t.Fatal("stop persistence failed")
	}
	if err := store.Insert(ctx, overlap); !errors.Is(err, ErrConflict) {
		t.Fatal("unconfirmed stop released reservation")
	}
	store.Close()
	if wrong, err := Open(ctx, runtimeURL, "different-plugin"); !errors.Is(err, ErrConflict) {
		if wrong != nil {
			wrong.Close()
		}
		t.Fatal("database identity changed silently")
	}
	store, err = Open(ctx, runtimeURL, "test-plugin")
	if err != nil {
		t.Fatal("recovery failed", err)
	}
	recovered, err := store.Get(ctx, op.ID)
	if store.AuthorityID() != authority || recovered.AuthorityID != authority {
		t.Fatal("recovery changed durable authority identity")
	}
	if err != nil || recovered.State != "interrupted" || recovered.Reason != "authority_restarted" || recovered.ActingFor == nil || *recovered.ActingFor != *op.ActingFor {
		t.Fatal("recovery resumed or hid uncertain execution")
	}
	if err := store.Insert(ctx, overlap); !errors.Is(err, ErrConflict) {
		t.Fatal("crash recovery lost reservation")
	}
	if generation, err := store.NextGeneration(ctx, one); err != nil || generation != 2 {
		t.Fatal("durable generation did not advance")
	}
	// A real terminal ACK, represented by released target metadata, permits reuse.
	recovered.State = "stopped"
	recovered.Revision++
	for i := range recovered.Targets {
		recovered.Targets[i].State = "completed"
		recovered.Targets[i].Released = true
	}
	if err := store.Save(ctx, recovered, recovered.Revision-1); err != nil {
		t.Fatal("terminal metadata failed", err)
	}
	overlap.Targets[0].Generation = 2
	if err := store.Insert(ctx, overlap); err != nil {
		t.Fatal("confirmed terminal execution remained reserved", err)
	}
	store.Close()
	// Offline retention must preserve unexpired execution fences and generation
	// counters, but can remove an operation after its conservative expiry guard.
	if removed, err := Purge(ctx, url, time.Now()); err != nil || removed != 0 {
		t.Fatal("purge removed unexpired metadata", err)
	}
	if _, err := admin.Exec(ctx, "UPDATE capture.operations SET expires_at=now()-interval '10 seconds' WHERE id=$1", op.ID); err != nil {
		t.Fatal("fixture expiry setup failed")
	}
	if removed, err := Purge(ctx, url, time.Now()); err != nil || removed != 1 {
		t.Fatal("eligible offline metadata was not removed", err)
	}
	if admin.QueryRow(ctx, "SELECT count(*) FROM capture.peers").Scan(&count) != nil || count != 2 {
		t.Fatal("purge erased generation fences")
	}
	if _, err := admin.Exec(ctx, "UPDATE capture.schema_version SET checksum=repeat('0',64)"); err != nil {
		t.Fatal("fixture checksum mutation failed")
	}
	if err := Migrate(ctx, url, ""); !errors.Is(err, ErrConflict) {
		t.Fatal("tampered plugin migration accepted")
	}
}
