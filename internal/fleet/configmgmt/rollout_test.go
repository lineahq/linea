package configmgmt

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

func agentIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i+1)
	}
	return ids
}

func TestPlanWavesDefaultAndCanary(t *testing.T) {
	ids := agentIDs(100)
	order, waves, count := PlanWaves("r1", ids, nil, DefaultPolicy().Waves)
	if count != 4 || len(order) != 100 {
		t.Fatal("wave count", count)
	}
	sizes := map[int]int{}
	for _, w := range waves {
		sizes[w]++
	}
	// Cumulative 1, 5, 25 and 100 agents.
	if sizes[0] != 1 || sizes[1] != 4 || sizes[2] != 20 || sizes[3] != 75 {
		t.Fatal("wave sizes", sizes)
	}
	if !slices.IsSorted(waves) {
		t.Fatal("waves are not in release order")
	}
	again, _, _ := PlanWaves("r1", reverse(ids), nil, DefaultPolicy().Waves)
	if !slices.Equal(order, again) {
		t.Fatal("order depends on input order")
	}
	other, _, _ := PlanWaves("r2", ids, nil, DefaultPolicy().Waves)
	if slices.Equal(order, other) {
		t.Fatal("order does not depend on the rollout")
	}

	canary := map[string]bool{ids[7]: true, ids[42]: true}
	order, waves, count = PlanWaves("r1", ids, func(id string) bool { return canary[id] }, []Wave{{Percent: 50}, {Percent: 100}})
	if count != 3 || waves[0] != 0 || waves[1] != 0 || waves[2] != 1 || !canary[order[0]] || !canary[order[1]] {
		t.Fatal("canary wave", count, waves[:3])
	}
	if sizes := countWaves(waves); sizes[1] != 49 || sizes[2] != 49 {
		t.Fatal("waves after the canary", sizes)
	}
}

func reverse(ids []string) []string {
	out := slices.Clone(ids)
	slices.Reverse(out)
	return out
}

func countWaves(waves []int) map[int]int {
	sizes := map[int]int{}
	for _, w := range waves {
		sizes[w]++
	}
	return sizes
}

func TestPlanWavesCollapsesEmptyBoundaries(t *testing.T) {
	for _, c := range []struct {
		agents, waves int
	}{{0, 0}, {1, 1}, {2, 2}, {3, 2}, {20, 3}, {21, 4}} {
		_, waves, count := PlanWaves("r", agentIDs(c.agents), nil, DefaultPolicy().Waves)
		if count != c.waves {
			t.Fatalf("%d agents: %d waves, want %d", c.agents, count, c.waves)
		}
		for i, w := range waves {
			if w >= count || i > 0 && w < waves[i-1] {
				t.Fatalf("%d agents: waves %v", c.agents, waves)
			}
		}
	}
	// A short explicit list still releases everyone in its last wave.
	_, waves, count := PlanWaves("r", agentIDs(10), nil, []Wave{{Count: 2}})
	if count != 1 || countWaves(waves)[0] != 10 {
		t.Fatal("last wave did not take the remainder", count, waves)
	}
}

func TestPolicyValidation(t *testing.T) {
	if err := DefaultPolicy().Validate(fleet.ValidID); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Policy){
		func(p *Policy) { p.Waves = nil },
		func(p *Policy) { p.Waves = make([]Wave, MaxWaves+1) },
		func(p *Policy) { p.Waves = []Wave{{Count: 1, Percent: 5}} },
		func(p *Policy) { p.Waves = []Wave{{Percent: 101}} },
		func(p *Policy) { p.CanaryGroupID = "x" },
		func(p *Policy) { p.BakeSeconds = -1 },
		func(p *Policy) { p.WaveTimeoutSeconds = 59 },
		func(p *Policy) { p.MaxFailurePercent = 101 },
		func(p *Policy) { p.OnFailure = "ignore" },
	}
	for i, mutate := range bad {
		p := DefaultPolicy()
		mutate(&p)
		if p.Validate(fleet.ValidID) == nil {
			t.Fatal("accepted invalid policy", i)
		}
	}
	r := RolloutRequest{ID: agentIDs(1)[0], AssignmentID: agentIDs(2)[1], ToVersion: 2, Policy: DefaultPolicy()}
	if r.Validate() != nil {
		t.Fatal("valid request rejected")
	}
	r.Delete = true
	if r.Validate() == nil {
		t.Fatal("version and delete accepted together")
	}
}

