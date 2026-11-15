package fleet_test

import (
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/fleettest"
)

// fakeClock is the memory store's injected time source.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestMemoryPluginStateContract(t *testing.T) {
	fleettest.RunPluginState(t, func(*testing.T) fleettest.PluginSubject {
		clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
		store := fleet.NewStore(4)
		store.SetClock(clock.Now)
		return fleettest.PluginSubject{Store: store, Admin: "admin", Age: clock.Advance}
	})
}

func TestMemoryPluginStateBoundaries(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	store := fleet.NewStore(4)
	store.SetClock(clock.Now)
	const instance = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	first, err := store.SeenPlugin(t.Context(), "demo", "", instance)
	if err != nil {
		t.Fatal(err)
	}
	// Readiness ends at exactly the window; the fence rule starts there, so
	// no instant is both not ready and unfenceable.
	clock.Advance(fleet.PluginSeenWindow - time.Millisecond)
	if got, _ := store.PluginStates(t.Context()); !got[0].Ready() {
		t.Fatal("not ready just inside the window")
	}
	if got, _ := store.FencePlugin(t.Context(), "demo", first.Epoch, true); got.Epoch != first.Epoch {
		t.Fatal("fenced just inside the window")
	}
	clock.Advance(time.Millisecond)
	if got, _ := store.PluginStates(t.Context()); got[0].Ready() {
		t.Fatal("ready at the window")
	}
	if got, _ := store.FencePlugin(t.Context(), "demo", first.Epoch, true); got.Epoch == first.Epoch {
		t.Fatal("not fenced at the window")
	}
}
