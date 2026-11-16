package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/lineahq/linea/pluginsdk"
	"github.com/lineahq/linea/pluginsdk/metrics"
)

type Info struct {
	ID              string     `json:"id"`
	Version         string     `json:"version"`
	ProtocolVersion string     `json:"protocol_version"`
	Enabled         bool       `json:"enabled"`
	Permissions     []string   `json:"permissions"`
	State           string     `json:"state"`
	CheckedAt       *time.Time `json:"checked_at,omitempty"`
	InstanceID      string     `json:"instance_id,omitempty"`
	Epoch           string     `json:"epoch"`
	Desired         string     `json:"desired"`
	Revision        int64      `json:"revision"`
	APIVersion      string     `json:"api_version,omitempty"`
}

// probeClient and probes serve only the manifest probe, so status calls
// (client and calls) can never take its connection or its slot.
type entry struct {
	config      Config
	host        auth.Bearer
	client      *http.Client
	probeClient *http.Client
	probes      chan struct{}
	calls       chan struct{}
	reads       chan struct{}
	mu          sync.RWMutex
	info        Info
	lease       context.Context
	endLease    context.CancelFunc
	apiReads    chan struct{}
	apiWrites   chan struct{}
	apiClient   *http.Client
	// sessions is the host slot of the session route alone, so the session
	// poll never waits for the two general host slots (reads).
	sessions chan struct{}
	// row is the slot held across every read or write of the shared row and
	// the adoption of its result.
	row chan struct{}
	// seenAt is the row's seen time on this replica's clock, zero when
	// unseen. local and goodAt are this replica's own last probe result and
	// last good probe under the current epoch.
	seenAt time.Time
	local  string
	goodAt time.Time
}

type Registry struct {
	entries   []*entry
	store     fleet.Repository
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	once      sync.Once
	lifecycle fleet.PluginLifecycleRepository
	// control and introspector back the optional host services.
	control      fleet.ControlRepository
	introspector auth.Introspector
	apiDuration  metrics.Duration
	apiRefused   atomic.Uint64
	apiErrors    atomic.Uint64
	// log is nil until SetLogger; a nil logger discards.
	log *translog.Logger
	// now is the clock of seen times; tests replace it.
	now func() time.Time
	// wake carries the fleet_plugin hint to the sweep.
	wake chan struct{}
}

// SetLogger binds the transition log before the registry starts.
func (r *Registry) SetLogger(log *translog.Logger) { r.log = log }

// stateOf names a plugin state for the transition log.
func stateOf(state string) translog.State {
	switch state {
	case "ready":
		return translog.StateReady
	case "unavailable":
		return translog.StateUnavailable
	case "incompatible":
		return translog.StateIncompatible
	case "enabled":
		return translog.StateEnabled
	case "draining":
		return translog.StateDraining
	case "disabled":
		return translog.StateDisabled
	}
	return translog.StateUnavailable
}

func New(ctx context.Context, configs []Config, store fleet.Repository) *Registry {
	ctx, cancel := context.WithCancel(ctx)
	r := &Registry{store: store, ctx: ctx, cancel: cancel, now: time.Now, wake: make(chan struct{}, 1)}
	r.lifecycle, _ = store.(fleet.PluginLifecycleRepository)
	for _, config := range configs {
		config.Permissions = slices.Clone(config.Permissions)
		host, _ := auth.New(config.hostToken)
		state := "disabled"
		if config.Enabled {
			state = "unavailable"
		}
		desired := "enabled"
		if !config.Enabled {
			desired = "disabled"
		}
		lease, endLease := context.WithCancel(ctx)
		e := &entry{config: config, host: host, client: pluginsdk.NewHTTPClient(), probeClient: pluginsdk.NewHTTPClient(), probes: make(chan struct{}, 1), calls: make(chan struct{}, 2), reads: make(chan struct{}, 2), sessions: make(chan struct{}, 1), row: make(chan struct{}, 1), lease: lease, endLease: endLease, info: Info{
			ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion,
			Enabled: config.Enabled, Permissions: slices.Clone(config.Permissions), State: state,
			Desired: desired,
		}}
		e.apiReads, e.apiWrites = make(chan struct{}, 2), make(chan struct{}, 2)
		e.apiClient = pluginsdk.NewHTTPClient()
		e.apiClient.Timeout = 2 * time.Second
		transport := e.apiClient.Transport.(*http.Transport)
		transport.MaxConnsPerHost, transport.MaxIdleConnsPerHost = 4, 4
		transport.ResponseHeaderTimeout = 2 * time.Second
		e.info.APIVersion = config.APIVersion
		r.entries = append(r.entries, e)
	}
	return r
}

