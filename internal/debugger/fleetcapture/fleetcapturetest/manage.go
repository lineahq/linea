package fleetcapturetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The management half of the fake: fleet's core inventory, group writes,
// events and config management, enough for the debugger's fleet node. State
// is in memory; documents are JSON objects as fleet sends them. Groups,
// configs, assignments and rollouts created here have fleet's 32-hex IDs.

// Created is the time the fake stamps on everything it stores.
var Created = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// Admin is the actor the fake records for every write.
const Admin = "0123456789abcdef0123456789abcdef"

type doc = map[string]any

type store struct {
	groups      map[string]doc
	configs     map[string]doc
	versions    map[string][]doc
	assignments map[string]doc
	rollouts    map[string]doc
	events      []doc
	retried     []string
}

func newStore() *store {
	return &store{groups: map[string]doc{}, configs: map[string]doc{}, versions: map[string][]doc{}, assignments: map[string]doc{}, rollouts: map[string]doc{}}
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc{"error": doc{"code": code, "message": message}})
}

func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// match checks If-Match against a revision; ok is false once it refused.
func match(w http.ResponseWriter, r *http.Request, revision int64) bool {
	h := r.Header.Get("If-Match")
	if h == "" {
		refuse(w, http.StatusPreconditionRequired, "precondition_required", "If-Match is required")
		return false
	}
	if h != strconv.Quote(strconv.FormatInt(revision, 10)) {
		refuse(w, http.StatusPreconditionFailed, "precondition_failed", "revision changed")
		return false
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_argument", "invalid JSON")
		return false
	}
	return true
}

func revisionOf(d doc) int64 {
	switch n := d["revision"].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// page returns docs sorted by id after the cursor, at most limit.
func page(items map[string]doc, r *http.Request) doc {
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []doc{}
	for _, id := range ids {
		if id > r.URL.Query().Get("after") {
			out = append(out, items[id])
		}
	}
	return doc{"items": out}
}

// agent is Peer's agent as fleet reports it.
func agent(detail bool) doc {
	a := doc{
		"id": Peer.AgentID, "generation": 1, "connected": true, "presence": "connected", "identity_conflict": false,
		"first_seen_at": Created, "connected_at": Created, "last_seen_at": Created, "capabilities": "1",
		"credential_id": Admin, "attributes": doc{"service.name": "gateway", "host.name": "pod"},
		"health": doc{"healthy": true, "observed_at": Created, "status": "StatusOK", "components_unhealthy": 0},
	}
	if detail {
		a["components"] = []doc{{"id": "receiver:otlp", "pipelines": []string{"logs"}, "healthy": true}}
	}
	return a
}

// event records a change as fleet does, with the request's acting-for
// headers as acting_for.
func (f *Server) event(r *http.Request, kind, subject string) {
	e := doc{"id": strconv.Itoa(len(f.manage.events) + 1), "kind": kind, "subject_id": subject, "actor_id": Admin, "occurred_at": Created}
	if id := r.Header.Get("X-Fleet-Acting-ID"); id != "" {
		acting := doc{"id": id}
		if v := r.Header.Get("X-Fleet-Acting-Name"); v != "" {
			acting["name"] = v
		}
		if v := r.Header.Get("X-Fleet-Acting-Request"); v != "" {
			acting["request_id"] = v
		}
		e["acting_for"] = acting
	}
	f.manage.events = append(f.manage.events, e)
}

// Retried returns the agents whose config hold a retry cleared.
func (f *Server) Retried() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.manage.retried...)
}

