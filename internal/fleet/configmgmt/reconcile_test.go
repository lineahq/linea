package configmgmt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeSource struct {
	targets []Target
	bodies  map[int]string
	reads   atomic.Int64
}

func (f *fakeSource) Targets(context.Context) ([]Target, error) { return f.targets, nil }
func (f *fakeSource) ManagedCredentials(context.Context) (map[string]map[string]string, error) {
	return map[string]map[string]string{"cred": {"env": "prod"}}, nil
}
func (f *fakeSource) GetVersion(_ context.Context, id string, v int, _ bool) (Version, error) {
	f.reads.Add(1)
	body := f.bodies[v]
	return Version{ConfigID: id, Version: v, Body: &body}, nil
}

func target(t *testing.T, name string, layer, version int, body string, labels map[string]string) Target {
	t.Helper()
	d := sha256.Sum256([]byte(body))
	tg, err := NewTarget(Assignment{ID: name, Name: name, ConfigID: "c", Version: version, Layer: layer, CredentialLabels: labels}, d[:], "", []string{"agent"})
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestReconcileUsesStoredDigestsAndCachesBodies(t *testing.T) {
	src := &fakeSource{bodies: map[int]string{1: "a: 1\n", 2: "b: 2\n"}}
	src.targets = []Target{target(t, "late", 20, 2, src.bodies[2], nil), target(t, "base", 10, 1, src.bodies[1], map[string]string{"env": "prod"})}
	r := NewReconciler(src, 1<<10)
	s := Session{AgentID: "agent", Credential: "cred", Capabilities: AcceptsRemoteConfig}
	if _, err := r.Reconcile(context.Background(), s); !errors.Is(err, ErrNotLoaded) {
		t.Fatal("reconciled before loading desired state")
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := r.Reconcile(context.Background(), s)
	plan, _, _ := Desired([]Layer{{10, "base", []byte(src.bodies[1])}, {20, "late", []byte(src.bodies[2])}})
	if err != nil || !d.Send || !bytes.Equal(d.Plan.Hash, plan.Hash) || len(d.Plan.Entries) != 2 || d.Plan.Entries[0].Name != "10-base" {
		t.Fatalf("decision %+v err %v", d, err)
	}
	// Concurrent sends share cached bodies.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.Reconcile(context.Background(), s) }()
	}
	wg.Wait()
	if n := src.reads.Load(); n != 2 {
		t.Fatalf("bodies read %d times", n)
	}
	// In sync: no body is read.
	s.Status = Status{Hash: plan.Hash, State: Applied}
	if d, _ := r.Reconcile(context.Background(), s); d.Send || src.reads.Load() != 2 {
		t.Fatal("in-sync agent read bodies or was sent config")
	}
	// Unknown credentials and agents without the capability get nothing.
	if d, _ := r.Reconcile(context.Background(), Session{AgentID: "agent", Credential: "other", Capabilities: AcceptsRemoteConfig}); d.Send {
		t.Fatal("sent without credential opt-in")
	}
	if d, _ := r.Reconcile(context.Background(), Session{AgentID: "agent", Credential: "cred"}); d.Send {
		t.Fatal("sent without AcceptsRemoteConfig")
	}
}

func TestReconcileRejectsBodyDigestMismatch(t *testing.T) {
	src := &fakeSource{bodies: map[int]string{1: "changed: true\n"}}
	src.targets = []Target{target(t, "base", 10, 1, "original: true\n", nil)}
	r := NewReconciler(src, 1<<10)
	_ = r.Refresh(context.Background())
	if d, err := r.Reconcile(context.Background(), Session{AgentID: "agent", Credential: "cred", Capabilities: AcceptsRemoteConfig}); err == nil || d.Send {
		t.Fatal("sent a body that does not match its digest")
	}
}

func TestBodyCacheIsByteBounded(t *testing.T) {
	c := newBodyCache(10)
	fetch := func(b string) func() ([]byte, error) { return func() ([]byte, error) { return []byte(b), nil } }
	_, _ = c.get(versionKey{"a", 1}, fetch("12345"))
	_, _ = c.get(versionKey{"a", 2}, fetch("12345"))
	_, _ = c.get(versionKey{"a", 1}, fetch("x")) // hit: a/1 becomes most recent
	_, _ = c.get(versionKey{"a", 3}, fetch("123"))
	if c.size() > 10 {
		t.Fatalf("cache holds %d bytes", c.size())
	}
	if _, ok := c.entries[versionKey{"a", 2}]; ok {
		t.Fatal("least recently used body kept")
	}
	if b, _ := c.get(versionKey{"a", 1}, fetch("x")); string(b) != "12345" {
		t.Fatal("recent body evicted")
	}
	// A body larger than the cache is returned but not kept.
	if b, _ := c.get(versionKey{"a", 4}, fetch("01234567890")); len(b) != 11 || c.size() > 10 {
		t.Fatal("oversized body cached")
	}
}
