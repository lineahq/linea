package gateway

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func setup(t *testing.T, maxAgents, maxConnections int) (*Gateway, *fleet.Store, string, string) {
	t.Helper()
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	s := fleet.NewStore(maxAgents)
	g, err := New(s, a, maxConnections, 1024)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	return g, s, "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp", token
}

func dial(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	c, r, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
	if r != nil && r.Body != nil {
		_ = r.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func send(t *testing.T, c *websocket.Conn, m *protobufs.AgentToServer) {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := c.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...)); err != nil {
		t.Fatal(err)
	}
}

func exchange(t *testing.T, c *websocket.Conn, m *protobufs.AgentToServer) *protobufs.ServerToAgent {
	t.Helper()
	send(t, c, m)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, b, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	response := &protobufs.ServerToAgent{}
	if len(b) == 0 || b[0] != 0 {
		t.Fatal("missing wire header")
	}
	if err := proto.Unmarshal(b[1:], response); err != nil {
		t.Fatal(err)
	}
	if response.Command != nil || response.RemoteConfig != nil || response.PackagesAvailable != nil || response.ConnectionSettings != nil || response.Capabilities != 1 {
		t.Fatal("reporting server sent management commands or unsupported capabilities")
	}
	return response
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("timed out waiting for connection state")
}

func expectClosed(t *testing.T, c *websocket.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("expected connection rejection")
	}
}

func TestReportingLifecycle(t *testing.T) {
	g, s, url, token := setup(t, 2, 4)
	uid := bytes.Repeat([]byte{1}, 16)
	id := hex.EncodeToString(uid)
	c := dial(t, url, token)
	m := &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 10, Capabilities: 7,
		Health:           &protobufs.ComponentHealth{Healthy: true, LastError: "secret-health-details"},
		AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{{Key: "secret-host"}}},
		EffectiveConfig:  &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: []byte("secret-config")}}}},
	}
	r := exchange(t, c, m)
	if r.Flags != 1 {
		t.Fatal("initial full-state request missing")
	}
	a, _ := s.Get(id)
	observed := a.Health.ObservedAt
	m.SequenceNum = 11
	m.Health = nil
	m.Capabilities = 0
	r = exchange(t, c, m)
	a, _ = s.Get(id)
	if r.Flags != 0 || a.Capabilities != 0 || !a.Health.Healthy || !a.Health.ObservedAt.Equal(observed) {
		t.Fatal("incremental report corrupted state")
	}
	encoded, _ := json.Marshal(a)
	if bytes.Contains(encoded, []byte("secret")) {
		t.Fatal("sensitive fields entered inventory")
	}
	m.SequenceNum = 13
	if exchange(t, c, m).Flags != 1 {
		t.Fatal("gap did not request resync")
	}
	m.SequenceNum = 12
	m.Health = &protobufs.ComponentHealth{Healthy: false}
	m.Capabilities = 99
	exchange(t, c, m)
	a, _ = s.Get(id)
	if !a.Health.Healthy || a.Capabilities != 0 {
		t.Fatal("out-of-order report overwrote state")
	}
	// The newest connection wins; the older socket is fenced and closed.
	duplicate := dial(t, url, token)
	m.Health = nil
	m.Capabilities = 0
	exchange(t, duplicate, m)
	expectClosed(t, c)
	waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.connections) == 1 })
	a, _ = s.Get(id)
	if !a.Connected || a.Generation != 2 || a.IdentityConflict {
		t.Fatalf("takeover state incorrect: %+v", a)
	}
	_ = duplicate.Close()
	waitFor(t, func() bool { a, _ := s.Get(id); return !a.Connected })
	c2 := dial(t, url, token)
	m.SequenceNum = 1
	m.Capabilities = 1
	exchange(t, c2, m)
	a, _ = s.Get(id)
	if a.Generation != 3 || !a.Connected || a.Health == nil || !a.Health.ObservedAt.Equal(observed) {
		t.Fatal("reconnect state incorrect")
	}
	g.Close()
	expectClosed(t, c2)
	a, _ = s.Get(id)
	if a.Connected {
		t.Fatal("shutdown left agent connected")
	}
}

func TestGatewayAuthAndTransport(t *testing.T) {
	_, _, url, token := setup(t, 1, 1)
	endpoint := "http" + strings.TrimPrefix(url, "ws")
	for _, tc := range []struct {
		method, credential, origin string
		want                       int
	}{
		{"GET", "", "", 401}, {"GET", strings.Repeat("m", 32), "", 401},
		{"POST", token, "", 405}, {"GET", token, "https://example.com", 403}, {"GET", token, "", 426},
	} {
		r, _ := http.NewRequest(tc.method, endpoint, nil)
		r.Header.Set("Authorization", "Bearer "+tc.credential)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("got %d want %d", resp.StatusCode, tc.want)
		}
	}
	// A Content-Type must not redirect an otherwise valid upgrade into polling.
	c, resp, err := websocket.DefaultDialer.Dial(url, http.Header{
		"Authorization": {"Bearer " + token}, "Content-Type": {"application/x-protobuf"},
	})
	if c != nil {
		_ = c.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("ambiguous transport accepted: response=%v err=%v", resp, err)
	}
}

