package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresBootstrapToken(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_bootstrap"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_bootstrap?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migration:", err)
	}
	token := "of1_" + strings.Repeat("ab", 16) + "_" + strings.Repeat("cd", 32)
	other := "of1_" + strings.Repeat("ef", 16) + "_" + strings.Repeat("cd", 32)

	if _, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", "of1_short"); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatalf("malformed token: %v", err)
	}
	if created, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", token); err != nil || !created {
		t.Fatalf("first bootstrap: created %v, %v", created, err)
	}
	if created, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", token); err != nil || created {
		t.Fatalf("repeated bootstrap: created %v, %v", created, err)
	}
	if _, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", other); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("second admin: %v", err)
	}
	if _, err := Bootstrap(ctx, url, "bootstrap-fleet", "another-admin"); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("generated admin beside the installed one: %v", err)
	}
	if _, err := BootstrapToken(ctx, url, "other-fleet", "linea-server", token); err == nil {
		t.Fatal("bootstrap accepted another fleet identity")
	}

	db, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var expires *string
	if err := db.QueryRow(ctx, "SELECT expires_at::text FROM fleet.credentials WHERE id=$1", strings.Repeat("ab", 16)).Scan(&expires); err != nil || expires != nil {
		t.Fatalf("installed admin expiry %v, %v", expires, err)
	}
	if _, err := db.Exec(ctx, "UPDATE fleet.credentials SET revoked_at=now() WHERE id=$1", strings.Repeat("ab", 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", token); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("revoked token bootstrapped again: %v", err)
	}
	if created, err := BootstrapToken(ctx, url, "bootstrap-fleet", "linea-server", other); err != nil || !created {
		t.Fatalf("new token after revocation: created %v, %v", created, err)
	}
}

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresInstallOperatorToken(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_operator_token"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_operator_token?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("migration:", err)
	}
	admin := "of1_" + strings.Repeat("01", 16) + "_" + strings.Repeat("cd", 32)
	token := "of1_" + strings.Repeat("ab", 16) + "_" + strings.Repeat("cd", 32)
	wrongSecret := "of1_" + strings.Repeat("ab", 16) + "_" + strings.Repeat("ef", 32)
	expiring := "of1_" + strings.Repeat("23", 16) + "_" + strings.Repeat("cd", 32)
	if created, err := BootstrapToken(ctx, url, "operator-fleet", "deployment-admin", admin); err != nil || !created {
		t.Fatalf("admin bootstrap: created %v, %v", created, err)
	}

	if _, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", "of1_short"); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatalf("malformed token: %v", err)
	}
	if _, err := InstallOperatorToken(ctx, url, "other-fleet", "linea-server", token); err == nil {
		t.Fatal("operator install accepted another fleet identity")
	}
	if created, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", token); err != nil || !created {
		t.Fatalf("first install: created %v, %v", created, err)
	}
	if created, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", token); err != nil || created {
		t.Fatalf("repeated install: created %v, %v", created, err)
	}
	if _, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", wrongSecret); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("same credential with another secret: %v", err)
	}
	if _, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", admin); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("admin token installed as operator: %v", err)
	}
	if created, err := BootstrapToken(ctx, url, "operator-fleet", "deployment-admin", admin); err != nil || created {
		t.Fatalf("admin changed by the operator install: created %v, %v", created, err)
	}

	db, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	var role string
	var expires *string
	if err := db.QueryRow(ctx, "SELECT role,expires_at::text FROM fleet.credentials WHERE id=$1", strings.Repeat("ab", 16)).Scan(&role, &expires); err != nil || role != "operator" || expires != nil {
		t.Fatalf("installed operator %q expiry %v, %v", role, expires, err)
	}
	var events int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM fleet.events WHERE kind='credential.created' AND subject_id=$1", strings.Repeat("ab", 16)).Scan(&events); err != nil || events != 1 {
		t.Fatalf("operator creation events %d, %v", events, err)
	}

	if _, err := db.Exec(ctx, "UPDATE fleet.credentials SET expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1", strings.Repeat("ab", 16)); err != nil {
		t.Fatal(err)
	}
	if created, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", token); err != nil || created {
		t.Fatalf("unexpired operator: created %v, %v", created, err)
	}
	if _, err := db.Exec(ctx, "UPDATE fleet.credentials SET revoked_at=now() WHERE id=$1", strings.Repeat("ab", 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", token); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("revoked operator installed again: %v", err)
	}
	if created, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", expiring); err != nil || !created {
		t.Fatalf("second operator: created %v, %v", created, err)
	}
	if _, err := db.Exec(ctx, "UPDATE fleet.credentials SET created_at=created_at-interval '2 hours', expires_at=clock_timestamp()-interval '1 hour' WHERE id=$1", strings.Repeat("23", 16)); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallOperatorToken(ctx, url, "operator-fleet", "linea-server", expiring); !errors.Is(err, fleet.ErrConflict) {
		t.Fatalf("expired operator installed again: %v", err)
	}
}
