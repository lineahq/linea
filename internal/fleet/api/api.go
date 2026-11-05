// Package api exposes one authenticated HTTP contract to all clients.
package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	runtimemetrics "runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/plugins"
	"github.com/lineahq/linea/pluginsdk"
	"github.com/lineahq/linea/pluginsdk/metrics"
)

//go:embed openapi.json
var specification []byte

type Limits struct {
	MaxAgents       int   `json:"max_agents"`
	MaxConnections  int   `json:"max_connections"`
	MaxMessageBytes int64 `json:"max_message_bytes"`
	MaxEvents       int   `json:"max_events,omitempty"`
	// handshakes per replica between admission and their first commit
	MaxPendingSessions int `json:"max_pending_sessions"`
}
type ServerInfo struct {
	ServerID   string   `json:"server_id"`
	ReplicaID  string   `json:"replica_id,omitempty"`
	Version    string   `json:"version"`
	APIVersion string   `json:"api_version"`
	Storage    string   `json:"storage"`
	Plugins    []string `json:"plugins"`
	Limits     Limits   `json:"limits"`
	// ConfigManagement reports whether the config management API is enabled.
	ConfigManagement bool `json:"config_management"`
}
type Handler struct {
	store       fleet.Repository
	access      auth.Authorizer
	control     fleet.ControlRepository
	credentials *auth.Service
	info        ServerInfo
	plugins     *plugins.Registry
	// configs is nil unless config management is enabled.
	configs           configmgmt.Repository
	allowedComponents []string
	ready             atomic.Bool
	draining          atomic.Bool
	drained           chan struct{}
	drainOnce         sync.Once
	waiters           atomic.Int64
	requests, errors  atomic.Uint64
	duration          metrics.Duration
	metricWriters     []func(io.Writer)
}

func New(store fleet.Repository, credential auth.Bearer, info ServerInfo) *Handler {
	return NewAuthorized(store, auth.Static{Bearer: credential, Role: "reader"}, info, nil, nil)
}
func NewAuthorized(store fleet.Repository, access auth.Authorizer, info ServerInfo, control fleet.ControlRepository, credentials *auth.Service) *Handler {
	info.APIVersion, info.Plugins = "v1", []string{}
	if info.Storage == "" {
		info.Storage = "memory"
	}
	return &Handler{store: store, access: access, info: info, control: control, credentials: credentials, drained: make(chan struct{})}
}
func (h *Handler) SetReady(ready bool) { h.ready.Store(ready) }

// Drain withdraws readiness and refuses new mutations on existing keepalives.
// Long-polling event readers return early.
func (h *Handler) Drain() {
	h.draining.Store(true)
	h.ready.Store(false)
	h.drainOnce.Do(func() { close(h.drained) })
}

// SetMetrics binds local, non-blocking collectors before serving requests.
func (h *Handler) SetMetrics(writers ...func(io.Writer)) { h.metricWriters = writers }

// SetPlugins binds operator registrations before serving requests.
func (h *Handler) SetPlugins(registry *plugins.Registry) {
	h.plugins = registry
	h.info.Plugins = []string{}
	for _, entry := range registry.List() {
		h.info.Plugins = append(h.info.Plugins, entry.ID)
	}
}
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

var (
	errNoQuery = fleet.Invalid("this endpoint takes no query parameters")
	errIfMatch = fleet.Invalid(`If-Match must be one quoted, non-negative revision, such as "3"`)
)

// bodyError explains a request body that does not decode. JSON errors name a
// field, a type or an offset, never a value.
func bodyError(err error) error {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.Is(err, fleet.ErrInvalid):
		return err
	case errors.As(err, &syntax):
		return fleet.Invalid(fmt.Sprintf("the request body is not JSON (offset %d)", syntax.Offset))
	case errors.As(err, &typ) && typ.Field != "":
		return fleet.Invalid(fmt.Sprintf("field %s must be a JSON %s", typ.Field, typ.Type.Kind()))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return fleet.Invalid(strings.TrimPrefix(err.Error(), "json: ") + " is not part of this request")
	case errors.Is(err, io.EOF):
		return fleet.Invalid("the request body is empty")
	}
	return fleet.Invalid("the request body is not valid JSON for this endpoint")
}

