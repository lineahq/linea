// Package fleetcapturetest serves a minimal fleet livecapture plugin over
// HTTP for tests of the debugger's interfaces.
package fleetcapturetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// Token is the bearer token the server accepts.
const Token = "fleet-token"

// Group is the ID of the server's one static group, holding Peer's agent.
const Group = "edge"

// Peer is the server's one Collector: logs egress, redacted.
var Peer = fleetcapture.Peer{
	AuthorityID: strings.Repeat("a", 32), PluginInstance: strings.Repeat("b", 32), ID: strings.Repeat("1", 64), Generation: 1,
	AgentID: strings.Repeat("d", 32), CredentialID: "credential",
	Identity: fleetcapture.Identity{Pod: "pod", CollectorGroup: "gateways", CollectorVersion: "0.162.0", Enabled: true, CapturePoints: []fleetcapture.CapturePoint{{Pipeline: "logs", Point: "egress", Signal: "logs", Sensitivity: "redacted"}}},
}

// Server is a started fake fleet.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	ops    map[string]*fleetcapture.Operation
	last   string
	queue  []fleetcapture.Envelope
	acks   map[string]int64
	nextID int
	// bodies are the versions of Config, oldest first, once ManageConfig
	// has set them.
	bodies []string
	manage *store
}

// Config is the ID of the one config fleet sends Peer's agent once
// ManageConfig has set its versions, in layer ConfigEntry.
const (
	Config      = "pipeline"
	ConfigEntry = "10-pipeline"
)

