package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresConfigs(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_configs"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_configs?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "configs-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "configs-fleet", MaxAgents: 10, MaxEvents: 10000,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 100 * time.Millisecond, Rollouts: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(s.Close)
	admin := issued.Credential.ID
	// Every assignment change is a rollout; none of these target eligible
	// agents, so the controller completes them on its next tick.
	rollout := func(id string) configmgmt.RolloutRequest {
		return configmgmt.RolloutRequest{ID: id, Policy: configmgmt.DefaultPolicy()}
	}
	settle := func(id string) {
		t.Helper()
		waitUntil(t, func() bool { r, err := s.GetRollout(ctx, id); return err == nil && r.State == configmgmt.Completed })
	}
	reader, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat("71", 16), Name: "reader", Role: "reader", ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, reader.Credential); err != nil {
		t.Fatal(err)
	}

	// Configs: admin-only writes, unique names, revisions.
	id := strings.Repeat("c1", 16)
	if _, err := s.CreateConfig(ctx, reader.Credential.ID, configmgmt.Config{ID: id, Name: "base"}); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("reader created a config:", err)
	}
	c, err := s.CreateConfig(ctx, admin, configmgmt.Config{ID: id, Name: "base", Description: "fleet base"})
	if err != nil || c.Revision != 1 || c.LatestVersion != 0 || c.CreatedBy != admin {
		t.Fatalf("create = %+v err = %v", c, err)
	}
	if _, err := s.CreateConfig(ctx, admin, configmgmt.Config{ID: strings.Repeat("c2", 16), Name: "base"}); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("duplicate name:", err)
	}
	if _, err := s.CreateConfig(ctx, admin, configmgmt.Config{ID: strings.Repeat("c2", 16), Name: "Bad Name"}); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("invalid name:", err)
	}
	c.Description = "renamed"
	if _, err := s.UpdateConfig(ctx, admin, c, 2); !errors.Is(err, fleet.ErrPrecondition) {
		t.Fatal("stale update:", err)
	}
	if c, err = s.UpdateConfig(ctx, admin, c, 1); err != nil || c.Revision != 2 || c.Description != "renamed" {
		t.Fatalf("update = %+v err = %v", c, err)
	}

	// Versions are numbered per config, bodies are read only on request and
	// never enter events.
	cursor, _ := s.Events(ctx, "", 10000)
	secretish := "receivers:\n  otlp:\n    protocols:\n      grpc: {}\nservice:\n  pipelines: {}\n"
	// A delegating credential's acting-for value rides along on its events.
	acting := &pluginsdk.Acting{ID: "user:alice", Name: "Alice", RequestID: "req-1"}
	v1, err := s.CreateVersion(fleet.WithActing(ctx, acting), admin, id, []byte(secretish), "first")
	if err != nil || v1.Version != 1 || len(v1.SHA256) != 64 || v1.Size != len(secretish) {
		t.Fatalf("version = %+v err = %v", v1, err)
	}
	if _, err := s.CreateVersion(ctx, reader.Credential.ID, id, []byte("{}"), ""); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("reader created a version:", err)
	}
	if _, err := s.CreateVersion(ctx, admin, strings.Repeat("c9", 16), []byte("{}"), ""); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("version of unknown config:", err)
	}
	got, err := s.GetVersion(ctx, id, 1, false)
	if err != nil || got.Body != nil || got.SHA256 != v1.SHA256 {
		t.Fatalf("version without body = %+v err = %v", got, err)
	}
	if got, err = s.GetVersion(ctx, id, 1, true); err != nil || got.Body == nil || *got.Body != secretish {
		t.Fatalf("version with body = %+v err = %v", got, err)
	}
	events, _ := s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 1 || events.Items[0].Kind != "config.version_created" || strings.Contains(string(events.Items[0].Detail), "otlp") {
		t.Fatalf("version events = %+v", events.Items)
	}
	if a := events.Items[0].ActingFor; a == nil || *a != *acting || events.Items[0].ActorID != admin {
		t.Fatalf("acting for = %+v", a)
	}

	// Assignments: static group, selector group and inline selector targets.
	group := strings.Repeat("9a", 16)
	member, outsider := strings.Repeat("a1", 16), strings.Repeat("a2", 16)
	agentCredential, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat("72", 16), Name: "agent", Role: "agent", ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, agentCredential.Credential); err != nil {
		t.Fatal(err)
	}
	held := strings.Repeat("dd", 32)
	memberGeneration, _, err := s.ConnectAgent(ctx, member, agentCredential.Credential.ID, fleet.Report{Capabilities: 3,
		RemoteConfig: &fleet.RemoteConfig{Hash: held, Status: "failed"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetConfigHold(ctx, member, memberGeneration, "", held); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutGroup(ctx, admin, fleet.Group{ID: group, Name: "static", AgentIDs: []string{member}}, 0); err != nil {
		t.Fatal(err)
	}
	selectorGroup := strings.Repeat("9b", 16)
	if _, err := s.PutGroup(ctx, admin, fleet.Group{ID: selectorGroup, Name: "prod", Selector: "deployment.environment.name=prod"}, 0); err != nil {
		t.Fatal(err)
	}
	staticAssignment := configmgmt.Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: id, Version: 1, Layer: 10, GroupID: group}
	if _, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e9", 16), Name: "missing", ConfigID: id, Version: 7, Layer: 10, GroupID: group}, rollout(strings.Repeat("f9", 16))); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("assignment of a missing version:", err)
	}
	cursor, _ = s.Events(ctx, "", 10000)
	a, view, err := s.CreateAssignment(ctx, admin, staticAssignment, rollout(strings.Repeat("f1", 16)))
	if err != nil || a.Revision != 1 || a.CredentialLabels != nil || a.Version != 0 || a.RolloutID != view.ID ||
		view.FromVersion != 0 || view.ToVersion != 1 || view.Targets != 0 || view.State != configmgmt.Running {
		t.Fatalf("assignment = %+v rollout = %+v err = %v", a, view, err)
	}
	events, _ = s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 2 || events.Items[0].Kind != "rollout.created" ||
		compact(events.Items[1].Detail) != fmt.Sprintf(`{"layer":10,"action":"created","version":1,"config_id":"%s"}`, id) {
		t.Fatalf("assignment events = %+v", events.Items)
	}
	if _, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e2", 16), Name: "base", ConfigID: id, Version: 1, Layer: 20, Selector: "a=b"}, rollout(strings.Repeat("f2", 16))); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("duplicate assignment name:", err)
	}
	if _, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e2", 16), Name: "both", ConfigID: id, Version: 1, Layer: 20, Selector: "a=b", GroupID: group}, rollout(strings.Repeat("f2", 16))); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("assignment with two targets:", err)
	}
	prod, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e2", 16), Name: "prod", ConfigID: id, Version: 1, Layer: 50, GroupID: selectorGroup}, rollout(strings.Repeat("f2", 16)))
	if err != nil {
		t.Fatal(err)
	}
	inline, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e3", 16), Name: "edge", ConfigID: id, Version: 1, Layer: 80,
		Selector: "service.name = edge", CredentialLabels: map[string]string{"env": "prod"}}, rollout(strings.Repeat("f3", 16)))
	if err != nil || inline.Selector != "service.name=edge" || inline.CredentialLabels["env"] != "prod" {
		t.Fatalf("inline assignment = %+v err = %v", inline, err)
	}
	if _, err := s.CreateRollout(ctx, admin, configmgmt.RolloutRequest{ID: strings.Repeat("f4", 16), AssignmentID: a.ID, ToVersion: 1, Policy: configmgmt.DefaultPolicy()}); !errors.Is(err, configmgmt.ErrRolloutActive) {
		t.Fatal("second rollout of a pending assignment:", err)
	}
	for _, r := range []string{"f1", "f2", "f3"} {
		settle(strings.Repeat(r, 16))
	}
	if a, err = s.GetAssignment(ctx, a.ID); err != nil || a.Version != 1 || a.Revision != 2 || a.RolloutID != "" {
		t.Fatalf("completed assignment = %+v err = %v", a, err)
	}
	targets, err := s.Targets(ctx)
	if err != nil || len(targets) != 3 {
		t.Fatalf("targets = %d err = %v", len(targets), err)
	}
	prodAttrs := map[string]string{"deployment.environment.name": "prod", "service.name": "edge"}
	matched := configmgmt.Match(targets, member, prodAttrs, map[string]string{"env": "prod"})
	if len(matched) != 3 || matched[0].Name != "base" || matched[1].Name != "prod" || matched[2].Name != "edge" {
		t.Fatalf("matched = %+v", matched)
	}
	if matched := configmgmt.Match(targets, outsider, prodAttrs, nil); len(matched) != 1 || matched[0].ID != prod.ID {
		t.Fatalf("outsider matched = %+v", matched)
	}
	desired, err := configmgmt.Describe(matched, "")
	if err != nil || len(desired.Layers) != 3 || desired.Layers[0].Name != "10-base" || desired.Layers[0].SHA256 != v1.SHA256 {
		t.Fatalf("desired = %+v err = %v", desired, err)
	}

	// Referenced configs, versions and groups cannot be deleted.
	if err := s.DeleteConfig(ctx, admin, id, c.Revision); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("deleted a referenced config:", err)
	}
	if err := s.DeleteGroup(ctx, admin, group, 1); !errors.Is(err, fleet.ErrConflict) {
		t.Fatal("deleted a referenced group:", err)
	}

	// Pruning keeps the newest versions and every referenced one.
	for i := 2; i <= configmgmt.KeepVersions+2; i++ {
		if _, err := s.CreateVersion(ctx, admin, id, fmt.Appendf(nil, "receivers:\n  otlp/v%d: {}\n", i), ""); err != nil {
			t.Fatal(i, err)
		}
	}
	page, err := s.ListVersions(ctx, id, 0, 500)
	if err != nil || len(page.Items) != configmgmt.KeepVersions+1 || page.Items[0].Version != configmgmt.KeepVersions+2 || page.Items[len(page.Items)-1].Version != 1 {
		t.Fatalf("versions after prune = %d err = %v", len(page.Items), err)
	}
	if _, err := s.GetVersion(ctx, id, 2, false); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("unreferenced old version kept:", err)
	}
	if page, err := s.ListVersions(ctx, id, 50, 10); err != nil || page.Items[0].Version != 49 || page.NextAfter != "40" {
		t.Fatalf("version page = %+v err = %v", page, err)
	}

	// An assignment moves to another version; deleting it frees the config.
	latest := configmgmt.KeepVersions + 2
	move := configmgmt.RolloutRequest{ID: strings.Repeat("f5", 16), AssignmentID: a.ID, ToVersion: latest, Policy: configmgmt.DefaultPolicy()}
	if _, err := s.CreateRollout(ctx, reader.Credential.ID, move); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("reader started a rollout:", err)
	}
	if _, err := s.CreateRollout(ctx, admin, move); err != nil {
		t.Fatal("rollout to latest:", err)
	}
	settle(move.ID)
	if a, err = s.GetAssignment(ctx, a.ID); err != nil || a.Version != latest || a.Revision != 3 {
		t.Fatalf("assignment update = %+v err = %v", a, err)
	}
	if _, err := s.CreateRollout(ctx, admin, configmgmt.RolloutRequest{ID: strings.Repeat("f6", 16), AssignmentID: a.ID, ToVersion: 2, Policy: configmgmt.DefaultPolicy()}); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("assignment moved to a pruned version:", err)
	}
	if _, err := s.CreateRollout(ctx, admin, configmgmt.RolloutRequest{ID: strings.Repeat("f6", 16), AssignmentID: a.ID, ToVersion: latest, Policy: configmgmt.DefaultPolicy()}); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("rollout to the current version:", err)
	}
	list, err := s.ListAssignments(ctx, "", 2)
	if err != nil || len(list.Items) != 2 || list.NextAfter != list.Items[1].ID {
		t.Fatalf("assignment page = %+v err = %v", list, err)
	}
	for i, x := range []configmgmt.Assignment{a, prod, inline} {
		remove := configmgmt.RolloutRequest{ID: fmt.Sprintf("%030x%02d", 0xf7, i), AssignmentID: x.ID, Delete: true, Policy: configmgmt.DefaultPolicy()}
		if _, err := s.CreateRollout(ctx, admin, remove); err != nil {
			t.Fatal("delete rollout:", err)
		}
		settle(remove.ID)
		if _, err := s.GetAssignment(ctx, x.ID); !errors.Is(err, fleet.ErrNotFound) {
			t.Fatal("deleted assignment remains:", err)
		}
	}
	if page, err := s.ListRollouts(ctx, "", 100); err != nil || len(page.Items) != 7 {
		t.Fatalf("rollouts = %d err = %v", len(page.Items), err)
	}
	if err := s.DeleteGroup(ctx, admin, group, 1); err != nil {
		t.Fatal("delete unreferenced group:", err)
	}
	if err := s.DeleteConfig(ctx, admin, id, c.Revision); err != nil {
		t.Fatal("delete config:", err)
	}
	if page, err := s.ListVersions(ctx, id, 0, 10); !errors.Is(err, fleet.ErrNotFound) || len(page.Items) != 0 {
		t.Fatal("versions outlived their config:", err)
	}

	// A hold is cleared once, with an event naming the hash, never a body.
	if err := s.ClearHold(ctx, reader.Credential.ID, member); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("reader cleared a hold:", err)
	}
	cursor, _ = s.Events(ctx, "", 10000)
	if err := s.ClearHold(ctx, admin, member); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearHold(ctx, admin, member); err != nil {
		t.Fatal("repeat clear:", err)
	}
	events, _ = s.Events(ctx, cursor.NextCursor, 100)
	if len(events.Items) != 1 || events.Items[0].Kind != "config.hold_cleared" || compact(events.Items[0].Detail) != `{"held_hash":"`+held+`"}` {
		t.Fatalf("hold events = %+v", events.Items)
	}
	if agent, _ := s.GetAgent(ctx, member); agent.RemoteConfig == nil || agent.RemoteConfig.Held != "" || agent.RemoteConfig.Status != "failed" {
		t.Fatalf("after clear = %+v", agent.RemoteConfig)
	}
	if err := s.ClearHold(ctx, admin, outsider); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("clear for unknown agent:", err)
	}
}
