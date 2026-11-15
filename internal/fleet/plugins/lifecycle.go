package plugins

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
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
				r.log.Info(translog.PluginStateChanged, translog.ID(translog.KeyPlugin, e.config.ID), translog.Now(stateOf(e.info.State)), translog.Why(translog.ReasonRestored))
			}
			e.mu.Unlock()
		}
	}
	return nil
}

// refresh reloads the plugin's state after the plugin answered 421: it saw an
// instance or epoch other than this entry's. Today the entry is the only
// state, so reloading it is a read. When plugin state is shared between core
// replicas this is the one place that loads the shared row into the entry and
// adopts a newer epoch (cancelling the lease as fenceLocked does). Callers
// never act on the result: they answer 503 and the caller retries.
func (r *Registry) refresh(e *entry) Info { return e.snapshot() }

// fenceLocked cancels admitted plugin calls and publishes a new host epoch.
func (r *Registry) fenceLocked(e *entry, why translog.Reason) {
	e.endLease()
	e.lease, e.endLease = context.WithCancel(r.ctx)
	e.info.Epoch = pluginsdk.RandomID()
	r.log.Info(translog.PluginEpochChanged, translog.ID(translog.KeyPlugin, e.config.ID), translog.Why(why))
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
	why := map[string]translog.Reason{"enabled": translog.ReasonLifecycleEnable, "draining": translog.ReasonLifecycleDrain, "disabled": translog.ReasonLifecycleDisable}[desired]
	r.fenceLocked(e, why)
	e.info.Desired, e.info.State, e.info.Revision = desired, desired, item.Revision
	if desired == "enabled" {
		e.info.State = "unavailable"
	}
	r.log.Info(translog.PluginStateChanged, translog.ID(translog.KeyPlugin, id), translog.Now(stateOf(e.info.State)), translog.Why(why), translog.ID(translog.KeyActor, caller.ID))
	e.info.CheckedAt = nil
	e.mu.Unlock()
	return e.snapshot(), nil
}

func hostJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