func TestBreachedThreshold(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		tally Tally
		want  bool
	}{
		{Tally{Settled: 1, Failures: 0}, false},
		{Tally{Settled: 1, Failures: 1}, true},
		{Tally{Settled: 100, Failures: 5}, false},
		{Tally{Settled: 100, Failures: 6}, true},
	}
	for _, c := range cases {
		if p.Breached(c.tally) != c.want {
			t.Fatal("threshold", c.tally)
		}
	}
	p.MaxFailures = 2
	if p.Breached(Tally{Settled: 3, Failures: 2}) || !p.Breached(Tally{Settled: 3, Failures: 3}) {
		t.Fatal("absolute threshold")
	}
}

type clock struct{ now time.Time }

func (c *clock) advance(d time.Duration) time.Time { c.now = c.now.Add(d); return c.now }

var (
	oldHash = []byte{1}
	newHash = []byte{2}
	yes     = true
	no      = false
)

func released(id string, wave int) Observation {
	return Observation{AgentID: id, Wave: wave, Outcome: OutcomeReleased, Connected: true, Desired: newHash, Status: Status{Hash: oldHash, State: Applied}, Healthy: &yes}
}

// apply folds a step's updates into the observations, as the store does.
func apply(obs []Observation, step Step) {
	for _, u := range step.Updates {
		for i := range obs {
			if obs[i].AgentID == u.AgentID {
				obs[i].Outcome, obs[i].ObservedAt = u.Outcome, u.ObservedAt
			}
		}
	}
}

func open(obs []Observation, upTo int) ([]Observation, map[int]Tally) {
	var out []Observation
	settled := map[int]Tally{}
	for _, o := range obs {
		if o.Wave > upTo {
			continue
		}
		if o.Outcome.Settled() {
			t := settled[o.Wave]
			t.add(o.Outcome, false)
			settled[o.Wave] = t
		} else {
			out = append(out, o)
		}
	}
	return out, settled
}

func TestEvaluateAdvancesThroughBakedWaves(t *testing.T) {
	c := &clock{now: time.Unix(1_000_000, 0)}
	r := Rollout{State: Running, CurrentWave: -1, WaveCount: 2, Policy: DefaultPolicy()}
	if step := Evaluate(r, nil, nil, c.now); step.Action != ReleaseWave {
		t.Fatal("first wave not released", step)
	}
	r.CurrentWave, r.PhaseStartedAt = 0, c.now
	obs := []Observation{released("a", 0), released("b", 1)}
	cur, settled := open(obs, 0)
	if step := Evaluate(r, cur, settled, c.advance(time.Second)); step.Action != NoAction || len(step.Updates) != 0 {
		t.Fatal("advanced before apply", step)
	}
	obs[0].Status = Status{Hash: newHash, State: Applied}
	cur, settled = open(obs, 0)
	step := Evaluate(r, cur, settled, c.advance(time.Second))
	if step.Action != NoAction || len(step.Updates) != 1 || step.Updates[0].Outcome != OutcomeBaking {
		t.Fatal("applied target is not baking", step)
	}
	apply(obs, step)
	cur, settled = open(obs, 0)
	if step := Evaluate(r, cur, settled, c.advance(time.Minute)); step.Action != NoAction || len(step.Updates) != 0 {
		t.Fatal("advanced before the bake ended", step)
	}
	cur, settled = open(obs, 0)
	step = Evaluate(r, cur, settled, c.advance(time.Minute))
	if step.Action != ReleaseWave || step.Updates[0].Outcome != OutcomeSucceeded {
		t.Fatal("baked wave did not advance", step)
	}
	apply(obs, step)

	r.CurrentWave, r.PhaseStartedAt = 1, c.now
	obs[1].Status = Status{Hash: newHash, State: Applied}
	cur, settled = open(obs, 1)
	step = Evaluate(r, cur, settled, c.advance(time.Second))
	apply(obs, step)
	cur, settled = open(obs, 1)
	if step := Evaluate(r, cur, settled, c.advance(2*time.Minute)); step.Action != Complete {
		t.Fatal("last wave did not complete", step)
	}
	if step := Evaluate(Rollout{State: Running, CurrentWave: -1}, nil, nil, c.now); step.Action != Complete {
		t.Fatal("rollout without targets did not complete", step)
	}
}