func failure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "unauthorized", "valid credential required")
	case errors.Is(err, auth.ErrForbidden):
		fail(w, 403, "forbidden", "credential does not permit this operation")
	case errors.Is(err, fleet.ErrNotFound):
		fail(w, 404, "not_found", "resource not found")
	case errors.Is(err, fleet.ErrConflict):
		fail(w, 409, "conflict", "request conflicts with current fleet state")
	case errors.Is(err, fleet.ErrStale):
		fail(w, 409, "stale_connection", "agent connection changed")
	case errors.Is(err, fleet.ErrCapacity):
		fail(w, 409, "capacity_exceeded", "resource capacity reached")
	case errors.Is(err, fleet.ErrPrecondition):
		fail(w, 412, "precondition_failed", "group revision changed; refresh before retrying")
	case errors.Is(err, fleet.ErrCursorExpired):
		fail(w, 410, "cursor_expired", "refresh inventory and groups, then poll without a cursor")
	case errors.Is(err, fleet.ErrInvalid):
		message := "invalid request"
		var invalid *fleet.InvalidError
		if errors.As(err, &invalid) {
			message += ": " + invalid.Reason
		}
		fail(w, 400, "invalid_argument", message)
	default:
		fail(w, 503, "unavailable", "fleet service unavailable")
	}
}

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseStatus) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	fail(w, 405, "method_not_allowed", "unsupported method")
	return false
}
func admin(w http.ResponseWriter, p auth.Principal) bool {
	if p.Role == "admin" {
		return true
	}
	failure(w, auth.ErrForbidden)
	return false
}
func (h *Handler) ServeHTTP(raw http.ResponseWriter, r *http.Request) {
	started := time.Now()
	w := &responseStatus{ResponseWriter: raw, status: 200}
	h.requests.Add(1)
	defer func() {
		h.duration.Observe(time.Since(started))
		if w.status >= 400 {
			h.errors.Add(1)
		}
	}()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Long polls outlive the per-request bound but not the client connection.
	base := r.Context()
	ctx, cancel := context.WithTimeout(base, 4*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		if !method(w, r, "GET") {
			return
		}
		if r.URL.Path == "/readyz" {
			if !h.ready.Load() || h.store.Ready(ctx) != nil {
				fail(w, 503, "not_ready", "server is not ready")
				return
			}
			writeJSON(w, 200, map[string]string{"status": "ready"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	p, err := h.access.Authenticate(ctx, r)
	if err != nil {
		failure(w, err)
		return
	}
	if p.Role != "admin" && p.Role != "reader" {
		failure(w, auth.ErrUnauthorized)
		return
	}
	acting, err := pluginsdk.ReadActing(r.Header)
	if err != nil {
		fail(w, 400, "invalid_acting_for", "acting-for headers are malformed or exceed their bounds")
		return
	}
	ctx, base = fleet.WithActing(ctx, acting), fleet.WithActing(base, acting)
	r = r.WithContext(ctx)
	if h.draining.Load() && r.Method != "GET" {
		fail(w, 503, "draining", "server is draining")
		return
	}
	if err := h.store.Ready(ctx); err != nil {
		failure(w, err)
		return
	}
	switch r.URL.Path {
	case "/api/v1/server":
		if method(w, r, "GET") {
			writeJSON(w, 200, h.info)
		}
	case "/api/v1/openapi.json":
		if method(w, r, "GET") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(specification)
		}
	case "/api/v1/agents":
		if !method(w, r, "GET") {
			return
		}
		q, limit, ok := query(w, r, "after", "connected", "healthy", "selector", "sort")
		if !ok {
			return
		}
		aq, ok := agentQuery(w, q, limit)
		if !ok {
			return
		}
		page, err := h.store.ListAgents(ctx, aq)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, page)
	case "/api/v1/summary":
		if !method(w, r, "GET") {
			return
		}
		q, _, ok := query(w, r, "group_by")
		if !ok || q.Has("limit") {
			if ok {
				failure(w, fleet.Invalid("the summary takes no limit"))
			}
			return
		}
		groupBy := q.Get("group_by")
		if _, promoted := fleet.Promoted[groupBy]; q.Has("group_by") && !promoted {
			failure(w, fleet.Invalid("group_by "+fleet.PromotedRule))
			return
		}
		summary, err := h.store.Summary(ctx, groupBy)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, summary)
	case "/api/v1/plugins":
		if !method(w, r, "GET") {
			return
		}
		if r.URL.RawQuery != "" {
			failure(w, errNoQuery)
			return
		}
		items := []plugins.Info{}
		if h.plugins != nil {
			items = h.plugins.List()
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case "/api/v1/events":
		if h.control == nil {
			failure(w, fleet.ErrNotFound)
			return
		}
		if !method(w, r, "GET") {
			return
		}
		q, limit, ok := query(w, r, "cursor", "wait")
		if !ok {
			return
		}
		if q.Has("cursor") && q.Get("cursor") == "" {
			failure(w, fleet.Invalid("cursor must not be empty"))
			return
		}
		wait := 0
		if q.Has("wait") {
			var err error
			if wait, err = strconv.Atoi(q.Get("wait")); err != nil || wait < 0 || wait > maxEventWait {
				failure(w, fleet.Invalid(fmt.Sprintf("wait must be 0 to %d seconds", maxEventWait)))
				return
			}
		}
		page, err := h.control.Events(ctx, q.Get("cursor"), limit)
		if err == nil && len(page.Items) == 0 && wait > 0 {
			page, err = h.longPoll(base, w, q.Get("cursor"), limit, page, time.Duration(wait)*time.Second)
		}
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, page)
	case "/api/v1/groups":
		if h.control == nil {
			failure(w, fleet.ErrNotFound)
			return
		}
		if !method(w, r, "GET") {
			return
		}
		q, limit, ok := query(w, r, "after")
		if !ok {
			return
		}
		after, ok := afterID(w, q)
		if !ok {
			return
		}
		page, err := h.control.ListGroups(ctx, after, limit)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, page)
	case "/api/v1/credentials":
		if h.credentials == nil {
			failure(w, fleet.ErrNotFound)
			return
		}
		if !method(w, r, "GET", "POST") || !admin(w, p) {
			return
		}
		if r.Method == "POST" {
			var req auth.CredentialRequest
			if !decode(w, r, &req) {
				return
			}
			issued, err := h.credentials.Issue(ctx, p.ID, req)
			if err != nil {
				failure(w, err)
				return
			}
			w.Header().Set("Location", "/api/v1/credentials/"+issued.Credential.ID)
			writeJSON(w, 201, issued)
			return
		}
		q, limit, ok := query(w, r, "after")
		if !ok {
			return
		}
		after, ok := afterID(w, q)
		if !ok {
			return
		}
		page, err := h.credentials.Repository.ListCredentials(ctx, after, limit)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, page)
	case "/metrics":
		h.metrics(w, r)
	default:
		if h.configRoute(w, r, p) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/plugins/") {
			h.pluginResource(w, r, p)
		} else {
			h.resource(w, r, p)
		}
	}
}