// mount adds the management routes; the capture half's GET routes for its
// fixed group and config defer to lookups here first.
func (f *Server) mount(mux *http.ServeMux) {
	m := f.manage
	lock := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /api/v1/server", lock(func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, doc{"server_id": "fake", "version": "test", "api_version": "v1", "storage": "memory", "plugins": []string{}, "config_management": true, "limits": doc{}})
	}))
	mux.HandleFunc("GET /api/v1/summary", lock(func(w http.ResponseWriter, r *http.Request) {
		s := doc{"total": 1, "connected": 1, "disconnected": 0, "unknown": 0}
		if r.URL.Query().Get("group_by") != "" {
			s["groups"] = []doc{{"value": "gateway", "total": 1, "connected": 1}}
		}
		send(w, 200, s)
	}))
	mux.HandleFunc("GET /api/v1/agents", lock(func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, doc{"items": []doc{agent(false)}})
	}))
	mux.HandleFunc("GET /api/v1/agents/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != Peer.AgentID {
			refuse(w, 404, "not_found", "no agent")
			return
		}
		send(w, 200, agent(true))
	}))
	mux.HandleFunc("POST /api/v1/agents/{id}/{op}", lock(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != Peer.AgentID || r.PathValue("op") != "config:retry" {
			refuse(w, 404, "not_found", "no agent")
			return
		}
		m.retried = append(m.retried, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /api/v1/events", lock(func(w http.ResponseWriter, r *http.Request) {
		from := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			var err error
			if from, err = strconv.Atoi(c); err != nil {
				refuse(w, 410, "cursor_expired", "cursor expired")
				return
			}
		}
		items := m.events[min(from, len(m.events)):]
		send(w, 200, doc{"items": items, "next_cursor": strconv.Itoa(from + len(items))})
	}))

	mux.HandleFunc("PUT /api/v1/groups/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req struct {
			Name     string    `json:"name"`
			AgentIDs *[]string `json:"agent_ids"`
			Selector string    `json:"selector"`
		}
		if !decode(w, r, &req) {
			return
		}
		old := m.groups[id]
		if !match(w, r, revisionOf(old)) {
			return
		}
		if req.Name == "" || (req.AgentIDs == nil) == (req.Selector == "") {
			refuse(w, 400, "invalid_argument", "name and exactly one of agent_ids or selector")
			return
		}
		g := doc{"id": id, "name": req.Name, "revision": revisionOf(old) + 1}
		if req.Selector != "" {
			g["kind"], g["selector"] = "selector", req.Selector
		} else {
			g["kind"], g["agent_ids"], g["member_count"] = "static", *req.AgentIDs, len(*req.AgentIDs)
		}
		m.groups[id] = g
		status := 200
		if old == nil {
			status = 201
			f.event(r, "group.created", id)
		} else {
			f.event(r, "group.updated", id)
		}
		send(w, status, g)
	}))
	mux.HandleFunc("DELETE /api/v1/groups/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		g := m.groups[r.PathValue("id")]
		if g == nil {
			refuse(w, 404, "not_found", "no group")
			return
		}
		if !match(w, r, revisionOf(g)) {
			return
		}
		delete(m.groups, r.PathValue("id"))
		f.event(r, "group.deleted", r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /api/v1/configs", lock(func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, page(m.configs, r))
	}))
	mux.HandleFunc("POST /api/v1/configs", lock(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, Name, Description string }
		if !decode(w, r, &req) {
			return
		}
		if m.configs[req.ID] != nil {
			refuse(w, 409, "conflict", "config exists")
			return
		}
		c := doc{"id": req.ID, "name": req.Name, "description": req.Description, "revision": int64(1), "latest_version": 0, "created_at": Created, "created_by": Admin}
		m.configs[req.ID] = c
		send(w, 201, c)
	}))
	mux.HandleFunc("PATCH /api/v1/configs/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		c := m.configs[r.PathValue("id")]
		if c == nil {
			refuse(w, 404, "not_found", "no config")
			return
		}
		var req map[string]string
		if !decode(w, r, &req) || !match(w, r, revisionOf(c)) {
			return
		}
		for k, v := range req {
			c[k] = v
		}
		c["revision"] = revisionOf(c) + 1
		send(w, 200, c)
	}))
	mux.HandleFunc("DELETE /api/v1/configs/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		c := m.configs[r.PathValue("id")]
		if c == nil {
			refuse(w, 404, "not_found", "no config")
			return
		}
		if !match(w, r, revisionOf(c)) {
			return
		}
		for _, a := range m.assignments {
			if a["config_id"] == r.PathValue("id") {
				refuse(w, 409, "conflict", "config is assigned")
				return
			}
		}
		delete(m.configs, r.PathValue("id"))
		delete(m.versions, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /api/v1/configs/{id}/versions", lock(func(w http.ResponseWriter, r *http.Request) {
		c := m.configs[r.PathValue("id")]
		if c == nil {
			refuse(w, 404, "not_found", "no config")
			return
		}
		var req struct{ Body, Message string }
		if !decode(w, r, &req) {
			return
		}
		if strings.Contains(req.Body, "password: ") {
			refuse(w, 422, "inline_secret", "inline secret at password")
			return
		}
		n := len(m.versions[r.PathValue("id")]) + 1
		v := doc{"config_id": r.PathValue("id"), "version": n, "sha256": fmt.Sprintf("%064x", n), "size": len(req.Body), "message": req.Message, "created_at": Created, "created_by": Admin, "body": req.Body}
		m.versions[r.PathValue("id")] = append(m.versions[r.PathValue("id")], v)
		c["latest_version"] = n
		out := doc{}
		for k, x := range v {
			if k != "body" {
				out[k] = x
			}
		}
		send(w, 201, out)
	}))

	mux.HandleFunc("GET /api/v1/config-assignments", lock(func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, page(m.assignments, r))
	}))
	mux.HandleFunc("GET /api/v1/config-assignments/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		if a := m.assignments[r.PathValue("id")]; a != nil {
			send(w, 200, a)
			return
		}
		refuse(w, 404, "not_found", "no assignment")
	}))
	mux.HandleFunc("POST /api/v1/config-assignments", lock(func(w http.ResponseWriter, r *http.Request) {
		var req doc
		if !decode(w, r, &req) {
			return
		}
		id, _ := req["id"].(string)
		configID, _ := req["config_id"].(string)
		if m.configs[configID] == nil {
			refuse(w, 400, "invalid_argument", "unknown config")
			return
		}
		rolloutID := fmt.Sprintf("%032x", len(m.rollouts)+1)
		a := doc{"id": id, "name": req["name"], "config_id": configID, "layer": req["layer"], "revision": int64(1), "created_at": Created, "created_by": Admin, "rollout_id": rolloutID}
		for _, k := range []string{"group_id", "selector", "credential_labels"} {
			if v, ok := req[k]; ok {
				a[k] = v
			}
		}
		m.assignments[id] = a
		m.rollouts[rolloutID] = rollout(rolloutID, id, configID, 0, req["version"])
		send(w, 201, a)
	}))
	mux.HandleFunc("POST /api/v1/config-assignments/{op}", lock(func(w http.ResponseWriter, r *http.Request) {
		id, ok := strings.CutSuffix(r.PathValue("op"), ":preview")
		a := m.assignments[id]
		if !ok || a == nil {
			refuse(w, 404, "not_found", "no assignment")
			return
		}
		layer := doc{"name": fmt.Sprintf("%02v-%v", a["layer"], a["name"]), "assignment_id": id, "config_id": a["config_id"], "version": a["version"], "sha256": fmt.Sprintf("%064x", 1)}
		send(w, 200, doc{"items": []doc{{"agent_id": Peer.AgentID, "desired": doc{"layers": []doc{layer}, "hash": fmt.Sprintf("%064x", 2)}}}})
	}))

	mux.HandleFunc("GET /api/v1/rollouts", lock(func(w http.ResponseWriter, r *http.Request) {
		send(w, 200, page(m.rollouts, r))
	}))
	mux.HandleFunc("GET /api/v1/rollouts/{id}", lock(func(w http.ResponseWriter, r *http.Request) {
		if ro := m.rollouts[r.PathValue("id")]; ro != nil {
			send(w, 200, ro)
			return
		}
		refuse(w, 404, "not_found", "no rollout")
	}))
	mux.HandleFunc("GET /api/v1/rollouts/{id}/targets", lock(func(w http.ResponseWriter, r *http.Request) {
		ro := m.rollouts[r.PathValue("id")]
		if ro == nil {
			refuse(w, 404, "not_found", "no rollout")
			return
		}
		items := []doc{}
		if o := r.URL.Query().Get("outcome"); o == "" || o == "pending" {
			items = append(items, doc{"agent_id": Peer.AgentID, "wave": 0, "outcome": "pending", "accepted": false})
		}
		send(w, 200, doc{"items": items})
	}))
	mux.HandleFunc("POST /api/v1/rollouts", lock(func(w http.ResponseWriter, r *http.Request) {
		var req doc
		if !decode(w, r, &req) {
			return
		}
		aid, _ := req["assignment_id"].(string)
		a := m.assignments[aid]
		if a == nil {
			refuse(w, 400, "invalid_argument", "unknown assignment")
			return
		}
		if active, _ := a["rollout_id"].(string); active != "" && m.rollouts[active]["state"] == "running" {
			refuse(w, 409, "conflict", "rollout_conflict")
			return
		}
		id := fmt.Sprintf("%032x", len(m.rollouts)+1)
		ro := rollout(id, aid, a["config_id"], a["version"], req["to_version"])
		if wv, ok := req["waves"]; ok {
			ro["policy"].(doc)["waves"] = wv
		}
		m.rollouts[id] = ro
		a["rollout_id"] = id
		send(w, 201, ro)
	}))
	mux.HandleFunc("POST /api/v1/rollouts/{op}", lock(func(w http.ResponseWriter, r *http.Request) {
		id, action, _ := strings.Cut(r.PathValue("op"), ":")
		ro := m.rollouts[id]
		if ro == nil {
			refuse(w, 404, "not_found", "no rollout")
			return
		}
		next := map[string]map[string]string{
			"pause":    {"running": "paused"},
			"resume":   {"paused": "running"},
			"rollback": {"running": "rolled_back", "paused": "rolled_back"},
			"cancel":   {"running": "cancelled", "paused": "cancelled"},
		}[action]
		to, ok := next[ro["state"].(string)]
		if !ok {
			refuse(w, 409, "conflict", "invalid_state")
			return
		}
		ro["state"] = to
		if action != "resume" {
			ro["reason"] = "operator"
		}
		send(w, 200, ro)
	}))
}

// rollout is a running rollout of one target in one wave, not released yet
// (fleet reports current_wave -1 until the first release).
func rollout(id, assignment string, config, from, to any) doc {
	ro := doc{"id": id, "assignment_id": assignment, "config_id": config, "state": "running", "current_wave": -1, "wave_count": 1,
		"policy":  doc{"waves": []doc{{"percent": 100}}, "bake_seconds": 120, "wave_timeout_seconds": 900, "max_failures": 0, "max_failure_percent": 5, "on_failure": "pause"},
		"targets": 1, "waves": []doc{{"wave": 0, "targets": 1, "outcomes": doc{"pending": 1}}}, "created_at": Created, "created_by": Admin, "updated_at": Created}
	if from != nil && from != 0 {
		ro["from_version"] = from
	}
	if to != nil {
		ro["to_version"] = to
	}
	return ro
}