func TestEvaluateFailureThresholdPausesOrRollsBack(t *testing.T) {
	c := &clock{now: time.Unix(1_000_000, 0)}
	for _, onFailure := range []string{"pause", "rollback"} {
		p := DefaultPolicy()
		p.OnFailure = onFailure
		r := Rollout{State: Running, CurrentWave: 0, WaveCount: 2, PhaseStartedAt: c.now, Policy: p}
		failed := released("a", 0)
		failed.Status = Status{Hash: newHash, State: Failed}
		step := Evaluate(r, []Observation{failed}, nil, c.advance(time.Second))
		want := PauseAction
		if onFailure == "rollback" {
			want = Rollback
		}
		if step.Action != want || step.Updates[0].Outcome != OutcomeFailed {
			t.Fatal(onFailure, step)
		}
	}
	// A local rollback shows as the hold, while the agent reports the old
	// hash as applied.
	r := Rollout{State: Running, CurrentWave: 0, WaveCount: 1, PhaseStartedAt: c.now, Policy: DefaultPolicy()}
	held := released("a", 0)
	held.Held = newHash
	if step := Evaluate(r, []Observation{held}, nil, c.now); step.Action != PauseAction || step.Updates[0].Outcome != OutcomeFailed {
		t.Fatal("held target is not a failure", step)
	}
	// An unhealthy agent after applying is a failure too.
	sick := released("a", 0)
	sick.Status, sick.Healthy = Status{Hash: newHash, State: Applied}, &no
	if step := Evaluate(r, []Observation{sick}, nil, c.now); step.Action != PauseAction || step.Updates[0].Outcome != OutcomeUnhealthy {
		t.Fatal("unhealthy target is not a failure", step)
	}
	// Failures accepted on resume do not count again.
	if step := Evaluate(r, nil, map[int]Tally{0: {Settled: 1}}, c.now); step.Action != Complete {
		t.Fatal("accepted failure blocked the rollout", step)
	}
	// The total threshold applies across waves.
	p := DefaultPolicy()
	p.MaxFailures = 1
	r = Rollout{State: Running, CurrentWave: 1, WaveCount: 3, PhaseStartedAt: c.now, Policy: p}
	if step := Evaluate(r, nil, map[int]Tally{0: {Settled: 1, Failures: 1}, 1: {Settled: 1, Failures: 1}}, c.now); step.Action != PauseAction {
		t.Fatal("total threshold ignored", step)
	}
}

func TestEvaluateOfflineAndTimeouts(t *testing.T) {
	c := &clock{now: time.Unix(1_000_000, 0)}
	r := Rollout{State: Running, CurrentWave: 1, WaveCount: 3, PhaseStartedAt: c.now, Policy: DefaultPolicy()}
	gone := released("a", 1)
	gone.Connected = false
	step := Evaluate(r, []Observation{gone}, nil, c.advance(time.Second))
	if step.Action != ReleaseWave || step.Updates[0].Outcome != OutcomeOffline {
		t.Fatal("offline target blocked its wave", step)
	}
	// A slow target times out in its own wave and counts as a failure.
	slow := released("b", 1)
	if step := Evaluate(r, []Observation{slow}, nil, c.advance(14*time.Minute)); step.Action != NoAction {
		t.Fatal("timed out early", step)
	}
	step = Evaluate(r, []Observation{slow}, nil, c.advance(time.Minute))
	if step.Action != PauseAction || step.Updates[0].Outcome != OutcomeTimedOut {
		t.Fatal("slow target did not time out", step)
	}
	// An earlier wave's straggler never times out or blocks.
	late := released("c", 0)
	if step := Evaluate(r, []Observation{late}, nil, c.now); step.Action != ReleaseWave || len(step.Updates) != 0 {
		t.Fatal("earlier wave straggler blocked", step)
	}
	// A target baking at the timeout is not timed out.
	baking := released("d", 1)
	baking.Status = Status{Hash: newHash, State: Applied}
	since := c.now.Add(-time.Minute)
	baking.Outcome, baking.ObservedAt = OutcomeBaking, &since
	if step := Evaluate(r, []Observation{baking}, nil, c.now); step.Action != NoAction || len(step.Updates) != 0 {
		t.Fatal("baking target timed out", step)
	}
	// A target that no longer receives remote config is settled, not failed.
	none := released("e", 1)
	none.Desired = nil
	if step := Evaluate(r, []Observation{none}, nil, c.now); step.Action != ReleaseWave || step.Updates[0].Outcome != OutcomeUnmatched {
		t.Fatal("unmatched target", step)
	}
}

