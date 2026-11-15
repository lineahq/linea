package postgres

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
// The fixture user must be allowed to create roles and databases.
func TestPostgresRuntimeRoleManagesGroups(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close(ctx)
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_grants"); err != nil {
		t.Fatal(err)
	}
	const role, password = "fleet_grants_runtime", "grants-runtime-test"
	if _, err := fixture.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	admin := strings.Replace(base, "/fleet?", "/fleet_grants?", 1)
	if err := Migrate(ctx, admin); err != nil {
		t.Fatal("migration:", err)
	}
	if err := GrantRuntime(ctx, admin, role); err != nil {
		t.Fatal("grant runtime:", err)
	}
	issued, err := Bootstrap(ctx, admin, "grants-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	parsed, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(role, password)
	s, err := Open(ctx, parsed.String(), Options{ServerID: "grants-fleet", MaxAgents: 10, MaxEvents: 100})
	if err != nil {
		t.Fatal("open as runtime role:", err)
	}
	defer s.Close()
	const group = "0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a"
	if _, err := s.PutGroup(ctx, issued.Credential.ID, fleet.Group{ID: group, Name: "doomed", Selector: "deployment.environment.name=prod"}, 0); err != nil {
		t.Fatal("put group as runtime role:", err)
	}
	if err := s.DeleteGroup(ctx, issued.Credential.ID, group, 1); err != nil {
		t.Fatal("delete group as runtime role:", err)
	}
}
