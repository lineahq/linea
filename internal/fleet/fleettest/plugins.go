// Package fleettest holds the contract suites that every fleet store must
// pass: the memory store always, the PostgreSQL store against a disposable
// database.
package fleettest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

// PluginSubject is one fresh, empty plugin lifecycle store under test.
type PluginSubject struct {
	Store fleet.PluginLifecycleRepository
	// Admin is an actor SetPluginLifecycle accepts.
	Admin string
	// Reader is an actor it must refuse with auth.ErrUnauthorized; empty
	// when the store knows no credentials.
	Reader string
	// Age makes the last probe of every plugin d older. The memory store
	// advances its clock; PostgreSQL moves seen_at back, since database time
	// cannot be set.
	Age func(d time.Duration)
}

const (
	instanceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	instanceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	staleID   = "00000000000000000000000000000000"
)

// RunPluginState runs the transition table of the plugin state design (T1 to
// T5) against newSubject's stores. The cases use only margins of 100 ms or
// more around the 500 ms and 3 s rules, so a real database clock passes them.
func RunPluginState(t *testing.T, newSubject func(t *testing.T) PluginSubject) {
	cases := []struct {
		name string
		run  func(t *testing.T, s PluginSubject)
	}{
		{"first probe inserts an enabled row at revision 0", func(t *testing.T, s PluginSubject) {
			if states := states(t, s); len(states) != 0 {
				t.Fatalf("fresh store has %d rows", len(states))
			}
			got := seen(t, s, "demo", "", instanceA)
			if got.ID != "demo" || got.Desired != "enabled" || got.Revision != 0 || got.InstanceID != instanceA || !pluginsdk.ValidUID(got.Epoch) {
				t.Fatalf("inserted row: %+v", got)
			}
			if !got.Seen() || got.SeenAge > 100*time.Millisecond || !got.Ready() {
				t.Fatalf("inserted row is not freshly ready: %+v", got)
			}
			items, err := s.Store.PluginLifecycles(context.Background())
			if err != nil || len(items) != 1 || items[0] != (fleet.PluginLifecycle{ID: "demo", Desired: "enabled"}) {
				t.Fatalf("lifecycles %+v %v", items, err)
			}
		}},
		{"T1 keeps the epoch and writes only when seen_at is older than 500 ms", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			s.Age(100 * time.Millisecond)
			same := seen(t, s, "demo", first.Epoch, instanceA)
			if same.Epoch != first.Epoch || same.SeenAge < 100*time.Millisecond {
				t.Fatalf("a probe inside 500 ms wrote: %+v", same)
			}
			s.Age(500 * time.Millisecond)
			again := seen(t, s, "demo", first.Epoch, instanceA)
			if again.Epoch != first.Epoch || again.SeenAge > 100*time.Millisecond {
				t.Fatalf("a probe after 600 ms did not refresh seen_at: %+v", again)
			}
		}},
		{"T2 a new instance rotates the epoch and is seen at once", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			s.Age(100 * time.Millisecond)
			second := seen(t, s, "demo", first.Epoch, instanceB)
			if second.Epoch == first.Epoch || !pluginsdk.ValidUID(second.Epoch) || second.InstanceID != instanceB || second.SeenAge > 50*time.Millisecond || !second.Ready() {
				t.Fatalf("restarted plugin: %+v", second)
			}
		}},
		{"a probe with a stale epoch changes nothing and returns the current row", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			second := seen(t, s, "demo", first.Epoch, instanceB)
			s.Age(time.Second)
			stale := seen(t, s, "demo", first.Epoch, instanceA)
			if stale.Epoch != second.Epoch || stale.InstanceID != instanceB || stale.SeenAge < time.Second {
				t.Fatalf("stale probe changed the row: %+v", stale)
			}
			unknown := seen(t, s, "demo", staleID, instanceA)
			if unknown.Epoch != second.Epoch || unknown.InstanceID != instanceB {
				t.Fatalf("unknown epoch changed the row: %+v", unknown)
			}
		}},
		{"T3 a refusal rotates at once, clears seen_at and keeps the instance", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			fenced := fence(t, s, "demo", first.Epoch, false)
			if fenced.Epoch == first.Epoch || !pluginsdk.ValidUID(fenced.Epoch) || fenced.Seen() || fenced.InstanceID != instanceA || fenced.Ready() {
				t.Fatalf("refused plugin: %+v", fenced)
			}
			// The next probe under the new epoch is the same instance: T1.
			back := seen(t, s, "demo", fenced.Epoch, instanceA)
			if back.Epoch != fenced.Epoch || !back.Seen() || !back.Ready() {
				t.Fatalf("plugin did not become ready under the new epoch: %+v", back)
			}
			// A second replica that also saw the refusal holds the old epoch.
			again := fence(t, s, "demo", first.Epoch, false)
			if again.Epoch != fenced.Epoch || !again.Seen() {
				t.Fatalf("a repeated refusal rotated again: %+v", again)
			}
		}},
		{"T3 and T4 on a missing row are not found", func(t *testing.T, s PluginSubject) {
			for _, unseenOnly := range []bool{false, true} {
				if _, err := s.Store.FencePlugin(context.Background(), "demo", staleID, unseenOnly); !errors.Is(err, fleet.ErrNotFound) {
					t.Fatalf("unseenOnly %v: %v", unseenOnly, err)
				}
			}
		}},
		{"T4 rotates only after 3 s unseen and not again while unseen", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			s.Age(2 * time.Second)
			if kept := fence(t, s, "demo", first.Epoch, true); kept.Epoch != first.Epoch || kept.SeenAge < 2*time.Second {
				t.Fatalf("a missed probe inside the window rotated: %+v", kept)
			}
			s.Age(2 * time.Second)
			rotated := fence(t, s, "demo", first.Epoch, true)
			if rotated.Epoch == first.Epoch || rotated.Seen() || rotated.Ready() {
				t.Fatalf("a plugin unseen for 4 s kept its epoch: %+v", rotated)
			}
			// Another replica's probe of the same outage: nothing left to fence.
			if again := fence(t, s, "demo", rotated.Epoch, true); again.Epoch != rotated.Epoch {
				t.Fatalf("an already fenced plugin rotated again: %+v", again)
			}
			// A replica still holding the old epoch adopts the new one.
			if stale := fence(t, s, "demo", first.Epoch, true); stale.Epoch != rotated.Epoch {
				t.Fatalf("stale fence changed the row: %+v", stale)
			}
		}},
		{"T4 does not rotate a plugin seen again inside the window", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			s.Age(4 * time.Second)
			seen(t, s, "demo", first.Epoch, instanceA)
			if kept := fence(t, s, "demo", first.Epoch, true); kept.Epoch != first.Epoch || !kept.Seen() {
				t.Fatalf("another replica's recent probe did not protect the epoch: %+v", kept)
			}
		}},
		{"T5 creates a row at revision 1 with a new epoch and no instance", func(t *testing.T, s PluginSubject) {
			item, err := s.Store.SetPluginLifecycle(context.Background(), s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "draining"}, 0)
			if err != nil || item.Revision != 1 || item.Desired != "draining" {
				t.Fatalf("create: %+v %v", item, err)
			}
			got := state(t, s, "demo")
			if got.Desired != "draining" || got.Revision != 1 || !pluginsdk.ValidUID(got.Epoch) || got.InstanceID != "" || got.Seen() || got.Ready() {
				t.Fatalf("created row: %+v", got)
			}
			// The first probe finds no instance, so it is a new one: T2.
			probe := seen(t, s, "demo", got.Epoch, instanceA)
			if probe.Epoch == got.Epoch || probe.InstanceID != instanceA || probe.Desired != "draining" || probe.Revision != 1 || probe.Ready() {
				t.Fatalf("probe of a draining plugin: %+v", probe)
			}
		}},
		{"T5 on a probe-created row rotates the epoch and clears seen_at", func(t *testing.T, s PluginSubject) {
			first := seen(t, s, "demo", "", instanceA)
			item, err := s.Store.SetPluginLifecycle(context.Background(), s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "disabled"}, 0)
			if err != nil || item.Revision != 1 {
				t.Fatalf("set: %+v %v", item, err)
			}
			got := state(t, s, "demo")
			if got.Epoch == first.Epoch || got.Seen() || got.Desired != "disabled" || got.Revision != 1 || got.InstanceID != instanceA || got.Ready() {
				t.Fatalf("row after lifecycle change: %+v", got)
			}
			// A prober still holding the old epoch writes nothing and adopts.
			stale := seen(t, s, "demo", first.Epoch, instanceA)
			if stale.Epoch != got.Epoch || stale.Seen() {
				t.Fatalf("stale probe overwrote a lifecycle change: %+v", stale)
			}
			if stale := fence(t, s, "demo", first.Epoch, false); stale.Epoch != got.Epoch || stale.Seen() {
				t.Fatalf("stale fence overwrote a lifecycle change: %+v", stale)
			}
			// Enabling again and probing under the adopted epoch makes it ready.
			if _, err := s.Store.SetPluginLifecycle(context.Background(), s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "enabled"}, 1); err != nil {
				t.Fatal(err)
			}
			now := state(t, s, "demo")
			ready := seen(t, s, "demo", now.Epoch, instanceA)
			if ready.Epoch != now.Epoch || ready.Revision != 2 || !ready.Ready() {
				t.Fatalf("enabled plugin: %+v", ready)
			}
		}},
		{"T5 refuses a stale revision, bad input and a reader", func(t *testing.T, s PluginSubject) {
			ctx := context.Background()
			if _, err := s.Store.SetPluginLifecycle(ctx, s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "draining"}, 3); !errors.Is(err, fleet.ErrPrecondition) {
				t.Fatalf("absent row at revision 3: %v", err)
			}
			if _, err := s.Store.SetPluginLifecycle(ctx, s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "draining"}, 0); err != nil {
				t.Fatal(err)
			}
			before := state(t, s, "demo")
			if _, err := s.Store.SetPluginLifecycle(ctx, s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "disabled"}, 0); !errors.Is(err, fleet.ErrPrecondition) {
				t.Fatalf("stale revision: %v", err)
			}
			for _, item := range []fleet.PluginLifecycle{{ID: "demo", Desired: "paused"}, {ID: "Demo", Desired: "enabled"}, {ID: "", Desired: "enabled"}} {
				if _, err := s.Store.SetPluginLifecycle(ctx, s.Admin, item, 1); !errors.Is(err, fleet.ErrInvalid) {
					t.Fatalf("%+v: %v", item, err)
				}
			}
			if _, err := s.Store.SetPluginLifecycle(ctx, s.Admin, fleet.PluginLifecycle{ID: "demo", Desired: "enabled"}, -1); !errors.Is(err, fleet.ErrInvalid) {
				t.Fatalf("negative revision: %v", err)
			}
			if s.Reader != "" {
				if _, err := s.Store.SetPluginLifecycle(ctx, s.Reader, fleet.PluginLifecycle{ID: "demo", Desired: "enabled"}, 1); !errors.Is(err, auth.ErrUnauthorized) {
					t.Fatalf("reader: %v", err)
				}
			}
			if after := state(t, s, "demo"); withoutAge(after) != withoutAge(before) {
				t.Fatalf("a refused change moved the row: %+v became %+v", before, after)
			}
		}},
		{"a probe refuses an invalid plugin ID or instance", func(t *testing.T, s PluginSubject) {
			for _, c := range [][2]string{{"Demo", instanceA}, {"demo", "short"}, {"demo", ""}, {"demo", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}} {
				if _, err := s.Store.SeenPlugin(context.Background(), c[0], "", c[1]); !errors.Is(err, fleet.ErrInvalid) {
					t.Fatalf("%v: %v", c, err)
				}
			}
			if _, err := s.Store.FencePlugin(context.Background(), "Demo", "", false); !errors.Is(err, fleet.ErrInvalid) {
				t.Fatalf("fence: %v", err)
			}
		}},
		{"states list every plugin by ID and the table is bounded", func(t *testing.T, s PluginSubject) {
			for i := 0; i < fleet.MaxPluginRows; i++ {
				seen(t, s, fmt.Sprintf("p-%03d", fleet.MaxPluginRows-1-i), "", instanceA)
			}
			list := states(t, s)
			if len(list) != fleet.MaxPluginRows || list[0].ID != "p-000" || list[len(list)-1].ID != fmt.Sprintf("p-%03d", fleet.MaxPluginRows-1) {
				t.Fatalf("listed %d rows from %s", len(list), list[0].ID)
			}
			if _, err := s.Store.SeenPlugin(context.Background(), "overflow", "", instanceA); !errors.Is(err, fleet.ErrCapacity) {
				t.Fatalf("probe past the bound: %v", err)
			}
			if _, err := s.Store.SetPluginLifecycle(context.Background(), s.Admin, fleet.PluginLifecycle{ID: "overflow", Desired: "enabled"}, 0); !errors.Is(err, fleet.ErrCapacity) {
				t.Fatalf("lifecycle past the bound: %v", err)
			}
			// A known plugin still changes at the bound.
			if _, err := s.Store.SetPluginLifecycle(context.Background(), s.Admin, fleet.PluginLifecycle{ID: "p-000", Desired: "disabled"}, 0); err != nil {
				t.Fatalf("change at the bound: %v", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

func withoutAge(s fleet.PluginState) fleet.PluginState {
	s.SeenAge = 0
	return s
}

func seen(t *testing.T, s PluginSubject, id, epoch, instance string) fleet.PluginState {
	t.Helper()
	got, err := s.Store.SeenPlugin(context.Background(), id, epoch, instance)
	if err != nil {
		t.Fatalf("SeenPlugin(%s, %s, %s): %v", id, epoch, instance, err)
	}
	return got
}

func fence(t *testing.T, s PluginSubject, id, epoch string, unseenOnly bool) fleet.PluginState {
	t.Helper()
	got, err := s.Store.FencePlugin(context.Background(), id, epoch, unseenOnly)
	if err != nil {
		t.Fatalf("FencePlugin(%s, %s, %v): %v", id, epoch, unseenOnly, err)
	}
	return got
}

func states(t *testing.T, s PluginSubject) []fleet.PluginState {
	t.Helper()
	got, err := s.Store.PluginStates(context.Background())
	if err != nil {
		t.Fatal("PluginStates:", err)
	}
	return got
}

func state(t *testing.T, s PluginSubject, id string) fleet.PluginState {
	t.Helper()
	for _, item := range states(t, s) {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("no row for %s", id)
	return fleet.PluginState{}
}