func TestEvaluatePausedUpdatesWithoutAdvancing(t *testing.T) {
	c := &clock{now: time.Unix(1_000_000, 0)}
	r := Rollout{State: Paused, CurrentWave: 0, WaveCount: 2, PhaseStartedAt: c.now, Policy: DefaultPolicy()}
	failed := released("a", 0)
	failed.Status = Status{Hash: newHash, State: Failed}
	step := Evaluate(r, []Observation{failed}, nil, c.now)
	if step.Action != NoAction || len(step.Updates) != 1 {
		t.Fatal("paused rollout acted", step)
	}
}

func TestRevertedAndControl(t *testing.T) {
	c := &clock{now: time.Unix(1_000_000, 0)}
	r := Rollout{State: RollingBack, PhaseStartedAt: c.now, Policy: DefaultPolicy()}
	back := released("a", 0)
	back.Desired = oldHash
	back.Status = Status{Hash: newHash, State: Applied}
	if Reverted(r, []Observation{back}, c.now) {
		t.Fatal("reverted before the agent reported the old config")
	}
	back.Status = Status{Hash: oldHash, State: Applied}
	gone := released("b", 0)
	gone.Connected = false
	if !Reverted(r, []Observation{back, gone}, c.now) {
		t.Fatal("not reverted")
	}
	back.Status = Status{Hash: newHash, State: Applying}
	if !Reverted(r, []Observation{back}, c.advance(15*time.Minute)) {
		t.Fatal("rollback did not end at the timeout")
	}

	transitions := []struct {
		c    Control
		from RolloutState
		to   RolloutState
	}{
		{ControlPause, Running, Paused}, {ControlResume, Paused, Running},
		{ControlRollback, Running, RollingBack}, {ControlRollback, Paused, RollingBack},
		{ControlCancel, Running, Cancelled}, {ControlCancel, RollingBack, Cancelled},
	}
	for _, tr := range transitions {
		if got, err := tr.c.Next(tr.from); err != nil || got != tr.to {
			t.Fatal(tr, got, err)
		}
	}
	for _, tr := range []struct {
		c    Control
		from RolloutState
	}{{ControlPause, Paused}, {ControlResume, Running}, {ControlRollback, RollingBack}, {ControlCancel, Completed}, {ControlPause, RolledBack}} {
		if _, err := tr.c.Next(tr.from); !errors.Is(err, ErrRolloutState) || !errors.Is(err, fleet.ErrConflict) {
			t.Fatal("invalid transition allowed", tr)
		}
	}
}

func TestMatchAppliesRolloutOverride(t *testing.T) {
	a, b := agentIDs(2)[0], agentIDs(2)[1]
	members := []string{a, b}
	base, _ := NewTarget(Assignment{ID: "base", Name: "base", ConfigID: "c", Version: 1, Layer: 10, GroupID: "g"}, []byte{1}, "", members)
	base.Override = &Override{RolloutID: "r", Version: 2, Digest: []byte{2}, Released: map[string]struct{}{a: {}}}
	got := Match([]Target{base}, a, nil, nil)
	if len(got) != 1 || got[0].Version != 2 || got[0].Digest[0] != 2 {
		t.Fatal("released target did not get the rollout version", got)
	}
	if got := Match([]Target{base}, b, nil, nil); len(got) != 1 || got[0].Version != 1 {
		t.Fatal("unreleased target changed", got)
	}
	// A deletion removes the layer for released targets only.
	base.Override.Version, base.Override.Digest = 0, nil
	if got := Match([]Target{base}, a, nil, nil); len(got) != 0 {
		t.Fatal("deleted layer still matched", got)
	}
	// A new assignment has no version until its rollout completes.
	created, _ := NewTarget(Assignment{ID: "new", Name: "new", ConfigID: "c", Layer: 20, GroupID: "g"}, nil, "", members)
	created.Override = &Override{RolloutID: "r", Version: 1, Digest: []byte{1}, Released: map[string]struct{}{a: {}}}
	if got := Match([]Target{created}, a, nil, nil); len(got) != 1 || got[0].Version != 1 {
		t.Fatal("released target did not get the new layer", got)
	}
	if got := Match([]Target{created}, b, nil, nil); len(got) != 0 {
		t.Fatal("unreleased target got the new layer", got)
	}
}
