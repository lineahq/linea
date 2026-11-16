package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
// Administrative events survive event trimming; agent activity never reaches
// the audit record; retention deletes by age.
func TestPostgresAuditRecord(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_audit"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_audit?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	if _, err := Bootstrap(ctx, url, "audit-fleet", "bootstrap-admin", nil); err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "audit-fleet", MaxAgents: 20, MaxEvents: 10000,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 50 * time.Millisecond, Rollouts: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	defer s.Close()
	group := strings.Repeat("a1", 16)
	if _, err := s.PutGroup(ctx, "admin-id", fleet.Group{ID: group, Name: "g", Kind: fleet.GroupSelector, Selector: "a=b"}, 0); err != nil {
		t.Fatal("put group:", err)
	}
	agent := strings.Repeat("b2", 16)
	if _, _, err := s.ConnectAgent(ctx, agent, "", fleet.Report{}, time.Now()); err != nil {
		t.Fatal("connect:", err)
	}
	if err := s.trimEvents(ctx, 0); err != nil {
		t.Fatal("trim:", err)
	}
	page, err := s.Audit(ctx, fleet.AuditQuery{Limit: 100})
	if err != nil {
		t.Fatal("audit:", err)
	}
	kinds := []string{}
	for _, e := range page.Items {
		kinds = append(kinds, e.Kind)
		if strings.HasPrefix(e.Kind, "agent.") || strings.HasPrefix(e.Kind, "replica.") {
			t.Fatalf("agent activity in the audit record: %s", e.Kind)
		}
	}
	if len(page.Items) == 0 || page.Items[0].Kind != "group.updated" || page.Items[0].SubjectID != group {
		t.Fatalf("audit record after trimming: %v", kinds)
	}
	if filtered, err := s.Audit(ctx, fleet.AuditQuery{Kind: "group.updated", Actor: "admin-id", Limit: 1}); err != nil || len(filtered.Items) != 1 {
		t.Fatalf("filtered read: %v %v", filtered, err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE fleet.audit SET occurred_at = now() - interval '401 days' WHERE kind='group.updated'"); err != nil {
		t.Fatal(err)
	}
	if err := s.retainAudit(ctx); err != nil {
		t.Fatal("retain:", err)
	}
	if after, err := s.Audit(ctx, fleet.AuditQuery{Kind: "group.updated", Limit: 10}); err != nil || len(after.Items) != 0 {
		t.Fatalf("expired record kept: %v %v", after, err)
	}
}
