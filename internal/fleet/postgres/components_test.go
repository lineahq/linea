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
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresComponentSets(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_components"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_components?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "components-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "components-fleet", MaxAgents: 20, MaxEvents: 10000,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 50 * time.Millisecond, Rollouts: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	defer s.Close()
	admin := issued.Credential.ID

	// Sets are stored once per build; the first report wins.
	withOTLP := configmgmt.Components{"receivers": {"otlp"}, "exporters": {"debug"}}
	without := configmgmt.Components{"receivers": {"filelog"}}
	for hash, set := range map[string]configmgmt.Components{"aa": withOTLP, "bb": without} {
		if err := s.SaveComponentSet(ctx, hash, set); err != nil {
			t.Fatal("save:", err)
		}
	}
	if err := s.SaveComponentSet(ctx, "aa", without); err != nil {
		t.Fatal("repeated save:", err)
	}
	if got, err := s.ComponentSet(ctx, "aa"); err != nil || len(got["receivers"]) != 1 || got["receivers"][0] != "otlp" {
		t.Fatalf("stored set %v %v", got, err)
	}
	if _, err := s.ComponentSet(ctx, "cc"); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("unknown build:", err)
	}
	if err := s.SaveComponentSet(ctx, "XYZ", withOTLP); !errors.Is(err, fleet.ErrInvalid) {
		t.Fatal("invalid hash accepted:", err)
	}

	managed, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat("72", 16), Name: "managed", Role: "agent", RemoteConfig: true, ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, managed.Credential); err != nil {
		t.Fatal(err)
	}
	attrs := map[string]string{"env": "prod"}
	compatible, incompatible, unknown := strings.Repeat("a1", 16), strings.Repeat("a2", 16), strings.Repeat("a3", 16)
	generations := map[string]uint64{}
	for id, hash := range map[string]string{compatible: "aa", incompatible: "bb", unknown: ""} {
		g, _, err := s.ConnectAgent(ctx, id, managed.Credential.ID, fleet.Report{Capabilities: 3, Attributes: attrs, AvailableComponents: hash}, time.Now())
		if err != nil {
			t.Fatal("connect:", err)
		}
		generations[id] = g
	}
	waitUntil(t, func() bool { page, _ := s.ListAgents(ctx, fleet.AgentQuery{Limit: 10}); return len(page.Items) == 3 })
	if a, err := s.GetAgent(ctx, compatible); err != nil || a.AvailableComponentsHash != "aa" {
		t.Fatalf("agent build hash %q %v", a.AvailableComponentsHash, err)
	}

	config := strings.Repeat("c1", 16)
	if _, err := s.CreateConfig(ctx, admin, configmgmt.Config{ID: config, Name: "base"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateVersion(ctx, admin, config, []byte("receivers:\n  otlp: {}\n"), ""); err != nil {
		t.Fatal(err)
	}
	policy := configmgmt.DefaultPolicy()
	policy.Waves, policy.BakeSeconds = []configmgmt.Wave{{Percent: 100}}, 0
	rollout := strings.Repeat("f1", 16)
	if _, _, err := s.CreateAssignment(ctx, admin, configmgmt.Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: config, Version: 1, Layer: 10, Selector: "env=prod"},
		configmgmt.RolloutRequest{ID: rollout, Policy: policy}); err != nil {
		t.Fatal("create:", err)
	}
	outcomes := func() map[string]configmgmt.RolloutTarget {
		page, err := s.ListRolloutTargets(ctx, rollout, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]configmgmt.RolloutTarget{}
		for _, target := range page.Items {
			out[target.AgentID] = target
		}
		return out
	}
	// The build without otlp is never released; an agent without a stored
	// set is.
	waitUntil(t, func() bool {
		o := outcomes()
		return o[compatible].Outcome == configmgmt.OutcomeReleased && o[unknown].Outcome == configmgmt.OutcomeReleased
	})
	if o := outcomes()[incompatible]; o.Outcome != configmgmt.OutcomeIncompatible || o.ReleasedAt != nil {
		t.Fatalf("incompatible target %+v", o)
	}
	// The unknown agent reports a build without otlp: it is withdrawn.
	if err := s.ObserveAgent(ctx, unknown, managed.Credential.ID, generations[unknown], fleet.Report{Capabilities: 3, Attributes: attrs, AvailableComponents: "bb"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		o := outcomes()[unknown]
		return o.Outcome == configmgmt.OutcomeIncompatible && o.ReleasedAt == nil
	})
	targets, err := s.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(configmgmt.Match(targets, unknown, attrs, nil)) != 0 || len(configmgmt.Match(targets, incompatible, attrs, nil)) != 0 {
		t.Fatal("incompatible agents still receive the rollout's layer")
	}
	d, err := configmgmt.Describe(configmgmt.Match(targets, compatible, attrs, nil), "")
	if err != nil || d == nil {
		t.Fatal("compatible agent lost its layer:", err)
	}
	// Incompatible targets do not block: the compatible one completes it.
	if err := s.ObserveAgent(ctx, compatible, managed.Credential.ID, generations[compatible], fleet.Report{Capabilities: 3, Attributes: attrs,
		RemoteConfig: &fleet.RemoteConfig{Hash: d.Hash, Status: string(configmgmt.Applied)}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		r, err := s.GetRollout(ctx, rollout)
		return err == nil && r.State == configmgmt.Completed
	})
	if o := outcomes(); o[compatible].Outcome != configmgmt.OutcomeSucceeded || o[incompatible].Outcome != configmgmt.OutcomeIncompatible {
		t.Fatalf("final outcomes %+v", o)
	}

	// Retention drops only sets that no agent references.
	if err := s.SaveComponentSet(ctx, "dd", without); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE fleet.component_sets SET first_seen_at=now() - interval '2 hours'"); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneComponentSets(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ComponentSet(ctx, "dd"); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("unreferenced set kept:", err)
	}
	for _, hash := range []string{"aa", "bb"} {
		if _, err := s.ComponentSet(ctx, hash); err != nil {
			t.Fatal("referenced set pruned:", hash, err)
		}
	}
}