func TestGatewayLimitsAndInvalidMessages(t *testing.T) {
	for _, mode := range []string{"oversize", "invalid-uid", "changed-uid", "malformed", "inventory-capacity", "connection-capacity"} {
		t.Run(mode, func(t *testing.T) {
			g, _, url, token := setup(t, 1, 2)
			c := dial(t, url, token)
			uid := bytes.Repeat([]byte{2}, 16)
			m := &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1}
			switch mode {
			case "oversize":
				m.EffectiveConfig = &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: bytes.Repeat([]byte{1}, 2048)}}}}
				send(t, c, m)
			case "invalid-uid":
				m.InstanceUid = []byte{1}
				send(t, c, m)
			case "changed-uid":
				exchange(t, c, m)
				m.InstanceUid = bytes.Repeat([]byte{3}, 16)
				m.SequenceNum++
				send(t, c, m)
			case "malformed":
				if err := c.WriteMessage(websocket.BinaryMessage, []byte{0, 255}); err != nil {
					t.Fatal(err)
				}
			case "inventory-capacity":
				// Only disconnected agents are evicted; a connected one keeps its place.
				exchange(t, c, m)
				c = dial(t, url, token)
				m.InstanceUid = bytes.Repeat([]byte{3}, 16)
				send(t, c, m)
			case "connection-capacity":
				exchange(t, c, m)
				c2 := dial(t, url, token)
				m.InstanceUid = bytes.Repeat([]byte{3}, 16)
				// Keep second connection registered without identifying it.
				_ = c2
				waitFor(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return len(g.connections) == 2 })
				// Admission may reject HTTP or close immediately after upgrade.
				c3, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if err != nil {
					if resp == nil || resp.StatusCode != 503 {
						t.Fatalf("unexpected admission error: %v", err)
					}
					return
				}
				defer c3.Close()
				expectClosed(t, c3)
				return
			}
			expectClosed(t, c)
		})
	}
}

func TestCapacityEvictsLongestDisconnected(t *testing.T) {
	_, s, url, token := setup(t, 1, 2)
	first := bytes.Repeat([]byte{2}, 16)
	c := dial(t, url, token)
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: first, SequenceNum: 1})
	_ = c.Close()
	waitFor(t, func() bool { a, _ := s.Get(hex.EncodeToString(first)); return !a.Connected })
	c = dial(t, url, token)
	second := bytes.Repeat([]byte{3}, 16)
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: second, SequenceNum: 1})
	if _, ok := s.Get(hex.EncodeToString(first)); ok {
		t.Fatal("disconnected agent was not evicted")
	}
	if a, ok := s.Get(hex.EncodeToString(second)); !ok || !a.Connected {
		t.Fatal("new agent was not admitted")
	}
}

func TestAdmissionLimitRefusesBeforeAuthentication(t *testing.T) {
	g, _, url, token := setup(t, 3, 3)
	g.SetPendingSessions(1)
	g.admission.wait = 0
	// The first session holds the only slot until its first commit.
	dial(t, url, token)
	c, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + strings.Repeat("x", 32)}})
	if c != nil {
		_ = c.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("session storm admitted: response=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestAdmissionHoldsHandshakeUntilASlotFrees(t *testing.T) {
	g, _, url, token := setup(t, 3, 3)
	g.SetPendingSessions(1)
	g.admission.wait = 2 * time.Second
	first := dial(t, url, token)
	b, _ := proto.Marshal(&protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{6}, 16), SequenceNum: 1})
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = first.WriteMessage(websocket.BinaryMessage, append([]byte{0}, b...))
	}()
	started := time.Now()
	dial(t, url, token)
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("handshake was admitted before a slot freed: %v", elapsed)
	}
	if g.admissionHeld.Load() == 0 || g.admissionRefused.Load() != 0 {
		t.Fatalf("held=%d refused=%d", g.admissionHeld.Load(), g.admissionRefused.Load())
	}
}

