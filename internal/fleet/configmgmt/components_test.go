package configmgmt

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestComponentUsesAndMissing(t *testing.T) {
	uses := Uses([]byte("receivers:\n  otlp: {}\n  otlp/2: {}\n  filelog/app: {}\nprocessors: {batch: {}}\nexporters: {debug: {}}\nservice: {pipelines: {}}\n"))
	want := Components{"receivers": {"filelog", "otlp"}, "processors": {"batch"}, "exporters": {"debug"}}
	if !reflect.DeepEqual(uses, want) {
		t.Fatalf("uses %v", uses)
	}
	if got := Uses([]byte("{}\n")); len(got) != 0 {
		t.Fatalf("empty layer uses %v", got)
	}
	available := NewComponents(map[string][]string{"receivers": {"otlp", "otlp", "bad type"}, "exporters": {"debug"}, "unknown": {"x"}})
	if !reflect.DeepEqual(available, Components{"receivers": {"otlp"}, "exporters": {"debug"}}) {
		t.Fatalf("normalized %v", available)
	}
	if missing := available.Missing(uses); !reflect.DeepEqual(missing, []string{"processor:batch", "receiver:filelog"}) {
		t.Fatalf("missing %v", missing)
	}
	if missing := available.Merge(Components{"receivers": {"filelog"}, "processors": {"batch"}}).Missing(uses); len(missing) != 0 {
		t.Fatalf("merged set still misses %v", missing)
	}
	// The stored form round-trips; oversized or empty reports skip the check.
	if got := ParseComponents(available.JSON()); !reflect.DeepEqual(got, available) {
		t.Fatalf("round trip %v", got)
	}
	huge := make([]string, MaxComponentTypes+1)
	for i := range huge {
		huge[i] = fmt.Sprintf("r%d", i)
	}
	if NewComponents(map[string][]string{"receivers": huge}) != nil || NewComponents(map[string][]string{"unknown": {"x"}}) != nil {
		t.Fatal("unusable report produced a set")
	}
}

func TestReconcileWithholdsIncompatibleConfig(t *testing.T) {
	src := &fakeSource{bodies: map[int]string{1: "receivers: {otlp: {}}\n", 2: "exporters: {kafka: {}}\n"}}
	src.targets = []Target{target(t, "base", 10, 1, src.bodies[1], nil), target(t, "late", 20, 2, src.bodies[2], nil)}
	r := NewReconciler(src, 1<<10)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := Session{AgentID: "agent", Credential: "cred", Capabilities: AcceptsRemoteConfig}
	// Without a reported build the check is skipped.
	if d, err := r.Reconcile(context.Background(), s); err != nil || !d.Send {
		t.Fatalf("unknown build: %+v %v", d, err)
	}
	s.Components = Components{"receivers": {"otlp"}, "exporters": {"debug"}}
	d, err := r.Reconcile(context.Background(), s)
	if err != nil || d.Send || len(d.Plan.Entries) != 0 || !reflect.DeepEqual(d.Missing, []string{"exporter:kafka"}) {
		t.Fatalf("incompatible build: %+v %v", d, err)
	}
	s.Components["exporters"] = []string{"debug", "kafka"}
	if d, err := r.Reconcile(context.Background(), s); err != nil || !d.Send || d.Missing != nil {
		t.Fatalf("compatible build: %+v %v", d, err)
	}
}

func TestIncompatibleTargetsDoNotBlockWaves(t *testing.T) {
	now := time.Now()
	r := Rollout{State: Running, CurrentWave: 0, WaveCount: 2, PhaseStartedAt: now, Policy: DefaultPolicy()}
	open := []Observation{{AgentID: "a", Wave: 0, Outcome: OutcomeReleased, Connected: true, Incompatible: true}}
	step := Evaluate(r, open, nil, now)
	if len(step.Updates) != 1 || step.Updates[0].Outcome != OutcomeIncompatible || step.Action != ReleaseWave {
		t.Fatalf("step %+v", step)
	}
	if OutcomeIncompatible.Settled() || OutcomeIncompatible.Failure() || !ValidOutcome(OutcomeIncompatible) {
		t.Fatal("incompatible must be neither settled for thresholds nor a failure")
	}
}
