// Package gateway adapts OpAMP reporting to the independent fleet core.
package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/inventory"
	"github.com/lineahq/linea/pluginsdk/metrics"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
)

type session struct {
	mu                   sync.Mutex
	conn                 types.Connection
	principal            auth.Principal
	id                   string
	generation, sequence uint64
	identified           bool
	closing              bool
	writes               chan struct{}
	// admission slot, released at the first commit or when the session ends
	handshake *handshake
	// last committed report, used to skip unchanged writes
	report fleet.Report
	// component sources of the committed report, merged on every change
	health, config map[string]*fleet.Component
	// config management only: the instance UID, the stored failure hold
	// (hex) and the last hash sent, to avoid repeating it before it is
	// reported
	uid       []byte
	held      string
	offered   []byte
	offeredAt time.Time
	// config management only: the component set of the reported build,
	// the hash it was resolved for, the hash last asked for in full, and
	// since when delivery waits for that list
	components      configmgmt.Components
	componentsHash  string
	componentsAsked string
	awaiting        time.Time
	// config management only: since when delivery waits for the full state
	// asked for after a reconnect's first message
	stateAwaited time.Time
}

// agentEntry records which local socket holds an agent identity.
type agentEntry struct {
	conn       types.Connection
	generation uint64
}

// verifyGrace bounds how long sessions stay open while their credentials
// cannot be checked. It exceeds the replica liveness window, so fencing
// normally closes them first.
const verifyGrace = 30 * time.Second

// busy is implemented by stores with writer backpressure.
type busy interface{ Busy() bool }

type principalKey struct{}
type handshakeKey struct{}
type Gateway struct {
	store            fleet.Repository
	access           auth.Authorizer
	verifyGrace      atomic.Int64 // nanoseconds
	ingest           *inventory.Ingest
	maxConnections   int
	mu               sync.Mutex
	connections      map[types.Connection]*session
	agents           map[string]agentEntry
	admission        *admission
	closed           bool
	draining         bool
	handler          http.Handler
	ctx              context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	reportDuration   metrics.Duration
	reportErrors     atomic.Uint64
	admissionRefused atomic.Uint64
	admissionHeld    atomic.Uint64
	// config is nil unless config management is enabled.
	config *configState
}

func New(store fleet.Repository, credential auth.Bearer, maxConnections int, maxMessageBytes int64) (*Gateway, error) {
	return NewAuthorized(store, auth.Static{Bearer: credential, Role: "agent"}, nil, maxConnections, maxMessageBytes)
}