func (h *Handler) pluginResource(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/plugins/"), "/")
	if len(parts) >= 2 && parts[1] == "api" {
		if h.plugins == nil {
			failure(w, fleet.ErrNotFound)
			return
		}
		active, err := h.access.Active(r.Context(), []auth.Principal{p})
		if err != nil {
			failure(w, err)
			return
		}
		if !active[p.ID] {
			failure(w, auth.ErrUnauthorized)
			return
		}
		h.plugins.ServeAPI(w, r, parts[0], strings.Join(parts[2:], "/"), p)
		return
	}
	if len(parts) >= 2 && parts[1] == "lifecycle" {
		h.pluginOperation(w, r, p, parts)
		return
	}
	if !method(w, r, "GET") {
		return
	}
	if r.URL.RawQuery != "" {
		failure(w, errNoQuery)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/plugins/")
	status := strings.HasSuffix(id, "/status")
	if status {
		id = strings.TrimSuffix(id, "/status")
	}
	if !pluginsdk.ValidID(id) {
		failure(w, fleet.Invalid("plugin id must be 1 to 64 letters, digits, dots, hyphens or underscores"))
		return
	}
	if h.plugins == nil {
		failure(w, fleet.ErrNotFound)
		return
	}
	if status {
		body, err := h.plugins.Status(r.Context(), id, p)
		if err != nil {
			failure(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"plugin_id": id, "data": body})
		return
	}
	info, err := h.plugins.Get(id)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, info)
}

