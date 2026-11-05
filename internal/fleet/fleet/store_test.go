package fleet

import (
	"errors"
	"testing"
	"time"
)

func TestInventoryFencingAndOmittedHealth(t *testing.T) {
	s := NewStore(1)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	id := "00000000000000000000000000000001"
	h := true
	g, err := s.Connect(id, Report{Capabilities: 7, Healthy: &h}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Connect("00000000000000000000000000000002", Report{}, now); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if err := s.Observe(id, g, Report{}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Get(id)
	if a.Capabilities != 0 || a.Health == nil || !a.Health.Healthy || !a.Health.ObservedAt.Equal(now) {
		t.Fatalf("omission/replacement: %+v", a)
	}
	s.Seen(id, g, now.Add(2*time.Second))
	a.Health.Healthy = false
	a.LastSeenAt = nil
	a, _ = s.Get(id)
	if !a.Health.Healthy || a.LastSeenAt == nil || !a.LastSeenAt.Equal(now.Add(2*time.Second)) || a.LastReportAt == nil {
		t.Fatal("caller mutated stored observation or heartbeat lost")
	}
	// The newest connection wins and fences the older generation.
	newG, err := s.Connect(id, Report{}, now)
	if err != nil || newG != g+1 {
		t.Fatalf("takeover: generation=%d err=%v", newG, err)
	}
	s.Disconnect(id, g, now)
	if err := s.Observe(id, g, Report{Capabilities: 99, Healthy: &h}, now); !errors.Is(err, ErrStale) {
		t.Fatalf("old observation: %v", err)
	}
	a, _ = s.Get(id)
	if !a.Connected || a.Generation != newG || a.Capabilities != 0 || a.IdentityConflict {
		t.Fatal("old callback overwrote takeover")
	}
	for range ConflictTakeovers - 1 {
		if _, err := s.Connect(id, Report{}, now); err != nil {
			t.Fatal(err)
		}
	}
	if a, _ = s.Get(id); !a.IdentityConflict {
		t.Fatal("repeated takeovers were not flagged")
	}
}

func TestInventoryPagination(t *testing.T) {
	s := NewStore(3)
	for _, id := range []string{"00000000000000000000000000000003", "00000000000000000000000000000001", "00000000000000000000000000000002"} {
		if _, err := s.Connect(id, Report{}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	p := s.List("", 2, nil)
	if len(p.Items) != 2 || p.Items[0].ID != "00000000000000000000000000000001" || p.NextAfter != p.Items[1].ID {
		t.Fatalf("first page: %+v", p)
	}
	q := s.List(p.NextAfter, 2, nil)
	if len(q.Items) != 1 || q.Items[0].ID != "00000000000000000000000000000003" || q.NextAfter != "" {
		t.Fatalf("next page: %+v", q)
	}
	s.Disconnect(q.Items[0].ID, 1, time.Now())
	connected := false
	p = s.List("", 2, &connected)
	if len(p.Items) != 1 || p.Items[0].ID != q.Items[0].ID {
		t.Fatal("connected filter failed")
	}
}
