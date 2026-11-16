package fleet

import (
	"context"
	"sort"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

// pluginRow is the memory twin of a fleet.plugin_lifecycle row.
type pluginRow struct {
	desired    string
	revision   int64
	epoch      string
	instanceID string
	seenAt     time.Time
	seen       bool
}

// SetClock replaces the clock that times plugin state, for tests. The
// durable store uses database time instead.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = now
}

func (s *Store) pluginState(id string, r *pluginRow) PluginState {
	age := time.Duration(-1)
	if r.seen {
		age = max(s.clock().Sub(r.seenAt), 0)
	}
	return PluginState{ID: id, Desired: r.desired, Epoch: r.epoch, InstanceID: r.instanceID, Revision: r.revision, SeenAge: age}
}

func (s *Store) PluginLifecycles(context.Context) ([]PluginLifecycle, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]PluginLifecycle, 0, len(s.plugins))
	for id, r := range s.plugins {
		items = append(items, PluginLifecycle{ID: id, Desired: r.desired, Revision: r.revision})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (s *Store) PluginStates(context.Context) ([]PluginState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]PluginState, 0, len(s.plugins))
	for id, r := range s.plugins {
		items = append(items, s.pluginState(id, r))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

// SetPluginLifecycle ignores the actor: the memory store knows no credentials,
// and the registry checks the caller's role before it asks.
func (s *Store) SetPluginLifecycle(_ context.Context, _ string, item PluginLifecycle, expected int64) (PluginLifecycle, error) {
	if err := ValidPluginLifecycle(item, expected); err != nil {
		return item, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.plugins[item.ID]
	var current int64
	if r != nil {
		current = r.revision
	}
	if current != expected {
		return item, ErrPrecondition
	}
	if r == nil {
		if len(s.plugins) >= MaxPluginRows {
			return item, ErrCapacity
		}
		r = &pluginRow{}
		s.plugins[item.ID] = r
	}
	r.desired, r.revision, r.epoch, r.seen = item.Desired, current+1, pluginsdk.RandomID(), false
	item.Revision = r.revision
	return item, nil
}

func (s *Store) SeenPlugin(_ context.Context, id, epoch, instance string) (PluginState, error) {
	if !pluginsdk.ValidID(id) || !pluginsdk.ValidUID(instance) {
		return PluginState{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock()
	r := s.plugins[id]
	if r == nil {
		if len(s.plugins) >= MaxPluginRows {
			return PluginState{}, ErrCapacity
		}
		r = &pluginRow{desired: "enabled", epoch: pluginsdk.RandomID(), instanceID: instance, seenAt: now, seen: true}
		s.plugins[id] = r
		return s.pluginState(id, r), nil
	}
	if r.epoch != epoch {
		return s.pluginState(id, r), nil
	}
	switch {
	case r.instanceID != instance:
		r.instanceID, r.epoch, r.seenAt, r.seen = instance, pluginsdk.RandomID(), now, true
	case !r.seen || now.Sub(r.seenAt) > PluginSeenRefresh:
		r.seenAt, r.seen = now, true
	}
	return s.pluginState(id, r), nil
}

func (s *Store) FencePlugin(_ context.Context, id, epoch string, unseenOnly bool) (PluginState, error) {
	if !pluginsdk.ValidID(id) {
		return PluginState{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.plugins[id]
	if r == nil {
		return PluginState{}, ErrNotFound
	}
	if r.epoch == epoch && (!unseenOnly || r.seen && s.clock().Sub(r.seenAt) >= PluginSeenWindow) {
		r.epoch, r.seen = pluginsdk.RandomID(), false
	}
	return s.pluginState(id, r), nil
}