// NewServer starts the server. Captures stay active until stopped, and a
// stop completes them at once; Deliver hands them records.
func NewServer(t testing.TB) *Server {
	t.Helper()
	f := &Server{ops: map[string]*fleetcapture.Operation{}, acks: map[string]int64{}, manage: newStore()}
	mu, ops := &f.mu, f.ops
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	one := 1
	group := fleetcapture.Group{ID: Group, Name: "Edge", Kind: "static", Revision: 1, MemberCount: &one, AgentIDs: []string{Peer.AgentID}}
	const base = "/api/v1/plugins/livecapture/api/v1/"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"peers", func(w http.ResponseWriter, r *http.Request) {
		reply(w, fleetcapture.PeerPage{Items: []fleetcapture.Peer{Peer}})
	})
	mux.HandleFunc("POST "+base+"channels", func(w http.ResponseWriter, r *http.Request) {
		reply(w, fleetcapture.Channel{AuthorityID: Peer.AuthorityID, PluginInstance: Peer.PluginInstance, ID: strings.Repeat("c", 32), HeartbeatSeconds: 5})
	})
	mux.HandleFunc("GET "+base+"channels/{id}/next", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if len(f.queue) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		e := f.queue[0]
		f.queue = f.queue[1:]
		reply(w, e)
	})
	mux.HandleFunc("POST "+base+"channels/{id}/acks", func(w http.ResponseWriter, r *http.Request) {
		var ack fleetcapture.Ack
		if json.NewDecoder(r.Body).Decode(&ack) != nil {
			http.Error(w, `{"error":{"code":"invalid","message":"bad ack"}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		f.acks[ack.BatchID] = ack.Accepted
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE "+base+"channels/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST "+base+"captures", func(w http.ResponseWriter, r *http.Request) {
		var req fleetcapture.CaptureRequest
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, `{"error":{"code":"invalid","message":"bad request"}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		op := &fleetcapture.Operation{AuthorityID: req.AuthorityID, ID: req.ID, State: "active", Revision: 1, ExpiresAt: req.ExpiresAt,
			Targets: []fleetcapture.TargetOutcome{{PeerID: Peer.ID, Generation: 1, AgentID: Peer.AgentID, State: "active", Stats: &fleetcapture.TargetStats{}}}}
		ops[req.ID] = op
		f.last = req.ID
		reply(w, op)
	})
	operation := func(w http.ResponseWriter, r *http.Request) *fleetcapture.Operation {
		op := ops[r.PathValue("id")]
		if op == nil {
			http.Error(w, `{"error":{"code":"not_found","message":"no capture"}}`, http.StatusNotFound)
		}
		return op
	}
	mux.HandleFunc("GET "+base+"captures/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if op := operation(w, r); op != nil {
			reply(w, op)
		}
	})
	mux.HandleFunc("POST "+base+"captures/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if op := operation(w, r); op != nil {
			if !op.Terminal() {
				op.State, op.Revision, op.Targets[0].State = "stopped", op.Revision+1, "completed"
			}
			reply(w, op)
		}
	})
	mux.HandleFunc("GET /api/v1/groups", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		items := []any{group}
		for _, g := range page(f.manage.groups, r)["items"].([]doc) {
			items = append(items, g)
		}
		reply(w, doc{"items": items})
	})
	mux.HandleFunc("GET /api/v1/groups/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if g := f.manage.groups[r.PathValue("id")]; g != nil {
			reply(w, g)
			return
		}
		if r.PathValue("id") != Group {
			http.Error(w, `{"error":{"code":"not_found","message":"no group"}}`, http.StatusNotFound)
			return
		}
		reply(w, group)
	})
	mux.HandleFunc("GET /api/v1/groups/{id}/agents", func(w http.ResponseWriter, r *http.Request) {
		reply(w, fleetcapture.AgentPage{Items: []fleetcapture.Agent{{ID: Peer.AgentID, Connected: true}}})
	})
	mux.HandleFunc("GET /api/v1/agents/{id}/config", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.PathValue("id") != Peer.AgentID {
			http.Error(w, `{"error":{"code":"not_found","message":"no agent"}}`, http.StatusNotFound)
			return
		}
		view := fleetcapture.AgentConfig{AgentID: Peer.AgentID}
		if n := len(f.bodies); n > 0 {
			view.Desired = &fleetcapture.DesiredConfig{Hash: "hash", Layers: []fleetcapture.DesiredLayer{{Name: ConfigEntry, AssignmentID: "assignment", ConfigID: Config, Version: n}}}
		}
		reply(w, view)
	})
	version := func(i int) fleetcapture.ConfigVersion {
		return fleetcapture.ConfigVersion{ConfigID: Config, Version: i + 1, Message: fmt.Sprintf("version %d", i+1), CreatedAt: time.Date(2026, 10, 1, i, 0, 0, 0, time.UTC)}
	}
	mux.HandleFunc("GET /api/v1/configs/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if c := f.manage.configs[r.PathValue("id")]; c != nil {
			reply(w, c)
			return
		}
		if r.PathValue("id") != Config || len(f.bodies) == 0 {
			http.Error(w, `{"error":{"code":"not_found","message":"no config"}}`, http.StatusNotFound)
			return
		}
		reply(w, fleetcapture.StoredConfig{ID: Config, Name: "Pipeline", LatestVersion: len(f.bodies)})
	})
	mux.HandleFunc("GET /api/v1/configs/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if vs, ok := f.manage.versions[r.PathValue("id")]; ok {
			items := []doc{}
			for i := len(vs) - 1; i >= 0; i-- {
				v := doc{}
				for k, x := range vs[i] {
					if k != "body" {
						v[k] = x
					}
				}
				items = append(items, v)
			}
			reply(w, doc{"items": items})
			return
		}
		page := fleetcapture.ConfigVersionPage{Items: []fleetcapture.ConfigVersion{}}
		for i := len(f.bodies) - 1; i >= 0 && r.PathValue("id") == Config; i-- {
			page.Items = append(page.Items, version(i))
		}
		reply(w, page)
	})
	mux.HandleFunc("GET /api/v1/configs/{id}/versions/{version}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var n int
		_, err := fmt.Sscan(r.PathValue("version"), &n)
		if vs, ok := f.manage.versions[r.PathValue("id")]; ok && err == nil && n >= 1 && n <= len(vs) {
			reply(w, vs[n-1])
			return
		}
		if err != nil || r.PathValue("id") != Config || n < 1 || n > len(f.bodies) {
			http.Error(w, `{"error":{"code":"not_found","message":"no version"}}`, http.StatusNotFound)
			return
		}
		v := version(n - 1)
		v.Body = f.bodies[n-1]
		reply(w, v)
	})
	f.mount(mux)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+Token {
			http.Error(w, `{"error":{"code":"unauthorized","message":"bad token"}}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

// Deliver queues an OTLP/protobuf batch of signal from Peer for the latest
// capture and returns its batch ID.
func (f *Server) Deliver(signal string, data []byte, records int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("%032x", f.nextID)
	f.queue = append(f.queue, fleetcapture.Envelope{
		AuthorityID: Peer.AuthorityID, PluginInstance: Peer.PluginInstance, PluginEpoch: "epoch", BatchID: id, CaptureID: f.last,
		PeerID: Peer.ID, Generation: Peer.Generation, Signal: signal, Records: records, Encoding: "otlp-protobuf", Data: data, ObservedAt: time.Now(),
	})
	return id
}

// Acked reports the records the debugger accepted of a batch, once it has
// acknowledged it.
func (f *Server) Acked(batch string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.acks[batch]
	return n, ok
}

// ManageConfig makes fleet send Peer's agent one config layer, Config, whose
// versions are bodies, oldest first; the agent receives the newest.
func (f *Server) ManageConfig(bodies ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = bodies
}
