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
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresRollouts(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_rollouts"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_rollouts?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "rollouts-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	open := func() *Store {
		s, err := Open(ctx, url, Options{ServerID: "rollouts-fleet", MaxAgents: 20, MaxEvents: 10000,
			Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 50 * time.Millisecond, Rollouts: 100 * time.Millisecond})
		if err != nil {
			t.Fatal("open:", err)
		}
		return s
	}
	s := open()
	admin := issued.Credential.ID

	managed, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat("72", 16), Name: "managed", Role: "agent", RemoteConfig: true, ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	reporting, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat("73", 16), Name: "reporting", Role: "agent", ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []auth.Credential{managed.Credential, reporting.Credential} {
		if err := s.InsertCredential(ctx, admin, c); err != nil {
			t.Fatal(err)
		}
	}

	// Four eligible agents; one without the capability and one whose
	// credential does not allow remote config are left out of the snapshot.
	attrs := map[string]string{"env": "prod"}
	eligible := []string{strings.Repeat("a1", 16), strings.Repeat("a2", 16), strings.Repeat("a3", 16), strings.Repeat("a4", 16)}
	generations := map[string]uint64{}
	connect := func(st *Store, id, credential string, capabilities uint64) {
		t.Helper()
		g, _, err := st.ConnectAgent(ctx, id, credential, fleet.Report{Capabilities: capabilities, Attributes: attrs}, time.Now())
		if err != nil {
			t.Fatal("connect:", err)
		}
		generations[id] = g
	}
	for _, id := range eligible {
		connect(s, id, managed.Credential.ID, 3)
	}
	connect(s, strings.Repeat("b1", 16), managed.Credential.ID, 1)
	connect(s, strings.Repeat("b2", 16), reporting.Credential.ID, 3)
	waitUntil(t, func() bool { page, _ := s.ListAgents(ctx, fleet.AgentQuery{Limit: 10}); return len(page.Items) == 6 })

	config := strings.Repeat("c1", 16)
	if _, err := s.CreateConfig(ctx, admin, configmgmt.Config{ID: config, Name: "base"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := s.CreateVersion(ctx, admin, config, fmt.Appendf(nil, "receivers:\n  otlp/v%d: {}\n", i), ""); err != nil {
			t.Fatal(err)
		}
	}

	// desired computes an agent's desired hash as a replica would.
	desired := func(st *Store, id string) string {
		t.Helper()
		targets, err := st.Targets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		d, err := configmgmt.Describe(configmgmt.Match(targets, id, attrs, map[string]string{}), "")
		if err != nil || d == nil {
			t.Fatalf("desired for %s = %+v err = %v", id, d, err)
		}
		return d.Hash
	}
	report := func(st *Store, id, hash, status string) {
		t.Helper()
		r := fleet.Report{Capabilities: 3, Attributes: attrs, RemoteConfig: &fleet.RemoteConfig{Hash: hash, Status: status}}
		if err := st.ObserveAgent(ctx, id, managed.Credential.ID, generations[id], r, time.Now()); err != nil {
			t.Fatal("observe:", err)
		}
	}
	wave := func(st *Store, id string, n int) []string {
		t.Helper()
		page, err := st.ListRolloutTargets(ctx, id, "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, target := range page.Items {
			if target.Wave == n {
				out = append(out, target.AgentID)
			}
		}
		return out
	}
	state := func(st *Store, id string, want configmgmt.RolloutState, wave int) configmgmt.RolloutView {
		t.Helper()
		var r configmgmt.RolloutView
		waitUntil(t, func() bool {
			r, err = st.GetRollout(ctx, id)
			return err == nil && r.State == want && r.CurrentWave >= wave
		})
		return r
	}
	policy := configmgmt.DefaultPolicy()
	policy.Waves = []configmgmt.Wave{{Count: 1}, {Percent: 100}}
	policy.BakeSeconds, policy.WaveTimeoutSeconds = 0, 60

	// Creation: the snapshot holds only eligible agents, in two waves.
	assignment := configmgmt.Assignment{ID: strings.Repeat("e1", 16), Name: "base", ConfigID: config, Version: 1, Layer: 10, Selector: "env=prod"}
	create := strings.Repeat("f1", 16)
	a, view, err := s.CreateAssignment(ctx, admin, assignment, configmgmt.RolloutRequest{ID: create, Policy: policy})
	if err != nil || view.Targets != 4 || view.WaveCount != 2 || a.Version != 0 {
		t.Fatalf("create = %+v %+v err = %v", a, view, err)
	}
	// Another assignment over the same agents waits for this rollout.
	other := configmgmt.Assignment{ID: strings.Repeat("e2", 16), Name: "other", ConfigID: config, Version: 1, Layer: 20, Selector: "env=prod"}
	if _, _, err := s.CreateAssignment(ctx, admin, other, configmgmt.RolloutRequest{ID: strings.Repeat("f2", 16), Policy: policy}); !errors.Is(err, configmgmt.ErrRolloutActive) {
		t.Fatal("overlapping rollout:", err)
	}
	if _, err := s.GetAssignment(ctx, other.ID); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("refused assignment was stored:", err)
	}
	state(s, create, configmgmt.Running, 0)
	canary := wave(s, create, 0)
	if len(canary) != 1 {
		t.Fatalf("wave 0 = %v", canary)
	}
	// Only released agents receive the pending assignment's layer.
	targets, _ := s.Targets(ctx)
	for _, id := range eligible {
		if matched := configmgmt.Match(targets, id, attrs, map[string]string{}); (len(matched) == 1) != (id == canary[0]) {
			t.Fatalf("%s matched %d layers before its wave", id, len(matched))
		}
	}
	v1 := desired(s, canary[0])
	report(s, canary[0], v1, "applied")
	state(s, create, configmgmt.Running, 1)

	// A failure in wave 1 pauses the rollout; resume accepts it.
	rest := wave(s, create, 1)
	report(s, rest[0], v1, "failed")
	report(s, rest[1], v1, "applied")
	paused := state(s, create, configmgmt.Paused, 1)
	if paused.Reason != "threshold" || paused.Waves[1].Outcomes[configmgmt.OutcomeFailed] != 1 {
		t.Fatalf("paused = %+v", paused)
	}
	if _, err := s.ControlRollout(ctx, admin, create, configmgmt.ControlPause); !errors.Is(err, configmgmt.ErrRolloutState) {
		t.Fatal("paused a paused rollout:", err)
	}
	if _, err := s.ControlRollout(ctx, admin, create, configmgmt.ControlResume); err != nil {
		t.Fatal(err)
	}
	report(s, rest[2], v1, "applied")
	state(s, create, configmgmt.Completed, 1)
	if a, err = s.GetAssignment(ctx, a.ID); err != nil || a.Version != 1 || a.RolloutID != "" {
		t.Fatalf("created assignment = %+v err = %v", a, err)
	}
	if failed, _ := s.ListRolloutTargets(ctx, create, configmgmt.OutcomeFailed, "", 10); len(failed.Items) != 1 || !failed.Items[0].Accepted {
		t.Fatalf("failed targets = %+v", failed.Items)
	}

	// Cancelling a creating rollout removes its assignment.
	cancelled := strings.Repeat("f3", 16)
	if _, _, err := s.CreateAssignment(ctx, admin, other, configmgmt.RolloutRequest{ID: cancelled, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.ControlRollout(ctx, admin, cancelled, configmgmt.ControlCancel); err != nil || r.State != configmgmt.Cancelled {
		t.Fatalf("cancel = %+v err = %v", r, err)
	}
	if _, err := s.GetAssignment(ctx, other.ID); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("cancelled assignment remains:", err)
	}

	// A second replica takes over as leader mid-rollout and rolls back.
	s2 := open()
	t.Cleanup(s2.Close)
	upgrade := strings.Repeat("f4", 16)
	if _, err := s.CreateRollout(ctx, admin, configmgmt.RolloutRequest{ID: upgrade, AssignmentID: a.ID, ToVersion: 2, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	state(s, upgrade, configmgmt.Running, 0)
	canary = wave(s, upgrade, 0)
	v2 := desired(s, canary[0])
	if v2 == v1 {
		t.Fatal("upgrade did not change the canary's desired config")
	}
	s.Close()
	for _, id := range eligible {
		connect(s2, id, managed.Credential.ID, 3)
	}
	report(s2, canary[0], v2, "failed")
	state(s2, upgrade, configmgmt.Paused, 0)
	if r, err := s2.ControlRollout(ctx, admin, upgrade, configmgmt.ControlRollback); err != nil || r.State != configmgmt.RollingBack {
		t.Fatalf("rollback = %+v err = %v", r, err)
	}
	if back := desired(s2, canary[0]); back != v1 {
		t.Fatal("rollback did not restore the previous config")
	}
	time.Sleep(300 * time.Millisecond)
	if r, _ := s2.GetRollout(ctx, upgrade); r.State != configmgmt.RollingBack {
		t.Fatalf("finished rollback before the agent reverted: %+v", r)
	}
	report(s2, canary[0], v1, "applied")
	state(s2, upgrade, configmgmt.RolledBack, 0)
	if a, err = s2.GetAssignment(ctx, a.ID); err != nil || a.Version != 1 {
		t.Fatalf("rolled back assignment = %+v err = %v", a, err)
	}
	events, _ := s2.Events(ctx, "", 10000)
	kinds := map[string]int{}
	for _, e := range events.Items {
		kinds[e.Kind]++
	}
	for kind, n := range map[string]int{"rollout.created": 3, "rollout.wave_released": 3, "rollout.paused": 2, "rollout.resumed": 1,
		"rollout.completed": 1, "rollout.cancelled": 1, "rollout.rolling_back": 1, "rollout.rolled_back": 1} {
		if kinds[kind] != n {
			t.Fatalf("%s events = %d, want %d (%v)", kind, kinds[kind], n, kinds)
		}
	}
}