func (h *Handler) pluginOperation(w http.ResponseWriter, r *http.Request, p auth.Principal, parts []string) {
	if h.plugins == nil || !pluginsdk.ValidID(parts[0]) {
		failure(w, fleet.ErrNotFound)
		return
	}
	if len(parts) != 2 || r.URL.RawQuery != "" {
		failure(w, fleet.Invalid("a plugin operation is /api/v1/plugins/{id}/{operation}, without a query"))
		return
	}
	if !method(w, r, "POST") || !admin(w, p) {
		return
	}
	var request struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &request) {
		return
	}
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		fail(w, 428, "precondition_required", "If-Match revision required")
		return
	}
	if len(values) != 1 || len(values[0]) < 3 || values[0][0] != '"' || values[0][len(values[0])-1] != '"' {
		failure(w, errIfMatch)
		return
	}
	revision, err := strconv.ParseInt(values[0][1:len(values[0])-1], 10, 64)
	if err != nil || revision < 0 {
		failure(w, errIfMatch)
		return
	}
	info, err := h.plugins.Lifecycle(r.Context(), parts[0], request.Action, p, revision)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, info)
}
func (h *Handler) resource(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	ctx := r.Context()
	for _, kind := range []string{"agents", "groups", "credentials"} {
		id, ok := strings.CutPrefix(r.URL.Path, "/api/v1/"+kind+"/")
		if !ok {
			continue
		}
		if group, members := strings.CutSuffix(id, "/agents"); members && kind == "groups" && fleet.ValidID(group) {
			h.groupAgents(w, r, group)
			return
		}
		if !fleet.ValidID(id) {
			failure(w, fleet.Invalid("id "+fleet.IDRule))
			return
		}
		switch kind {
		case "agents":
			if !method(w, r, "GET") {
				return
			}
			a, err := h.store.GetAgent(ctx, id)
			if err != nil {
				failure(w, err)
				return
			}
			writeJSON(w, 200, a)
		case "groups":
			if h.control == nil {
				failure(w, fleet.ErrNotFound)
				return
			}
			if !method(w, r, "GET", "PUT", "DELETE") {
				return
			}
			if r.Method == "GET" {
				g, err := h.control.GetGroup(ctx, id)
				if err != nil {
					failure(w, err)
					return
				}
				w.Header().Set("ETag", fmt.Sprintf("%q", strconv.FormatInt(g.Revision, 10)))
				writeJSON(w, 200, g)
				return
			}
			if !admin(w, p) {
				return
			}
			revision, ok := precondition(w, r)
			if !ok {
				return
			}
			if r.Method == "DELETE" {
				if err := h.control.DeleteGroup(ctx, p.ID, id, revision); err != nil {
					failure(w, err)
					return
				}
				w.WriteHeader(204)
				return
			}
			// Exactly one of agent_ids (static) or selector (evaluated on read).
			var body struct {
				Name     string   `json:"name"`
				AgentIDs []string `json:"agent_ids"`
				Selector *string  `json:"selector"`
			}
			if !decode(w, r, &body) {
				return
			}
			if (body.AgentIDs == nil) == (body.Selector == nil) || body.Selector != nil && *body.Selector == "" {
				failure(w, fleet.Invalid("a group has either agent_ids or a nonempty selector"))
				return
			}
			group := fleet.Group{ID: id, Name: body.Name, AgentIDs: body.AgentIDs}
			if body.Selector != nil {
				group.Selector = *body.Selector
			}
			g, err := h.control.PutGroup(ctx, p.ID, group, revision)
			if err != nil {
				failure(w, err)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf("%q", strconv.FormatInt(g.Revision, 10)))
			status := 200
			if revision == 0 {
				status = 201
			}
			writeJSON(w, status, g)
		case "credentials":
			if h.credentials == nil {
				failure(w, fleet.ErrNotFound)
				return
			}
			if !method(w, r, "GET", "PATCH", "DELETE") || !admin(w, p) {
				return
			}
			if r.Method == "DELETE" {
				if err := h.credentials.Repository.RevokeCredential(ctx, p.ID, id); err != nil {
					failure(w, err)
					return
				}
				w.WriteHeader(204)
				return
			}
			if r.Method == "PATCH" {
				// Only the remote config opt-in is mutable.
				var body struct {
					RemoteConfig *bool `json:"remote_config"`
				}
				if !decode(w, r, &body) {
					return
				}
				if body.RemoteConfig == nil {
					failure(w, fleet.Invalid("remote_config is required"))
					return
				}
				c, err := h.credentials.Repository.SetCredentialRemoteConfig(ctx, p.ID, id, *body.RemoteConfig)
				if err != nil {
					failure(w, err)
					return
				}
				writeJSON(w, 200, c)
				return
			}
			c, err := h.credentials.Repository.Credential(ctx, id)
			if err != nil {
				failure(w, err)
				return
			}
			writeJSON(w, 200, c)
		}
		return
	}
	failure(w, fleet.ErrNotFound)
}
func query(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, int, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		failure(w, fleet.Invalid("the query string is malformed"))
		return nil, 0, false
	}
	for key, values := range q {
		valid := key == "limit"
		for _, k := range allowed {
			valid = valid || key == k
		}
		if !valid || len(values) != 1 {
			failure(w, fleet.Invalid("query parameters are "+strings.Join(slices.Concat(allowed, []string{"limit"}), ", ")+", each at most once"))
			return nil, 0, false
		}
	}
	limit := 100
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 500 {
			failure(w, fleet.Invalid("limit must be 1 to 500"))
			return nil, 0, false
		}
	}
	return q, limit, true
}

