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
func TestPostgresRemoteConfig(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_remote_config"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_remote_config?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "remote-config-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "remote-config-fleet", MaxAgents: 5, MaxEvents: 1000,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(s.Close)
	admin := issued.Credential.ID

	// Only agent credentials carry the remote config opt-in; it defaults off.
	if _, err := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000051", Name: "reader", Role: "reader", RemoteConfig: true, ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now()); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("reader credential accepted remote_config:", err)
	}
	c, err := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000052", Name: "agent", Role: "agent", ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, c.Credential); err != nil {
		t.Fatal(err)
	}
	credential := c.Credential.ID
	if stored, err := s.Credential(ctx, credential); err != nil || stored.RemoteConfig {
		t.Fatalf("new credential = %+v err = %v", stored, err)
	}
	cursor, _ := s.Events(ctx, "", 1000)
	updated, err := s.SetCredentialRemoteConfig(ctx, admin, credential, true)
	if err != nil || !updated.RemoteConfig {
		t.Fatalf("enable remote config = %+v err = %v", updated, err)
	}
	if again, err := s.SetCredentialRemoteConfig(ctx, admin, credential, true); err != nil || !again.RemoteConfig {
		t.Fatal("repeat enable:", err)
	}
	events, _ := s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 1 || events.Items[0].Kind != "credential.updated" || compact(events.Items[0].Detail) != `{"remote_config":true}` {
		t.Fatalf("credential events = %+v", events.Items)
	}
	if _, err := s.SetCredentialRemoteConfig(ctx, admin, admin, true); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("admin credential accepted remote_config:", err)
	}
	if _, err := s.SetCredentialRemoteConfig(ctx, credential, credential, false); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("agent changed its own policy:", err)
	}
	if _, err := s.SetCredentialRemoteConfig(ctx, admin, "00000000000000000000000000000059", true); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("unknown credential:", err)
	}

	// Remote config status is stored only when it changes, under the config
	// field group, and survives a reconnect with its hold.
	id := strings.Repeat("5a", 16)
	hashA, hashB := strings.Repeat("aa", 32), strings.Repeat("bb", 32)
	report := fleet.Report{Capabilities: 1, RemoteConfig: &fleet.RemoteConfig{Hash: hashA, Status: "applied"}}
	generation, _, err := s.ConnectAgent(ctx, id, credential, report, time.Now())
	if err != nil {
		t.Fatal("connect:", err)
	}
	a, err := s.GetAgent(ctx, id)
	if err != nil || a.RemoteConfig == nil || a.RemoteConfig.Hash != hashA || a.RemoteConfig.Status != "applied" || a.RemoteConfig.ObservedAt.IsZero() {
		t.Fatalf("connected agent = %+v err = %v", a.RemoteConfig, err)
	}
	observed := a.RemoteConfig.ObservedAt
	changes := func(r fleet.Report) string {
		t.Helper()
		cursor, _ := s.Events(ctx, "", 1000)
		if err := s.ObserveAgent(ctx, id, credential, generation, r, time.Now()); err != nil {
			t.Fatal("observe:", err)
		}
		events, _ := s.Events(ctx, cursor.NextCursor, 100)
		out := []string{}
		for _, e := range events.Items {
			out = append(out, e.Kind+compact(e.Detail))
		}
		return strings.Join(out, ";")
	}
	if got := changes(report); got != "" {
		t.Fatalf("unchanged status wrote %q", got)
	}
	failed := fleet.Report{Capabilities: 1, RemoteConfig: &fleet.RemoteConfig{Hash: hashB, Status: "failed", Error: "bad config"}}
	if got := changes(failed); got != `agent.changed{"changed":["config"]}` {
		t.Fatalf("failed status events = %q", got)
	}
	a, _ = s.GetAgent(ctx, id)
	if a.RemoteConfig.Held != "" || a.RemoteConfig.Error != "bad config" || !a.RemoteConfig.ObservedAt.After(observed) {
		t.Fatalf("failed status = %+v", a.RemoteConfig)
	}
	observed = a.RemoteConfig.ObservedAt

	// The hold is written only by compare-and-set, never by reports.
	cursor, _ = s.Events(ctx, "", 1000)
	if held, err := s.SetConfigHold(ctx, id, generation, "", hashB); err != nil || held != hashB {
		t.Fatalf("set hold = %q err = %v", held, err)
	}
	if held, err := s.SetConfigHold(ctx, id, generation, "", hashA); err != nil || held != hashB {
		t.Fatalf("stale compare-and-set = %q err = %v", held, err)
	}
	if _, err := s.SetConfigHold(ctx, id, generation+5, hashB, ""); !errors.Is(err, fleet.ErrStale) {
		t.Fatal("hold written for a stale generation:", err)
	}
	events, _ = s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 1 || events.Items[0].Kind != "config.held" || compact(events.Items[0].Detail) != `{"held_hash":"`+hashB+`"}` || events.Items[0].ActorID != "server" {
		t.Fatalf("hold events = %+v", events.Items)
	}
	a, _ = s.GetAgent(ctx, id)
	if a.RemoteConfig.Held != hashB || !a.RemoteConfig.ObservedAt.Equal(observed) {
		t.Fatalf("held = %+v", a.RemoteConfig)
	}
	if holds, err := s.ConfigHolds(ctx); err != nil || len(holds) != 1 || holds[id] != hashB {
		t.Fatalf("owned holds = %v err = %v", holds, err)
	}
	if managed, err := s.ManagedCredentials(ctx); err != nil || len(managed) != 1 || managed[credential] == nil {
		t.Fatalf("managed credentials = %v err = %v", managed, err)
	}
	// Reports keep the hold, and so do reports without the section.
	if got := changes(fleet.Report{Capabilities: 1, RemoteConfig: &fleet.RemoteConfig{Hash: hashA, Status: "applied"}}); got != `agent.changed{"changed":["config"]}` {
		t.Fatalf("status change events = %q", got)
	}
	if got := changes(fleet.Report{Capabilities: 1}); got != "" {
		t.Fatalf("omitted status wrote %q", got)
	}
	if err := s.ObserveAgent(ctx, id, credential, generation, failed, time.Now()); err != nil {
		t.Fatal(err)
	}
	generation, stored, err := s.ConnectAgent(ctx, id, credential, failed, time.Now())
	if err != nil || stored.RemoteConfig == nil || stored.RemoteConfig.Held != hashB || stored.RemoteConfig.Hash != hashB || stored.RemoteConfig.Status != "failed" {
		t.Fatalf("reconnect returned %+v err = %v", stored.RemoteConfig, err)
	}
	cursor, _ = s.Events(ctx, "", 1000)
	if held, err := s.SetConfigHold(ctx, id, generation, hashB, ""); err != nil || held != "" {
		t.Fatalf("clear hold = %q err = %v", held, err)
	}
	events, _ = s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 1 || events.Items[0].Kind != "config.hold_cleared" {
		t.Fatalf("clear events = %+v", events.Items)
	}
	page, err := s.ListAgents(ctx, fleet.AgentQuery{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].RemoteConfig == nil || page.Items[0].RemoteConfig.Status != "failed" {
		t.Fatalf("page = %+v err = %v", page, err)
	}
	// An agent that never reported a status has no remote config section.
	other := strings.Repeat("5b", 16)
	if _, _, err := s.ConnectAgent(ctx, other, credential, fleet.Report{Capabilities: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAgent(ctx, other); a.RemoteConfig != nil {
		t.Fatalf("unreported status = %+v", a.RemoteConfig)
	}
}
