package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/lineahq/linea/pluginsdk"
)

// Plugin state lives in one shared row per plugin (fleet.PluginState): the
// row is the truth, probes write evidence, everything else reads the row.
// Each entry caches its row. Every read or write of the row, together with
// adopting its result, holds the entry's row slot, so an older read never
// overwrites a newer one.

// sweepInterval is how often every row is read again, behind the
// fleet_plugin hint.
const sweepInterval = 500 * time.Millisecond

// Local probe results of this replica under the current epoch.
const (
	localUnknown      = ""
	localOK           = "ok"
	localFailed       = "failed"
	localIncompatible = "incompatible"
)

// readyLocked is the forwarding rule: enabled, an epoch and instance known,
// this replica's manifest check not refused, and either the row's seen time
// or this replica's own last good probe within the seen window.
func (e *entry) readyLocked(now time.Time) bool {
	if e.info.Desired != "enabled" || e.info.Epoch == "" || e.info.InstanceID == "" || e.local == localIncompatible {
		return false
	}
	recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) < fleet.PluginSeenWindow }
	return recent(e.seenAt) || recent(e.goodAt)
}

func (r *Registry) ready(e *entry) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.readyLocked(r.now())
}

func (e *entry) stateLocked(now time.Time) string {
	switch {
	case e.info.Desired != "enabled":
		return e.info.Desired
	case e.local == localIncompatible:
		return localIncompatible
	case e.readyLocked(now):
		return "ready"
	}
	return "unavailable"
}

// restateLocked publishes the current state and logs a change.
func (r *Registry) restateLocked(e *entry, why translog.Reason, actor string) {
	state := e.stateLocked(r.now())
	if state == e.info.State {
		return
	}
	e.info.State = state
	attrs := []translog.Attr{translog.ID(translog.KeyPlugin, e.config.ID), translog.Now(stateOf(state)), translog.Why(why)}
	if actor != "" {
		attrs = append(attrs, translog.ID(translog.KeyActor, actor))
	}
	r.log.Info(translog.PluginStateChanged, attrs...)
}

// adoptLocked loads a row read at readAt into the entry. A new epoch cancels
// every admitted call and makes this replica's own probe result unknown, so
// it never acts twice on evidence another replica already acted on.
func (r *Registry) adoptLocked(e *entry, s fleet.PluginState, readAt time.Time, why translog.Reason) {
	if s.Epoch != e.info.Epoch {
		e.endLease()
		e.lease, e.endLease = context.WithCancel(r.ctx)
		e.goodAt = time.Time{}
		if e.local != localIncompatible {
			e.local = localUnknown
		}
		if e.info.Epoch != "" {
			r.log.Info(translog.PluginEpochChanged, translog.ID(translog.KeyPlugin, e.config.ID), translog.Why(why))
		}
	}
	e.info.Desired, e.info.Revision, e.info.Epoch, e.info.InstanceID = s.Desired, s.Revision, s.Epoch, s.InstanceID
	if !e.config.Enabled {
		e.info.Desired = "disabled"
	}
	e.seenAt = time.Time{}
	if s.Seen() {
		e.seenAt = readAt.Add(-s.SeenAge)
	}
}

// holdRow takes the entry's row slot.
func (e *entry) holdRow(ctx context.Context) bool {
	select {
	case e.row <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *entry) releaseRow() { <-e.row }

// missingRow is the state of a plugin no replica has written yet: enabled,
// with no epoch, so it is not ready until a probe inserts the row.
func missingRow(id string) fleet.PluginState {
	return fleet.PluginState{ID: id, Desired: "enabled", SeenAge: -1}
}

func findRow(states []fleet.PluginState, id string) fleet.PluginState {
	for _, s := range states {
		if s.ID == id {
			return s
		}
	}
	return missingRow(id)
}

// Load reads every row once before the registry serves. A store that keeps
// no plugin state leaves every plugin unready.
func (r *Registry) Load(ctx context.Context) error {
	return r.sync(ctx, translog.ReasonRestored)
}

// sync reads every row and adopts it.
func (r *Registry) sync(ctx context.Context, why translog.Reason) error {
	if r.lifecycle == nil || len(r.entries) == 0 {
		return nil
	}
	held := 0
	defer func() {
		for _, e := range r.entries[:held] {
			e.releaseRow()
		}
	}()
	for _, e := range r.entries {
		if !e.holdRow(ctx) {
			return ctx.Err()
		}
		held++
	}
	readAt := r.now()
	states, err := r.lifecycle.PluginStates(ctx)
	if err != nil {
		return err
	}
	for _, e := range r.entries {
		s := findRow(states, e.config.ID)
		e.mu.Lock()
		reason := why
		if why == translog.ReasonSharedState && s.Epoch == e.info.Epoch && s.Desired == e.info.Desired {
			// Only time passed: the seen window closed or another replica's
			// probe kept it open.
			reason = translog.ReasonProbeLost
			if s.Ready() {
				reason = translog.ReasonProbeOK
			}
		}
		r.adoptLocked(e, s, readAt, reason)
		r.restateLocked(e, reason, "")
		e.mu.Unlock()
	}
	return nil
}

// refresh reloads one plugin's row after the plugin answered 421 or sent an
// epoch this replica does not know. Callers never act on a refreshed
// authority within the same request: they answer 503 and the caller retries,
// or compare once more for host requests.
func (r *Registry) refresh(e *entry) {
	if r.lifecycle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, pluginsdk.RequestTimeout)
	defer cancel()
	if !e.holdRow(ctx) {
		return
	}
	defer e.releaseRow()
	readAt := r.now()
	states, err := r.lifecycle.PluginStates(ctx)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r.adoptLocked(e, findRow(states, e.config.ID), readAt, translog.ReasonSharedState)
	r.restateLocked(e, translog.ReasonSharedState, "")
}

