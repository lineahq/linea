package configmgmt

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

// Rollout bounds. Every agent is in at most one active rollout, so active
// rollouts together never hold more targets than the inventory.
const (
	MaxRolloutTargets = 20000
	MaxWaves          = 10
	MaxActiveRollouts = 50
	// KeepRollouts is how many finished rollouts are kept with their targets.
	KeepRollouts = 100
)

// Rollout errors wrap fleet errors, so generic handling still applies.
var (
	// ErrRolloutActive means the assignment or one of the targets is already
	// in an active rollout.
	ErrRolloutActive = fmt.Errorf("%w: an active rollout already covers the assignment or a target", fleet.ErrConflict)
	// ErrRolloutState means the control action does not apply in the
	// rollout's current state.
	ErrRolloutState = fmt.Errorf("%w: the rollout's state does not allow this action", fleet.ErrConflict)
	// ErrTooManyTargets means more than MaxRolloutTargets agents match.
	ErrTooManyTargets = fmt.Errorf("%w: too many rollout targets", fleet.ErrCapacity)
)

// RolloutState is a rollout's lifecycle state.
type RolloutState string

const (
	Running     RolloutState = "running"
	Paused      RolloutState = "paused"
	RollingBack RolloutState = "rolling_back"
	Completed   RolloutState = "completed"
	RolledBack  RolloutState = "rolled_back"
	Cancelled   RolloutState = "cancelled"
)

// Active reports whether the rollout still owns its targets.
func (s RolloutState) Active() bool { return s == Running || s == Paused || s == RollingBack }

// Overriding reports whether released targets receive the rollout's version.
func (s RolloutState) Overriding() bool { return s == Running || s == Paused }

// Outcome is one target's result in a rollout.
type Outcome string

const (
	// OutcomePending: the target's wave is not released yet.
	OutcomePending Outcome = "pending"
	// OutcomeReleased: sent the new desired config, not yet applied.
	OutcomeReleased Outcome = "released"
	// OutcomeBaking: applied and healthy, waiting for the bake time.
	OutcomeBaking Outcome = "baking"
	// OutcomeOffline: released but not connected; it does not block its wave.
	OutcomeOffline Outcome = "offline"
	// OutcomeSucceeded: applied and healthy for the bake time.
	OutcomeSucceeded Outcome = "succeeded"
	// OutcomeFailed: reported FAILED for the desired config, or held after a local
	// rollback.
	OutcomeFailed Outcome = "failed"
	// OutcomeUnhealthy: applied the desired config but reported unhealthy.
	OutcomeUnhealthy Outcome = "unhealthy"
	// OutcomeTimedOut: still not applied when the wave timeout passed.
	OutcomeTimedOut Outcome = "timed_out"
	// OutcomeUnmatched: the target no longer receives any remote config.
	OutcomeUnmatched Outcome = "unmatched"
	// OutcomeIncompatible: the agent's build lacks a component type the
	// version uses. The target is never released and does not count against
	// the thresholds.
	OutcomeIncompatible Outcome = "incompatible"
)

// Outcomes lists every outcome, for validation and progress tables.
var Outcomes = []Outcome{OutcomePending, OutcomeReleased, OutcomeBaking, OutcomeOffline, OutcomeSucceeded, OutcomeFailed, OutcomeUnhealthy, OutcomeTimedOut, OutcomeUnmatched, OutcomeIncompatible}

// Settled reports whether the outcome is final for threshold accounting.
func (o Outcome) Settled() bool {
	return o == OutcomeSucceeded || o == OutcomeUnmatched || o.Failure()
}

// Failure reports whether the outcome counts against the thresholds.
func (o Outcome) Failure() bool {
	return o == OutcomeFailed || o == OutcomeUnhealthy || o == OutcomeTimedOut
}

// Wave is one cumulative wave boundary: a count of agents or a percentage
// of the rollout's targets. Exactly one is set.
type Wave struct {
	Count   int `json:"count,omitempty"`
	Percent int `json:"percent,omitempty"`
}

// Policy controls how a rollout advances.
type Policy struct {
	Waves              []Wave `json:"waves"`
	CanaryGroupID      string `json:"canary_group_id,omitempty"`
	BakeSeconds        int    `json:"bake_seconds"`
	WaveTimeoutSeconds int    `json:"wave_timeout_seconds"`
	MaxFailures        int    `json:"max_failures"`
	MaxFailurePercent  int    `json:"max_failure_percent"`
	OnFailure          string `json:"on_failure"`
}

