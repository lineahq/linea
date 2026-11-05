package plugins

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

func (r *Registry) RestoreLifecycle(ctx context.Context, store fleet.PluginLifecycleRepository) error {
	r.lifecycle = store
	if store == nil {
		return nil
	}
	items, err := store.PluginLifecycles(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if e := r.find(item.ID); e != nil {
			e.mu.Lock()
			e.info.Desired, e.info.Revision = item.Desired, item.Revision
			if !e.config.Enabled {
				e.info.Desired = "disabled"
			}
			if e.info.Desired != "enabled" {
				e.info.State = e.info.Desired
			}
			e.mu.Unlock()
		}
	}
	return nil
}

// fenceLocked cancels admitted plugin calls and publishes a new host epoch.
func (r *Registry) fenceLocked(e *entry) {
	e.endLease()
	e.lease, e.endLease = context.WithCancel(r.ctx)
	e.info.Epoch = pluginsdk.RandomID()
}

func (r *Registry) Lifecycle(ctx context.Context, id, action string, caller auth.Principal, expected int64) (Info, error) {
	if r.ctx.Err() != nil {
		return Info{}, fleet.ErrUnavailable
	}
	if caller.Role != "admin" {
		return Info{}, auth.ErrForbidden
	}
	e := r.find(id)
	if e == nil {
		return Info{}, fleet.ErrNotFound
	}
	if r.lifecycle == nil {
		return Info{}, fleet.ErrUnavailable
	}
	desired := map[string]string{"enable": "enabled", "drain": "draining", "disable": "disabled"}[action]
	if desired == "" {
		return Info{}, fleet.ErrInvalid
	}
	if desired == "enabled" && !e.config.Enabled {
		return Info{}, auth.ErrForbidden
	}
	select {
	case e.work <- struct{}{}:
		defer func() { <-e.work }()
	case <-ctx.Done():
		return Info{}, fleet.ErrUnavailable
	}
	item, err := r.lifecycle.SetPluginLifecycle(ctx, caller.ID, fleet.PluginLifecycle{ID: id, Desired: desired}, expected)
	if err != nil {
		return Info{}, err
	}
	e.mu.Lock()
	r.fenceLocked(e)
	e.info.Desired, e.info.State, e.info.Revision = desired, desired, item.Revision
	if desired == "enabled" {
		e.info.State = "unavailable"
	}
	e.info.CheckedAt = nil
	e.mu.Unlock()
	return e.snapshot(), nil
}

func hostJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