func (r *Registry) Start() {
	if r.lifecycle != nil && len(r.entries) > 0 {
		r.wg.Add(1)
		go r.sweep()
	}
	for _, e := range r.entries {
		if !e.config.Enabled {
			continue
		}
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			for {
				r.probe(e)
				timer := time.NewTimer(time.Second)
				select {
				case <-r.ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	}
}

func (r *Registry) Close() {
	r.once.Do(func() {
		r.cancel()
		r.wg.Wait()
		for _, e := range r.entries {
			e.client.CloseIdleConnections()
			e.probeClient.CloseIdleConnections()
			e.apiClient.CloseIdleConnections()
		}
	})
}

func (e *entry) snapshot() Info {
	e.mu.RLock()
	defer e.mu.RUnlock()
	info := e.info
	info.Permissions = slices.Clone(info.Permissions)
	return info
}

// view is the entry as the API shows it, with its state evaluated now.
func (r *Registry) view(e *entry) Info {
	e.mu.RLock()
	defer e.mu.RUnlock()
	info := e.info
	info.Permissions = slices.Clone(info.Permissions)
	info.State = e.stateLocked(r.now())
	if info.State == "ready" && r.ctx.Err() != nil {
		info.State = "unavailable"
	}
	return info
}

func (r *Registry) call(ctx context.Context, e *entry, path string, headers http.Header, max int64, result any) error {
	return r.rpc(ctx, e, "GET", path, headers, nil, max, result)
}

// errSlotsFull refuses a plugin call when its bounded pool is busy.
var errSlotsFull = errors.New("plugin call slots full")

func (r *Registry) rpc(ctx context.Context, e *entry, method, path string, headers http.Header, body []byte, max int64, result any) error {
	err := r.send(ctx, e, method, path, headers, body, max, result)
	if err == nil {
		return nil
	}
	var failure *pluginsdk.HTTPError
	if errors.As(err, &failure) {
		switch failure.Status {
		case 400:
			return fleet.ErrInvalid
		case 403:
			return auth.ErrForbidden
		case 404:
			return fleet.ErrNotFound
		case 409:
			return fleet.ErrConflict
		case pluginsdk.StatusMisdirected:
			// Never an answer for the caller: the plugin sees another
			// authority than this entry.
			r.refresh(e)
		case 429:
			return fleet.ErrCapacity
		}
	}
	return fleet.ErrUnavailable
}

// send makes one plugin call and returns its raw error.
func (r *Registry) send(ctx context.Context, e *entry, method, path string, headers http.Header, body []byte, max int64, result any) error {
	// The manifest probe has a dedicated slot and connection pool; every other
	// call shares a bounded pool of its own and is refused when it is full.
	slots, client := e.calls, e.client
	if path == pluginsdk.ManifestPath {
		slots, client = e.probes, e.probeClient
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return errSlotsFull
	}
	ctx, cancel := context.WithTimeout(ctx, pluginsdk.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	if path != pluginsdk.ManifestPath {
		e.mu.RLock()
		lease := e.lease
		e.mu.RUnlock()
		stopLease := context.AfterFunc(lease, cancel)
		defer stopLease()
	}
	return pluginsdk.RequestJSON(ctx, client, method, e.config.Endpoint+path, e.config.serviceToken, headers, body, max, result)
}

func (r *Registry) find(id string) *entry {
	for _, e := range r.entries {
		if e.config.ID == id {
			return e
		}
	}
	return nil
}

func (r *Registry) List() []Info {
	items := make([]Info, 0, len(r.entries))
	for _, e := range r.entries {
		items = append(items, r.view(e))
	}
	return items
}

func (r *Registry) Get(id string) (Info, error) {
	for _, info := range r.List() {
		if info.ID == id {
			return info, nil
		}
	}
	return Info{}, fleet.ErrNotFound
}

func (r *Registry) Status(ctx context.Context, id string, caller auth.Principal) (json.RawMessage, error) {
	if caller.Role != "reader" && caller.Role != "operator" && caller.Role != "admin" {
		return nil, auth.ErrForbidden
	}
	e := r.find(id)
	if e == nil {
		return nil, fleet.ErrNotFound
	}
	if r.ctx.Err() != nil || !r.ready(e) {
		return nil, fleet.ErrUnavailable
	}
	var body json.RawMessage
	before := e.snapshot()
	headers := http.Header{pluginsdk.CallerIDHeader: {caller.ID}, pluginsdk.CallerRoleHeader: {caller.Role}}
	if err := r.call(ctx, e, pluginsdk.StatusPath, headers, 32<<10, &body); err != nil {
		return nil, err
	}
	if e.snapshot().Epoch != before.Epoch {
		return nil, fleet.ErrConflict
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, fleet.ErrUnavailable
	}
	return body, nil
}

func hostError(w http.ResponseWriter, status int) {
	if status == 401 {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"host_request_refused","message":"plugin host request refused"}}`))
}

// ServeHTTP exposes the generic host services on the separate host listener.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var owner *entry
	for _, e := range r.entries {
		if e.host.Valid(req) {
			owner = e
		}
	}
	if owner == nil {
		r.log.AuthFailure(translog.ListenerPluginHost)
		hostError(w, 401)
		return
	}
	// The session route has a slot of its own: the poll that keeps the
	// plugin's authority alive never competes with its other host calls.
	slot := owner.reads
	if req.URL.Path == pluginsdk.SessionPath {
		slot = owner.sessions
	}
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	default:
		hostError(w, 503)
		return
	}
	if req.URL.Path == pluginsdk.SessionPath {
		r.serveSession(w, req, owner)
		return
	}
	if !owner.config.Enabled || !r.ready(owner) || r.ctx.Err() != nil {
		hostError(w, 403)
		return
	}
	// The plugin may know a newer row than this replica's cache: read the
	// row once and compare again.
	matches := func() bool {
		info := owner.snapshot()
		return req.Header.Get(pluginsdk.InstanceHeader) == info.InstanceID && req.Header.Get(pluginsdk.EpochHeader) == info.Epoch
	}
	if !matches() {
		r.refresh(owner)
		if !matches() {
			hostError(w, pluginsdk.StatusMisdirected)
			return
		}
	}
	route, ok := hostRoutes[req.URL.Path]
	if !ok {
		hostError(w, 404)
		return
	}
	if !slices.Contains(owner.config.Permissions, route.permission) {
		hostError(w, 403)
		return
	}
	if req.Method != route.method {
		w.Header().Set("Allow", route.method)
		hostError(w, 405)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), pluginsdk.RequestTimeout)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	owner.mu.RLock()
	lease := owner.lease
	owner.mu.RUnlock()
	stopLease := context.AfterFunc(lease, cancel)
	defer stopLease()
	if r.store.Ready(ctx) != nil {
		hostError(w, 503)
		return
	}
	route.serve(r, w, req.WithContext(ctx))
}

// serveSession answers the session poll from the row, read from the database
// on every request, so every replica gives the same answer at once.
func (r *Registry) serveSession(w http.ResponseWriter, req *http.Request, owner *entry) {
	if !owner.config.Enabled || r.ctx.Err() != nil || r.lifecycle == nil {
		hostError(w, 403)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), pluginsdk.RequestTimeout)
	defer cancel()
	states, err := r.lifecycle.PluginStates(ctx)
	if err != nil {
		hostError(w, 503)
		return
	}
	row := findRow(states, owner.config.ID)
	owner.mu.RLock()
	incompatible, cached := owner.local == localIncompatible, owner.info.Epoch
	owner.mu.RUnlock()
	if row.Epoch != cached {
		r.Notify(owner.config.ID)
	}
	if !row.Ready() || incompatible {
		hostError(w, 403)
		return
	}
	if req.Header.Get(pluginsdk.InstanceHeader) != row.InstanceID {
		hostError(w, pluginsdk.StatusMisdirected)
		return
	}
	if req.Method != "GET" || req.URL.RawQuery != "" {
		hostError(w, 400)
		return
	}
	hostJSON(w, 200, pluginsdk.Session{PluginID: owner.config.ID, InstanceID: row.InstanceID, Epoch: row.Epoch, Revision: row.Revision, Permissions: slices.Clone(owner.config.Permissions)})
}