// DefaultPolicy is [1 agent, 5 %, 25 %, 100 %], a 2-minute bake, a 15-minute
// wave timeout, no tolerated failure below 5 %, and pause on failure.
func DefaultPolicy() Policy {
	return Policy{Waves: []Wave{{Count: 1}, {Percent: 5}, {Percent: 25}, {Percent: 100}}, BakeSeconds: 120, WaveTimeoutSeconds: 900,
		MaxFailures: 0, MaxFailurePercent: 5, OnFailure: "pause"}
}

func (p Policy) Bake() time.Duration        { return time.Duration(p.BakeSeconds) * time.Second }
func (p Policy) WaveTimeout() time.Duration { return time.Duration(p.WaveTimeoutSeconds) * time.Second }

// Validate checks a policy's bounds.
func (p Policy) Validate(validID func(string) bool) error {
	if len(p.Waves) < 1 || len(p.Waves) > MaxWaves {
		return errors.New("waves")
	}
	for _, w := range p.Waves {
		if (w.Count > 0) == (w.Percent > 0) || w.Count < 0 || w.Count > MaxRolloutTargets || w.Percent < 0 || w.Percent > 100 {
			return errors.New("waves")
		}
	}
	switch {
	case p.CanaryGroupID != "" && !validID(p.CanaryGroupID):
		return errors.New("canary_group_id")
	case p.BakeSeconds < 0 || p.BakeSeconds > 86400:
		return errors.New("bake_seconds")
	case p.WaveTimeoutSeconds < 60 || p.WaveTimeoutSeconds > 86400:
		return errors.New("wave_timeout_seconds")
	case p.MaxFailures < 0 || p.MaxFailures > MaxRolloutTargets:
		return errors.New("max_failures")
	case p.MaxFailurePercent < 0 || p.MaxFailurePercent > 100:
		return errors.New("max_failure_percent")
	case p.OnFailure != "pause" && p.OnFailure != "rollback":
		return errors.New("on_failure")
	}
	return nil
}