func TestAdmissionWaitEndsOnDrain(t *testing.T) {
	g, _, url, token := setup(t, 3, 3)
	g.SetPendingSessions(1)
	g.admission.wait = time.Minute
	dial(t, url, token)
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		c, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
		if c != nil {
			_ = c.Close()
		}
		done <- result{resp, err}
	}()
	time.Sleep(300 * time.Millisecond) // the handshake is waiting for the slot
	g.Drain()
	select {
	case r := <-done:
		if r.err == nil || r.resp == nil || r.resp.StatusCode != 503 || r.resp.Header.Get("Retry-After") != "1" {
			t.Fatalf("draining replica answered a waiting handshake with %v, %v", r.resp, r.err)
		}
		_ = r.resp.Body.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("waiting handshake was not released by drain")
	}
	if g.admissionRefused.Load() != 0 {
		t.Fatal("drain was counted as an admission refusal")
	}
}

func TestAdmissionRefusalOutlastsTheListenerWriteTimeout(t *testing.T) {
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	g, err := New(fleet.NewStore(3), a, 3, 1024)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewUnstartedServer(g)
	h.Config.WriteTimeout = 200 * time.Millisecond
	h.Start()
	t.Cleanup(func() { g.Close(); h.Close() })
	url := "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp"
	g.SetPendingSessions(1)
	g.admission.wait = 500 * time.Millisecond
	dial(t, url, token)
	// Refused after waiting longer than the write timeout: the agent still
	// reads the 503 and its Retry-After instead of a reset connection.
	c, resp, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + token}})
	if c != nil {
		_ = c.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("refusal after a long wait: response=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestHandshakeSlotsAreReleased(t *testing.T) {
	g, _, url, token := setup(t, 3, 3)
	g.SetPendingSessions(1)
	g.admission.wait = 0
	pending := func() int { slots, _ := g.admission.state(); return len(slots) }
	// A refused credential ends the handshake without an upgrade.
	for range 3 {
		c, resp, _ := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": {"Bearer " + strings.Repeat("x", 32)}})
		if c != nil {
			_ = c.Close()
		}
		if resp == nil || resp.StatusCode != 401 {
			t.Fatalf("refused credential got %v", resp)
		}
		_ = resp.Body.Close()
	}
	// A session that closes before its first message frees its slot.
	c := dial(t, url, token)
	if pending() != 1 {
		t.Fatal("upgraded session did not hold its slot")
	}
	_ = c.Close()
	waitFor(t, func() bool { return pending() == 0 })
	// The first commit frees it; the session stays connected.
	c = dial(t, url, token)
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{7}, 16), SequenceNum: 1})
	if pending() != 0 {
		t.Fatal("committed session kept its slot")
	}
	dial(t, url, token)
}

func TestSpreadClosesSocketsOverWindow(t *testing.T) {
	g, s, url, token := setup(t, 3, 3)
	peers := []*websocket.Conn{dial(t, url, token), dial(t, url, token)}
	for i, c := range peers {
		exchange(t, c, &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{byte(i + 1)}, 16), SequenceNum: 1})
	}
	g.Drain()
	started := time.Now()
	g.Spread(context.Background(), 200*time.Millisecond)
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("spread closed all sockets at once: %v", elapsed)
	}
	for _, c := range peers {
		expectClosed(t, c)
	}
	waitFor(t, func() bool { summary, _ := s.Summary(context.Background(), ""); return summary.Connected == 0 })
}

func TestInventoryReport(t *testing.T) {
	_, s, url, token := setup(t, 2, 2)
	uid := bytes.Repeat([]byte{4}, 16)
	id := hex.EncodeToString(uid)
	c := dial(t, url, token)
	str := func(k, v string) *protobufs.KeyValue {
		return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}}
	}
	m := &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1,
		AgentDescription: &protobufs.AgentDescription{
			IdentifyingAttributes:    []*protobufs.KeyValue{str("service.name", "otelcol-contrib")},
			NonIdentifyingAttributes: []*protobufs.KeyValue{str("host.name", "node-1"), str("process.env", "SECRET=1")},
		},
		Health: &protobufs.ComponentHealth{Healthy: true, Status: "StatusOK", ComponentHealthMap: map[string]*protobufs.ComponentHealth{
			"pipeline:traces": {Healthy: true, ComponentHealthMap: map[string]*protobufs.ComponentHealth{"receiver:otlp": {Healthy: true, Status: "StatusOK"}}},
		}},
		EffectiveConfig: &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"": {Body: []byte("receivers: {otlp: {}}\nexporters: {debug: {verbosity: secret}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}\n")}}}},
	}
	exchange(t, c, m)
	a, _ := s.Get(id)
	if a.Attributes["service.name"] != "otelcol-contrib" || a.Attributes["host.name"] != "node-1" || len(a.Attributes) != 2 {
		t.Fatalf("attributes = %v", a.Attributes)
	}
	if len(a.Components) != 2 || a.Components[0].ID != "exporter:debug" || a.Components[1].Healthy == nil || len(a.ConfigHash) != 64 {
		t.Fatalf("components = %+v hash=%q", a.Components, a.ConfigHash)
	}
	hash := a.ConfigHash
	// Sections omitted from an incremental report keep their stored values.
	m.SequenceNum, m.AgentDescription, m.EffectiveConfig = 2, nil, nil
	m.Health = &protobufs.ComponentHealth{Healthy: false, Status: "StatusRecoverableError", ComponentHealthMap: map[string]*protobufs.ComponentHealth{
		"pipeline:traces": {Healthy: false, ComponentHealthMap: map[string]*protobufs.ComponentHealth{"receiver:otlp": {Healthy: false, Status: "StatusRecoverableError", LastError: "secret"}}},
	}}
	exchange(t, c, m)
	a, _ = s.Get(id)
	if a.Attributes["service.name"] != "otelcol-contrib" || a.ConfigHash != hash || len(a.Components) != 2 {
		t.Fatalf("incremental report lost inventory: %+v", a)
	}
	if a.Health.Healthy || a.Health.Status != "StatusRecoverableError" || a.Health.ComponentsUnhealthy != 1 || a.Components[0].Pipelines[0] != "traces" {
		t.Fatalf("health = %+v components = %+v", a.Health, a.Components)
	}
	if encoded, _ := json.Marshal(a); bytes.Contains(encoded, []byte("secret")) || bytes.Contains(encoded, []byte("SECRET")) {
		t.Fatal("sensitive fields entered inventory")
	}
}