// Notify is the fleet_plugin hint: some row changed, so read them all soon.
// It never blocks; the sweep reads the rows anyway.
func (r *Registry) Notify(string) {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// sweep reads every row on each hint and at least every sweepInterval.
func (r *Registry) sweep() {
	defer r.wg.Done()
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
		ctx, cancel := context.WithTimeout(r.ctx, pluginsdk.RequestTimeout)
		_ = r.sync(ctx, translog.ReasonSharedState)
		cancel()
	}
}

// manifestMatches reports whether the plugin is the registered one.
func (e *entry) manifestMatches(m pluginsdk.Manifest) bool {
	return m.ID == e.config.ID && m.Version == e.config.Version && m.ProtocolVersion == pluginsdk.ProtocolVersion && pluginsdk.ValidUID(m.InstanceID) && m.APIVersion == e.config.APIVersion
}

// probe checks the manifest and writes what it saw into the row: T1 and T2
// on a good answer, T3 when the plugin refused, T4 when it did not answer,
// and nothing for a manifest that does not match this replica's
// registration (T3a), which only stops this replica forwarding.
func (r *Registry) probe(e *entry) {
	e.mu.RLock()
	desired := e.info.Desired
	e.mu.RUnlock()
	if desired != "enabled" || r.lifecycle == nil {
		return
	}
	var m pluginsdk.Manifest
	err := r.send(r.ctx, e, "GET", pluginsdk.ManifestPath, nil, nil, 4096, &m)
	if errors.Is(err, errSlotsFull) {
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, pluginsdk.RequestTimeout)
	defer cancel()
	if !e.holdRow(ctx) {
		return
	}
	defer e.releaseRow()
	e.mu.RLock()
	epoch, instance, seen := e.info.Epoch, e.info.InstanceID, !e.seenAt.IsZero()
	e.mu.RUnlock()
	local, why := localOK, translog.ReasonProbeOK
	var failure *pluginsdk.HTTPError
	var state fleet.PluginState
	var werr error
	readAt := r.now()
	switch {
	case errors.As(err, &failure) && !seen:
		// Nothing to fence: the epoch was never seen, or a refusal
		// already rotated it.
		local, why, werr = localFailed, translog.ReasonProbeFailed, errNoWrite
	case errors.As(err, &failure):
		local, why = localFailed, translog.ReasonProbeFailed
		state, werr = r.lifecycle.FencePlugin(ctx, e.config.ID, epoch, false)
	case err != nil:
		local, why = localFailed, translog.ReasonProbeLost
		state, werr = r.lifecycle.FencePlugin(ctx, e.config.ID, epoch, true)
	case !e.manifestMatches(m):
		local, why, werr = localIncompatible, translog.ReasonProbeIncompatible, errNoWrite
	default:
		state, werr = r.lifecycle.SeenPlugin(ctx, e.config.ID, epoch, m.InstanceID)
		if instance != "" && instance != m.InstanceID {
			why = translog.ReasonInstanceChanged
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if werr == nil {
		epochWhy := why
		if why == translog.ReasonProbeOK {
			epochWhy = translog.ReasonSharedState
		}
		r.adoptLocked(e, state, readAt, epochWhy)
	}
	now := r.now()
	switch {
	case local == localOK && e.info.InstanceID == m.InstanceID:
		e.local, e.goodAt = localOK, now
	case local == localOK:
		// The row names another instance; this answer is no evidence for it.
		if e.local == localIncompatible {
			e.local = localUnknown
		}
	default:
		e.local = local
	}
	e.info.CheckedAt = &now
	r.restateLocked(e, why, "")
}

// errNoWrite marks a probe result that writes nothing.
var errNoWrite = errors.New("no write")

// Lifecycle is T5: it changes the desired state in the row, which rotates the
// epoch, and adopts the result at once on this replica.
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
	if !e.holdRow(ctx) {
		return Info{}, fleet.ErrUnavailable
	}
	defer e.releaseRow()
	item, err := r.lifecycle.SetPluginLifecycle(ctx, caller.ID, fleet.PluginLifecycle{ID: id, Desired: desired}, expected)
	if err != nil {
		return Info{}, err
	}
	why := map[string]translog.Reason{"enabled": translog.ReasonLifecycleEnable, "draining": translog.ReasonLifecycleDrain, "disabled": translog.ReasonLifecycleDisable}[desired]
	readAt := r.now()
	// The epoch rotated. Without a read of it this replica knows no epoch
	// and stays unready until the sweep reads the row.
	state := fleet.PluginState{ID: id, Desired: desired, Revision: item.Revision, SeenAge: -1}
	if states, err := r.lifecycle.PluginStates(ctx); err == nil {
		state = findRow(states, id)
	}
	e.mu.Lock()
	if state.Epoch == e.info.Epoch {
		// A read that lost the race with another change still must not keep
		// admitted calls of the old desired state.
		state.Epoch = ""
	}
	r.adoptLocked(e, state, readAt, why)
	r.restateLocked(e, why, caller.ID)
	e.mu.Unlock()
	return r.view(e), nil
}

func hostJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
