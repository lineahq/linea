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
