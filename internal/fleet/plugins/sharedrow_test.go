package plugins

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"

	"github.com/lineahq/linea/internal/fleet/translog"
)

// markReady records a good probe of instance in the shared row and adopts it,
// as a probe of a matching manifest would.
func markReady(t *testing.T, r *Registry, e *entry, instance string) {
	t.Helper()
	e.mu.RLock()
	epoch := e.info.Epoch
	e.mu.RUnlock()
	state, err := r.lifecycle.SeenPlugin(context.Background(), e.config.ID, epoch, instance)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r.adoptLocked(e, state, r.now(), translog.ReasonProbeOK)
	e.local, e.goodAt = localOK, r.now()
	r.restateLocked(e, translog.ReasonProbeOK, "")
}

// fakeClock is one clock shared by registries and the memory store.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// useClock puts the store and every registry on one fake clock.
func useClock(store *fleet.Store, registries ...*Registry) *fakeClock {
	clock := &fakeClock{t: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	store.SetClock(clock.now)
	for _, r := range registries {
		r.now = clock.now
	}
	return clock
}