// agentQuery reads inventory filters. With sort, after is the opaque
// next_after of the previous page; otherwise it is an agent ID.
func agentQuery(w http.ResponseWriter, q url.Values, limit int) (fleet.AgentQuery, bool) {
	aq := fleet.AgentQuery{Limit: limit, After: q.Get("after"), Sort: q.Get("sort")}
	flag := func(name string) (*bool, bool) {
		if !q.Has(name) {
			return nil, true
		}
		v := q.Get(name)
		b := v == "true"
		return &b, v == "true" || v == "false"
	}
	var ok bool
	if aq.Connected, ok = flag("connected"); !ok {
		failure(w, fleet.Invalid("connected must be true or false"))
		return aq, false
	}
	if aq.Healthy, ok = flag("healthy"); !ok {
		failure(w, fleet.Invalid("healthy must be true or false"))
		return aq, false
	}
	if q.Has("selector") {
		selector, err := fleet.ParseSelector(q.Get("selector"))
		if err != nil {
			failure(w, err)
			return aq, false
		}
		aq.Selector = selector
	}
	if _, promoted := fleet.Promoted[aq.Sort]; q.Has("sort") && !promoted {
		failure(w, fleet.Invalid("sort "+fleet.PromotedRule))
		return aq, false
	}
	if q.Has("after") {
		valid := fleet.ValidID(aq.After)
		if aq.Sort != "" {
			_, _, err := fleet.DecodeAfter(aq.After)
			valid = err == nil
		}
		if !valid {
			failure(w, fleet.Invalid("after must be a position from a previous page with the same sort"))
			return aq, false
		}
	}
	return aq, true
}