// NewAuthorized serves agents admitted by access. A nil ingest applies the
// built-in attribute allowlist without health error text.
func NewAuthorized(store fleet.Repository, access auth.Authorizer, ingest *inventory.Ingest, maxConnections int, maxMessageBytes int64) (*Gateway, error) {
	if ingest == nil {
		ingest, _ = inventory.New(nil, false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g := &Gateway{store: store, access: access, ingest: ingest, maxConnections: maxConnections, connections: make(map[types.Connection]*session), agents: map[string]agentEntry{}, admission: newAdmission(DefaultPendingSessions), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s := server.New(nil)
	h, _, err := s.Attach(server.Settings{MaxMessageSize: maxMessageBytes, Callbacks: types.Callbacks{OnConnecting: g.connecting}})
	if err != nil {
		cancel()
		return nil, err
	}
	g.handler = http.HandlerFunc(h)
	g.verifyGrace.Store(int64(verifyGrace))
	go g.watch()
	return g, nil
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	g.mu.Lock()
	draining := g.draining
	g.mu.Unlock()
	if draining {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "fleet is draining", http.StatusServiceUnavailable)
		return
	}
	// Admission runs before credential checks so storms stay cheap.
	h, held, admitted := g.admission.acquire(r.Context())
	if held {
		g.admissionHeld.Add(1)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWrite))
	}
	if !admitted {
		if r.Context().Err() != nil {
			return
		}
		g.mu.Lock()
		draining := g.draining || g.closed
		g.mu.Unlock()
		if draining {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "fleet is draining", http.StatusServiceUnavailable)
			return
		}
		g.admissionRefused.Add(1)
		w.Header().Set("Retry-After", retryAfter())
		http.Error(w, "fleet admission limit reached", http.StatusServiceUnavailable)
		return
	}
	// A hijacked connection takes the slot with it; otherwise the handshake
	// ended here.
	hw := &hijackWriter{ResponseWriter: w}
	defer func() {
		if !hw.hijacked {
			h.release()
		}
	}()
	w = hw
	if held {
		g.mu.Lock()
		draining := g.draining || g.closed
		g.mu.Unlock()
		if draining {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "fleet is draining", http.StatusServiceUnavailable)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	p, err := g.access.Authenticate(ctx, r)
	if err != nil || p.Role != "agent" {
		if err != nil && !errors.Is(err, auth.ErrUnauthorized) {
			http.Error(w, "fleet storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "valid agent credential required", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/opamp" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "WebSocket transport required", http.StatusMethodNotAllowed)
		return
	}
	if len(r.Header.Values("Origin")) > 0 {
		http.Error(w, "browser origins unsupported", http.StatusForbidden)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) || r.Header.Get("Content-Type") == "application/x-protobuf" {
		http.Error(w, "WebSocket transport required", http.StatusUpgradeRequired)
		return
	}
	ctx = context.WithValue(context.WithValue(r.Context(), principalKey{}, p), handshakeKey{}, h)
	g.handler.ServeHTTP(w, r.WithContext(ctx))
}

// hijackWriter records whether the WebSocket upgrade took the connection.
type hijackWriter struct {
	http.ResponseWriter
	hijacked bool
}

func (w *hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	w.hijacked = err == nil
	return c, rw, err
}

func (w *hijackWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (g *Gateway) connecting(r *http.Request) types.ConnectionResponse {
	g.mu.Lock()
	full := g.closed || g.draining || len(g.connections) >= g.maxConnections
	g.mu.Unlock()
	if b, ok := g.store.(busy); ok && b.Busy() {
		full = true
	}
	if full {
		g.admissionRefused.Add(1)
		return types.ConnectionResponse{HTTPStatusCode: 503, HTTPResponseHeader: map[string]string{"Retry-After": retryAfter()}}
	}
	p, ok := r.Context().Value(principalKey{}).(auth.Principal)
	if !ok {
		return types.ConnectionResponse{HTTPStatusCode: 401}
	}
	h, _ := r.Context().Value(handshakeKey{}).(*handshake)
	return types.ConnectionResponse{Accept: true, ConnectionCallbacks: types.ConnectionCallbacks{
		OnConnected: func(_ context.Context, c types.Connection) {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.closed || g.draining || len(g.connections) >= g.maxConnections {
				g.admissionRefused.Add(1)
				h.release()
				disconnect(c)
				return
			}
			_ = c.Connection().SetReadDeadline(time.Now().Add(10 * time.Second))
			g.connections[c] = &session{conn: c, principal: p, writes: make(chan struct{}, 1), handshake: h}
		},
		OnMessage: g.respond, OnConnectionClose: g.disconnected,
		OnReadMessageError: func(c types.Connection, _ int, _ []byte, _ error) { disconnect(c) },
	}}
}
func (g *Gateway) message(_ context.Context, c types.Connection, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
	started := time.Now()
	accepted := false
	defer func() {
		g.reportDuration.Observe(time.Since(started))
		if !accepted {
			g.reportErrors.Add(1)
		}
	}()
	g.mu.Lock()
	s, ok := g.connections[c]
	closed := g.closed
	g.mu.Unlock()
	if !ok || closed {
		disconnect(c)
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g.ctx.Err() != nil || len(m.InstanceUid) != 16 || m.Flags != 0 {
		disconnect(c)
		return nil
	}
	id := hex.EncodeToString(m.InstanceUid)
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer cancel()
	previous := statusOf(s.report.RemoteConfig)
	report, health, config := g.next(s, m)
	response := &protobufs.ServerToAgent{InstanceUid: m.InstanceUid, Capabilities: g.capabilities()}
	first := !s.identified
	// evaluate is set when config delivery must re-run the send rule.
	evaluate := first
	if first {
		generation, stored, err := g.store.ConnectAgent(ctx, id, s.principal.ID, report, time.Now())
		s.handshake.release()
		if err != nil {
			disconnect(c)
			return nil
		}
		s.id, s.generation, s.uid = id, generation, slices.Clone(m.InstanceUid)
		if rc := stored.RemoteConfig; rc != nil {
			// The hold is kept beside the report; only SetConfigHold and
			// ClearHold write it.
			s.held = rc.Held
			copied := *rc
			copied.Held = ""
			stored.RemoteConfig = &copied
		}
		// The cache starts from the stored report: after a reconnect's empty
		// first message, an unchanged full state then needs no write.
		s.commit(stored, health, config)
		g.claim(s)
		_ = c.Connection().SetReadDeadline(time.Time{})
		response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	} else if s.id != id {
		disconnect(c)
		return nil
	} else if m.SequenceNum <= s.sequence {
		accepted = true
		return response
	} else {
		if m.SequenceNum != s.sequence+1 {
			response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
		}
		if changes := fleet.Changes(s.report, report); len(changes) > 0 {
			err := g.store.ObserveAgent(ctx, id, s.principal.ID, s.generation, report, time.Now())
			if errors.Is(err, fleet.ErrBusy) {
				// Keep the cached report so the next full state retries the write.
				response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
				accepted = true
				return response
			}
			if err != nil {
				disconnect(c)
				return nil
			}
			s.commit(report, health, config)
			for _, group := range changes {
				evaluate = evaluate || group == "capabilities" || group == "attributes" || group == "config"
			}
		} else {
			g.store.SeenAgent(id, s.generation, time.Now())
		}
	}
	s.identified, s.sequence = true, m.SequenceNum
	accepted = true
	if m.AgentDisconnect != nil {
		s.closing = true
		disconnect(c)
		return nil
	}
	if g.config != nil {
		if first && m.AgentDescription == nil && m.RemoteConfigStatus == nil {
			// A reconnect's first message carries only pending changes, and
			// the stored status can predate what the agent last reported to
			// another replica: delivery waits for the full state asked for.
			s.stateAwaited = time.Now()
		} else if !s.stateAwaited.IsZero() && (m.AgentDescription != nil || m.RemoteConfigStatus != nil) {
			s.stateAwaited, evaluate = time.Time{}, true
		}
		ask, changed := g.components(ctx, s, m.AvailableComponents)
		if ask {
			response.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
		}
		evaluate = evaluate || changed
	}
	if g.config != nil && evaluate {
		current := statusOf(s.report.RemoteConfig)
		if remote := g.deliver(ctx, s, previous, first || !sameStatus(previous, current)); remote != nil {
			response.RemoteConfig = remote
			g.config.responses.Add(1)
		}
	}
	return response
}

// next builds the full report from the committed state and the sections this
// message carries; OpAMP omits unchanged sections. Raw descriptions, health
// text and configuration are reduced by the inventory allowlist here.
func (g *Gateway) next(s *session, m *protobufs.AgentToServer) (fleet.Report, map[string]*fleet.Component, map[string]*fleet.Component) {
	report, health, config := s.report, s.health, s.config
	report.Capabilities = m.Capabilities
	if m.Health != nil {
		healthy := m.Health.Healthy
		var status fleet.Status
		status, health = g.ingest.Health(m.Health)
		report.Healthy, report.Status = &healthy, &status
	}
	if m.AgentDescription != nil {
		report.Attributes = g.ingest.Attributes(m.AgentDescription)
	}
	if m.EffectiveConfig != nil {
		report.ConfigHash, config = g.ingest.Config(m.EffectiveConfig)
	}
	if m.Health != nil || m.EffectiveConfig != nil {
		report.Components = inventory.Merge(config, health)
	}
	if m.RemoteConfigStatus != nil {
		rc := g.ingest.RemoteConfig(m.RemoteConfigStatus)
		report.RemoteConfig = &rc
	}
	if ac := m.AvailableComponents; ac != nil && len(ac.Hash) > 0 && len(ac.Hash) <= configmgmt.MaxComponentsHash {
		report.AvailableComponents = hex.EncodeToString(ac.Hash)
	}
	return report, health, config
}

// commit records a report the store accepted.
func (s *session) commit(report fleet.Report, health, config map[string]*fleet.Component) {
	s.report, s.health, s.config = report, health, config
}

// claim records the newest local holder of an identity and closes an older
// local socket immediately; remote holders close on the takeover hint.
func (g *Gateway) claim(s *session) {
	g.mu.Lock()
	previous, ok := g.agents[s.id]
	if !ok || previous.generation < s.generation {
		g.agents[s.id] = agentEntry{conn: s.conn, generation: s.generation}
	}
	g.mu.Unlock()
	if ok && previous.conn != s.conn && previous.generation < s.generation {
		disconnect(previous.conn)
	}
}

func (g *Gateway) WriteMetrics(w io.Writer) {
	g.mu.Lock()
	connections := len(g.connections)
	g.mu.Unlock()
	fmt.Fprintf(w, "# TYPE linea_fleet_agent_connections gauge\nlinea_fleet_agent_connections %d\n# TYPE linea_fleet_agent_connection_limit gauge\nlinea_fleet_agent_connection_limit %d\n# TYPE linea_fleet_agent_admission_refused_total counter\nlinea_fleet_agent_admission_refused_total %d\n# TYPE linea_fleet_agent_admission_held_total counter\nlinea_fleet_agent_admission_held_total %d\n# TYPE linea_fleet_report_errors_total counter\nlinea_fleet_report_errors_total %d\n", connections, g.maxConnections, g.admissionRefused.Load(), g.admissionHeld.Load(), g.reportErrors.Load())
	slots, _ := g.admission.state()
	fmt.Fprintf(w, "# TYPE linea_fleet_agent_handshakes_pending gauge\nlinea_fleet_agent_handshakes_pending %d\n# TYPE linea_fleet_agent_handshake_limit gauge\nlinea_fleet_agent_handshake_limit %d\n", len(slots), cap(slots))
	fmt.Fprintf(w, "# TYPE linea_fleet_attributes_dropped_total counter\nlinea_fleet_attributes_dropped_total %d\n# TYPE linea_fleet_config_summaries_skipped_total counter\nlinea_fleet_config_summaries_skipped_total %d\n", g.ingest.Dropped(), g.ingest.Unsummarized())
	g.reportDuration.Snapshot().Write(w, "linea_fleet_report_duration_seconds")
	g.writeConfigMetrics(w)
}
func (g *Gateway) disconnected(c types.Connection) {
	g.mu.Lock()
	s, ok := g.connections[c]
	delete(g.connections, c)
	g.mu.Unlock()
	if !ok {
		return
	}
	s.handshake.release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != "" {
		g.mu.Lock()
		if e, ok := g.agents[s.id]; ok && e.conn == c {
			delete(g.agents, s.id)
		}
		g.mu.Unlock()
		s.closing = true
		ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
		defer cancel()
		_ = g.store.DisconnectAgent(ctx, s.id, s.generation, time.Now())
	}
}
func (g *Gateway) watch() {
	defer close(g.done)
	// Revocation hints close sockets at once; this sweep covers lost hints
	// and expiry.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	verified := time.Now()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
		g.mu.Lock()
		principals := make([]auth.Principal, 0, len(g.connections))
		peers := make(map[types.Connection]string, len(g.connections))
		for c, s := range g.connections {
			principals = append(principals, s.principal)
			peers[c] = s.principal.ID
		}
		g.mu.Unlock()
		if len(principals) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
		active, err := g.access.Active(ctx, principals)
		cancel()
		if err != nil {
			// A database blip must not drop every session (architecture
			// §5.7); a longer outage fences the replica, which closes them.
			// Unverified sessions are closed after the grace period anyway.
			if time.Since(verified) < time.Duration(g.verifyGrace.Load()) {
				continue
			}
			active = nil
		} else {
			verified = time.Now()
		}
		for c, id := range peers {
			if !active[id] {
				disconnect(c)
			}
		}
	}
}

// Close fences admission, cancels publication, closes hijacked sockets and
// persists one bounded bulk shutdown rather than N blocking writes.
func (g *Gateway) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.cancel()
	g.admission.close()
	peers := g.connections
	g.connections = make(map[types.Connection]*session)
	g.agents = map[string]agentEntry{}
	g.mu.Unlock()
	for c := range peers {
		disconnect(c)
	}
	for _, s := range peers {
		s.handshake.release()
		s.mu.Lock()
		s.mu.Unlock()
	}
	<-g.done
	if g.config != nil {
		<-g.config.done
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return g.store.StopAgents(ctx)
}
func disconnect(c types.Connection) {
	// TLS close_notify must not serially stall reporting or shutdown cleanup.
	if secured, ok := c.Connection().(*tls.Conn); ok {
		_ = secured.NetConn().Close()
	}
	_ = c.Disconnect()
}