// flaky admits like a, but its credential check can fail like a database
// outage.
type flaky struct {
	auth.Authorizer
	down atomic.Bool
}

func (f *flaky) Active(ctx context.Context, ps []auth.Principal) (map[string]bool, error) {
	if f.down.Load() {
		return nil, fleet.ErrUnavailable
	}
	return f.Authorizer.Active(ctx, ps)
}

func TestCredentialSweepKeepsSessionsThroughBlips(t *testing.T) {
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	access := &flaky{Authorizer: auth.Static{Bearer: a, Role: "agent"}}
	g, err := NewAuthorized(fleet.NewStore(2), access, nil, 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	c := dial(t, "ws"+strings.TrimPrefix(h.URL, "http")+"/v1/opamp", token)
	m := &protobufs.AgentToServer{InstanceUid: bytes.Repeat([]byte{4}, 16), SequenceNum: 1}
	exchange(t, c, m)

	// A failed check keeps the session; the sweep runs every 2 s.
	access.down.Store(true)
	time.Sleep(2500 * time.Millisecond)
	m.SequenceNum++
	exchange(t, c, m)

	// Past the grace period an unverifiable session is closed.
	g.verifyGrace.Store(0)
	expectClosedWithin(t, c, 3*time.Second)
}

func expectClosedWithin(t *testing.T, c *websocket.Conn, wait time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("unverified session stayed open")
	}
}

// writes counts the inventory writes after a connect.
type writes struct {
	*fleet.Store
	observed atomic.Int64
}

func (w *writes) ObserveAgent(ctx context.Context, id, credential string, generation uint64, report fleet.Report, now time.Time) error {
	w.observed.Add(1)
	return w.Store.ObserveAgent(ctx, id, credential, generation, report, now)
}

func TestReconnectWithUnchangedStateWritesOnce(t *testing.T) {
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	store := &writes{Store: fleet.NewStore(2)}
	g, err := New(store, a, 2, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	url := "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp"
	uid := bytes.Repeat([]byte{5}, 16)
	full := &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: 7,
		AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "otelcol-contrib"}}}}},
		Health: &protobufs.ComponentHealth{Healthy: true, Status: "StatusOK", ComponentHealthMap: map[string]*protobufs.ComponentHealth{
			"pipeline:traces": {Healthy: true, ComponentHealthMap: map[string]*protobufs.ComponentHealth{"receiver:otlp": {Healthy: true, Status: "StatusOK"}}},
		}},
		EffectiveConfig: &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"": {Body: []byte("receivers: {otlp: {}}\nexporters: {debug: {}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}\n")}}}},
	}
	c := dial(t, url, token)
	exchange(t, c, full)
	_ = c.Close()
	waitFor(t, func() bool { a, _ := store.Get(hex.EncodeToString(uid)); return !a.Connected })

	// opamp-go reconnects with an empty message, then sends its full state
	// when the server asks for it. Unchanged state needs no second write.
	c = dial(t, url, token)
	if r := exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2, Capabilities: 7}); r.Flags != 1 {
		t.Fatal("reconnect did not request full state")
	}
	full.SequenceNum = 3
	exchange(t, c, full)
	if n := store.observed.Load(); n != 0 {
		t.Fatalf("unchanged full state wrote %d times", n)
	}
	full.SequenceNum, full.Health.Healthy = 4, false
	exchange(t, c, full)
	if n := store.observed.Load(); n != 1 {
		t.Fatalf("changed health wrote %d times", n)
	}
}
