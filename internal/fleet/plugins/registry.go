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

type entry struct {
	config    Config
	host      auth.Bearer
	client    *http.Client
	calls     chan struct{}
	reads     chan struct{}
	mu        sync.RWMutex
	info      Info
	work      chan struct{}
	lease     context.Context
	endLease  context.CancelFunc
	apiReads  chan struct{}
	apiWrites chan struct{}
	apiClient *http.Client
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
}

func New(ctx context.Context, configs []Config, store fleet.Repository) *Registry {
	ctx, cancel := context.WithCancel(ctx)
	r := &Registry{store: store, ctx: ctx, cancel: cancel}
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
		e := &entry{config: config, host: host, client: pluginsdk.NewHTTPClient(), calls: make(chan struct{}, 2), reads: make(chan struct{}, 2), work: make(chan struct{}, 1), lease: lease, endLease: endLease, info: Info{
			ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion,
			Enabled: config.Enabled, Permissions: slices.Clone(config.Permissions), State: state,
			Desired: desired, Epoch: pluginsdk.RandomID(),
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

func (e *entry) ready() bool {
	info := e.snapshot()
	return info.Desired == "enabled" && info.State == "ready" && info.CheckedAt != nil && time.Since(*info.CheckedAt) < 3*time.Second
}

func (r *Registry) probe(e *entry) {
	before := e.snapshot()
	if before.Desired != "enabled" {
		return
	}
	var m pluginsdk.Manifest
	err := r.call(r.ctx, e, pluginsdk.ManifestPath, nil, 4096, &m)
	state := "ready"
	if err != nil {
		state = "unavailable"
	} else if m.ID != e.config.ID || m.Version != e.config.Version || m.ProtocolVersion != pluginsdk.ProtocolVersion || !pluginsdk.ValidUID(m.InstanceID) || m.APIVersion != e.config.APIVersion {
		state = "incompatible"
	}
	now := time.Now().UTC()
	if state == "ready" && before.InstanceID != "" && before.InstanceID != m.InstanceID {
		// Publish replacement authority only after an admitted lifecycle change exits.
		select {
		case e.work <- struct{}{}:
			defer func() { <-e.work }()
		default:
			state = "unavailable"
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.info.Epoch != before.Epoch || e.info.Desired != "enabled" {
		return
	}
	if (e.info.InstanceID != "" && e.info.InstanceID != m.InstanceID && err == nil) || (e.info.State == "ready" && state != "ready") {
		r.fenceLocked(e)
	}
	if state == "ready" {
		e.info.InstanceID = m.InstanceID
	}
	e.info.State, e.info.CheckedAt = state, &now
}

func (r *Registry) call(ctx context.Context, e *entry, path string, headers http.Header, max int64, result any) error {
	return r.rpc(ctx, e, "GET", path, headers, nil, max, result)
}

func (r *Registry) rpc(ctx context.Context, e *entry, method, path string, headers http.Header, body []byte, max int64, result any) error {
	select {
	case e.calls <- struct{}{}:
		defer func() { <-e.calls }()
	default:
		return fleet.ErrUnavailable
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
	if err := pluginsdk.RequestJSON(ctx, e.client, method, e.config.Endpoint+path, e.config.serviceToken, headers, body, max, result); err != nil {
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
			case 429:
				return fleet.ErrCapacity
			}
		}
		return fleet.ErrUnavailable
	}
	return nil
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
		info := e.snapshot()
		if info.State == "ready" && (!e.ready() || r.ctx.Err() != nil) {
			info.State = "unavailable"
		}
		items = append(items, info)
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
	if caller.Role != "reader" && caller.Role != "admin" {
		return nil, auth.ErrForbidden
	}
	e := r.find(id)
	if e == nil {
		return nil, fleet.ErrNotFound
	}
	if r.ctx.Err() != nil || !e.ready() {
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
		hostError(w, 401)
		return
	}
	select {
	case owner.reads <- struct{}{}:
		defer func() { <-owner.reads }()
	default:
		hostError(w, 503)
		return
	}
	if !owner.config.Enabled || !owner.ready() || r.ctx.Err() != nil {
		hostError(w, 403)
		return
	}
	info := owner.snapshot()
	if req.Header.Get(pluginsdk.InstanceHeader) != info.InstanceID {
		hostError(w, 409)
		return
	}
	if req.URL.Path == pluginsdk.SessionPath {
		if req.Method != "GET" || req.URL.RawQuery != "" {
			hostError(w, 400)
			return
		}
		hostJSON(w, 200, pluginsdk.Session{PluginID: info.ID, InstanceID: info.InstanceID, Epoch: info.Epoch, Revision: info.Revision, Permissions: info.Permissions})
		return
	}
	if req.Header.Get(pluginsdk.EpochHeader) != info.Epoch {
		hostError(w, 409)
		return
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