// PlanWaves orders targets and assigns waves. Canary members form wave 0 on
// their own; the rest follow the policy's cumulative boundaries. Inside a
// wave, agents are ordered by hash(rollout ID, agent ID): stable and spread
// across hosts. Boundaries that add no agent are dropped and the last wave
// always takes the remainder. It returns agent IDs in release order, their
// waves and the wave count.
func PlanWaves(rolloutID string, agents []string, canary func(string) bool, waves []Wave) ([]string, []int, int) {
	key := func(id string) uint64 {
		h := sha256.Sum256([]byte(rolloutID + "\x00" + id))
		return binary.BigEndian.Uint64(h[:8])
	}
	var first, rest []string
	for _, id := range agents {
		if canary != nil && canary(id) {
			first = append(first, id)
		} else {
			rest = append(rest, id)
		}
	}
	byKey := func(a, b string) int {
		ka, kb := key(a), key(b)
		if ka != kb {
			if ka < kb {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	}
	slices.SortFunc(first, byKey)
	slices.SortFunc(rest, byKey)
	order := append(first, rest...)
	assigned := make([]int, len(order))
	count := 0
	if len(first) > 0 {
		count = 1
	}
	n, done := len(rest), 0
	for i, w := range waves {
		bound := w.Count
		if w.Percent > 0 {
			bound = (w.Percent*n + 99) / 100
		}
		if i == len(waves)-1 || bound > n {
			bound = n
		}
		if bound <= done {
			continue
		}
		for j := done; j < bound; j++ {
			assigned[len(first)+j] = count
		}
		done, count = bound, count+1
	}
	return order, assigned, count
}

// Rollout is the controller's view of one active rollout.
type Rollout struct {
	State       RolloutState
	CurrentWave int
	WaveCount   int
	// PhaseStartedAt is when the current wave was released or the rollback
	// began.
	PhaseStartedAt time.Time
	Policy         Policy
}

// Observation is one released target with its agent's current state.
type Observation struct {
	AgentID    string
	Wave       int
	Outcome    Outcome
	ObservedAt *time.Time
	Connected  bool
	// Desired is the agent's desired hash now; nil when it receives no
	// remote config.
	Desired []byte
	// Undeliverable marks an agent whose desired config cannot be built,
	// for example with more than MaxLayers layers.
	Undeliverable bool
	Status        Status
	Held          []byte
	// Healthy is nil for an agent that does not report health.
	Healthy *bool
	// Incompatible marks an agent whose reported build lacks a component
	// type of the rollout's version.
	Incompatible bool
}

// Tally counts settled targets and unaccepted failures.
type Tally struct {
	Settled, Failures int
}

func (t *Tally) add(o Outcome, accepted bool) {
	if o.Settled() {
		t.Settled++
	}
	if o.Failure() && !accepted {
		t.Failures++
	}
}

// Breached applies a threshold: failures above max(MaxFailures,
// MaxFailurePercent of settled).
func (p Policy) Breached(t Tally) bool {
	return t.Failures > max(p.MaxFailures, p.MaxFailurePercent*t.Settled/100)
}

// Action is the controller's next transition for a rollout.
type Action string

const (
	NoAction    Action = ""
	ReleaseWave Action = "release"
	Complete    Action = "complete"
	PauseAction Action = "pause"
	Rollback    Action = "rollback"
	FinishBack  Action = "rolled_back"
)

// TargetUpdate is a changed outcome.
type TargetUpdate struct {
	AgentID    string
	Outcome    Outcome
	ObservedAt *time.Time
}

// Step is one controller evaluation.
type Step struct {
	Updates []TargetUpdate
	Action  Action
}

// Observe returns a released target's outcome now. Settled outcomes are
// final. Only targets of the current wave time out; earlier ones stopped
// blocking when their wave advanced.
func Observe(r Rollout, o Observation, now time.Time) (Outcome, *time.Time) {
	if o.Outcome.Settled() {
		return o.Outcome, o.ObservedAt
	}
	if o.Incompatible {
		return OutcomeIncompatible, nil
	}
	if !o.Connected {
		return OutcomeOffline, nil
	}
	if o.Undeliverable {
		return OutcomeFailed, nil
	}
	if o.Desired == nil {
		return OutcomeUnmatched, nil
	}
	if bytes.Equal(o.Held, o.Desired) || o.Status.State == Failed && bytes.Equal(o.Status.Hash, o.Desired) {
		return OutcomeFailed, nil
	}
	if o.Status.State == Applied && bytes.Equal(o.Status.Hash, o.Desired) {
		if o.Healthy != nil && !*o.Healthy {
			return OutcomeUnhealthy, nil
		}
		since := now
		if o.ObservedAt != nil {
			since = *o.ObservedAt
		}
		if now.Sub(since) >= r.Policy.Bake() {
			return OutcomeSucceeded, &since
		}
		return OutcomeBaking, &since
	}
	if o.Wave == r.CurrentWave && now.Sub(r.PhaseStartedAt) >= r.Policy.WaveTimeout() {
		return OutcomeTimedOut, nil
	}
	return OutcomeReleased, nil
}

// Evaluate decides one controller step for a running or paused rollout.
// open holds released targets whose outcome is not settled; settled holds
// the tallies of settled targets per wave, which the caller reads in
// aggregate. A rollout that has not released its first wave starts it.
func Evaluate(r Rollout, open []Observation, settled map[int]Tally, now time.Time) Step {
	var step Step
	if r.State == Running && r.CurrentWave < 0 {
		if r.WaveCount == 0 {
			step.Action = Complete
		} else {
			step.Action = ReleaseWave
		}
		return step
	}
	waves := map[int]Tally{}
	var total Tally
	for w, t := range settled {
		waves[w] = t
		total.Settled += t.Settled
		total.Failures += t.Failures
	}
	waiting := false
	for _, o := range open {
		outcome, observed := Observe(r, o, now)
		if outcome != o.Outcome || !sameTime(observed, o.ObservedAt) {
			step.Updates = append(step.Updates, TargetUpdate{AgentID: o.AgentID, Outcome: outcome, ObservedAt: observed})
		}
		if outcome.Settled() {
			t := waves[o.Wave]
			t.add(outcome, false)
			waves[o.Wave] = t
			total.add(outcome, false)
		} else if o.Wave == r.CurrentWave && outcome != OutcomeOffline && outcome != OutcomeIncompatible {
			waiting = true
		}
	}
	if r.State != Running {
		return step
	}
	if r.Policy.Breached(waves[r.CurrentWave]) || r.Policy.Breached(total) {
		step.Action = PauseAction
		if r.Policy.OnFailure == "rollback" {
			step.Action = Rollback
		}
		return step
	}
	if waiting {
		return step
	}
	if r.CurrentWave >= r.WaveCount-1 {
		step.Action = Complete
	} else {
		step.Action = ReleaseWave
	}
	return step
}

// Reverted decides whether a rollback has finished: every connected
// released target reports its restored desired config as applied, or has
// failed it, or the wave timeout passed since the rollback began. targets
// holds every released target, observed with the rollout no longer
// overriding.
func Reverted(r Rollout, targets []Observation, now time.Time) bool {
	if now.Sub(r.PhaseStartedAt) >= r.Policy.WaveTimeout() {
		return true
	}
	for _, o := range targets {
		if !o.Connected || o.Desired == nil || bytes.Equal(o.Held, o.Desired) {
			continue
		}
		if bytes.Equal(o.Status.Hash, o.Desired) && (o.Status.State == Applied || o.Status.State == Failed) {
			continue
		}
		return false
	}
	return true
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// Control is an operator action on a rollout.
type Control string

const (
	ControlPause    Control = "pause"
	ControlResume   Control = "resume"
	ControlRollback Control = "rollback"
	ControlCancel   Control = "cancel"
)

// Next returns the state a control action leads to from s. Resume accepts
// the failures seen so far; cancel ends at once, and released targets return
// to the assignment's version without being observed.
func (c Control) Next(s RolloutState) (RolloutState, error) {
	switch {
	case c == ControlPause && s == Running:
		return Paused, nil
	case c == ControlResume && s == Paused:
		return Running, nil
	case c == ControlRollback && s.Overriding():
		return RollingBack, nil
	case c == ControlCancel && s.Active():
		return Cancelled, nil
	}
	return s, ErrRolloutState
}

// RolloutRequest starts a rollout of an assignment to ToVersion, or deletes
// the assignment's layer with Delete.
type RolloutRequest struct {
	ID           string `json:"id"`
	AssignmentID string `json:"assignment_id"`
	ToVersion    int    `json:"to_version,omitempty"`
	Delete       bool   `json:"delete,omitempty"`
	Policy
}

// Validate checks a request other than its assignment's existence.
func (r RolloutRequest) Validate() error {
	switch {
	case !fleet.ValidID(r.ID):
		return fleet.Invalid("id " + fleet.IDRule)
	case !fleet.ValidID(r.AssignmentID):
		return fleet.Invalid("assignment_id " + fleet.IDRule)
	case r.Delete == (r.ToVersion > 0) || r.ToVersion < 0:
		return fleet.Invalid("a rollout sets either a positive to_version or delete")
	}
	if err := r.Policy.Validate(fleet.ValidID); err != nil {
		return fleet.Invalid("policy " + err.Error() + " is out of range")
	}
	return nil
}

// RolloutView is a rollout with its progress. FromVersion is 0 for a rollout
// creating its assignment; ToVersion is 0 for one deleting it.
type RolloutView struct {
	ID           string         `json:"id"`
	AssignmentID string         `json:"assignment_id"`
	ConfigID     string         `json:"config_id"`
	FromVersion  int            `json:"from_version,omitempty"`
	ToVersion    int            `json:"to_version,omitempty"`
	State        RolloutState   `json:"state"`
	CurrentWave  int            `json:"current_wave"`
	WaveCount    int            `json:"wave_count"`
	Reason       string         `json:"reason,omitempty"`
	Policy       Policy         `json:"policy"`
	Targets      int            `json:"targets"`
	Waves        []WaveProgress `json:"waves,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	CreatedBy    string         `json:"created_by"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// WaveProgress counts one wave's targets by outcome.
type WaveProgress struct {
	Wave       int             `json:"wave"`
	Targets    int             `json:"targets"`
	ReleasedAt *time.Time      `json:"released_at,omitempty"`
	Outcomes   map[Outcome]int `json:"outcomes"`
}

type RolloutPage struct {
	Items     []RolloutView `json:"items"`
	NextAfter string        `json:"next_after,omitempty"`
}

// RolloutTarget is one agent's place and outcome in a rollout.
type RolloutTarget struct {
	AgentID    string     `json:"agent_id"`
	Wave       int        `json:"wave"`
	Outcome    Outcome    `json:"outcome"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Accepted   bool       `json:"accepted,omitempty"`
}

type RolloutTargetPage struct {
	Items     []RolloutTarget `json:"items"`
	NextAfter string          `json:"next_after,omitempty"`
}

// ValidOutcome reports whether o names an outcome.
func ValidOutcome(o Outcome) bool { return slices.Contains(Outcomes, o) }

// OutcomeList names the outcomes for an error message.
func OutcomeList() string {
	names := make([]string, len(Outcomes))
	for i, o := range Outcomes {
		names[i] = string(o)
	}
	return strings.Join(names, ", ")
}