// groupAgents pages a group's current members: the explicit members of a
// static group, or the agents a selector group matches now.
func (h *Handler) groupAgents(w http.ResponseWriter, r *http.Request, id string) {
	if h.control == nil {
		failure(w, fleet.ErrNotFound)
		return
	}
	if !method(w, r, "GET") {
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after, ok := afterID(w, q)
	if !ok {
		return
	}
	g, err := h.control.GetGroup(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	aq := fleet.AgentQuery{After: after, Limit: limit}
	if g.Kind == fleet.GroupSelector {
		if aq.Selector, err = fleet.ParseSelector(g.Selector); err != nil {
			failure(w, fleet.ErrUnavailable)
			return
		}
	} else {
		aq.Group = g.ID
	}
	page, err := h.store.ListAgents(r.Context(), aq)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func afterID(w http.ResponseWriter, q url.Values) (string, bool) {
	id := q.Get("after")
	if q.Has("after") && !fleet.ValidID(id) {
		failure(w, fleet.Invalid("after "+fleet.IDRule))
		return "", false
	}
	return id, true
}
func precondition(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if len(r.Header.Values("If-Match")) == 0 {
		fail(w, 428, "precondition_required", "If-Match with quoted revision is required")
		return 0, false
	}
	if len(r.Header.Values("If-Match")) != 1 {
		failure(w, errIfMatch)
		return 0, false
	}
	value, err := strconv.Unquote(r.Header.Get("If-Match"))
	if err != nil {
		failure(w, errIfMatch)
		return 0, false
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 || strconv.FormatInt(revision, 10) != value {
		failure(w, errIfMatch)
		return 0, false
	}
	return revision, true
}
func decode(w http.ResponseWriter, r *http.Request, dest any) bool {
	return decodeLimit(w, r, dest, 64*1024)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, dest any, limit int64) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, "unsupported_media_type", "application/json required")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	err = dec.Decode(dest)
	if err == nil {
		var extra any
		if trailing := dec.Decode(&extra); trailing != io.EOF {
			if trailing == nil {
				err = fleet.Invalid("the request body must hold one JSON value")
			} else {
				err = trailing
			}
		}
	}
	if err == nil {
		return true
	}
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		fail(w, 413, "payload_too_large", fmt.Sprintf("request body exceeds %d KiB", limit/1024))
	} else {
		failure(w, bodyError(err))
	}
	return false
}
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET") {
		return
	}
	summary, err := h.store.Summary(r.Context(), "")
	if err != nil {
		failure(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE linea_fleet_http_requests_total counter\n"+
		"linea_fleet_http_requests_total %d\n# TYPE linea_fleet_http_errors_total counter\n"+
		"linea_fleet_http_errors_total %d\n# TYPE linea_fleet_agents gauge\n"+
		"linea_fleet_agents{presence=\"connected\"} %d\nlinea_fleet_agents{presence=\"disconnected\"} %d\n"+
		"linea_fleet_agents{presence=\"unknown\"} %d\n", h.requests.Load(), h.errors.Load(), summary.Connected, summary.Disconnected, summary.Unknown)
	h.duration.Snapshot().Write(w, "linea_fleet_http_duration_seconds")
	writeRuntime(w)
	for _, write := range h.metricWriters {
		write(w)
	}
}

// writeRuntime exports the process footprint used to check the per-agent
// memory budget. Heap objects include garbage awaiting the
// next GC; the heap marked live by the last GC and goroutine stacks are the
// steady cost of the sessions held.
func writeRuntime(w io.Writer) {
	samples := []runtimemetrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}, {Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/heap/live:bytes"}, {Name: "/memory/classes/heap/stacks:bytes"}}
	runtimemetrics.Read(samples)
	value := func(s runtimemetrics.Sample) uint64 {
		if s.Value.Kind() == runtimemetrics.KindUint64 {
			return s.Value.Uint64()
		}
		return 0
	}
	fmt.Fprintf(w, "# TYPE linea_fleet_go_heap_objects_bytes gauge\nlinea_fleet_go_heap_objects_bytes %d\n# TYPE linea_fleet_go_memory_bytes gauge\nlinea_fleet_go_memory_bytes %d\n# TYPE linea_fleet_goroutines gauge\nlinea_fleet_goroutines %d\n", value(samples[0]), value(samples[1]), value(samples[2]))
	fmt.Fprintf(w, "# TYPE linea_fleet_go_heap_live_bytes gauge\nlinea_fleet_go_heap_live_bytes %d\n# TYPE linea_fleet_go_stack_bytes gauge\nlinea_fleet_go_stack_bytes %d\n", value(samples[3]), value(samples[4]))
}

// Long-poll bounds: at most maxEventWaiters readers park per replica and
// each waits at most maxEventWait seconds.
const maxEventWait, maxEventWaiters = 30, 64

// longPoll waits for the next event hint, with a periodic fallback because
// hints can be lost. It returns the empty page on timeout, drain or when
// too many readers are already waiting.
func (h *Handler) longPoll(base context.Context, w http.ResponseWriter, cursor string, limit int, page fleet.EventPage, wait time.Duration) (fleet.EventPage, error) {
	if h.waiters.Add(1) > maxEventWaiters {
		h.waiters.Add(-1)
		return page, nil
	}
	defer h.waiters.Add(-1)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(wait + 5*time.Second))
	notifier, _ := h.control.(fleet.EventNotifier)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		var signal <-chan struct{}
		if notifier != nil {
			signal = notifier.EventSignal()
		}
		select {
		case <-base.Done():
			return page, nil
		case <-h.drained:
			return page, nil
		case <-deadline.C:
			return page, nil
		case <-signal:
		case <-poll.C:
		}
		ctx, cancel := context.WithTimeout(base, 4*time.Second)
		next, err := h.control.Events(ctx, cursor, limit)
		cancel()
		if err != nil || len(next.Items) > 0 {
			return next, err
		}
		page = next
	}
}
