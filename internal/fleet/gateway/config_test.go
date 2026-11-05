package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

const acceptsRemoteConfig = uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus | protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
	protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig)

func TestReportingOnlyNeverSendsRemoteConfig(t *testing.T) {
	_, _, url, token := setup(t, 2, 2)
	uid := bytes.Repeat([]byte{7}, 16)
	c := dial(t, url, token)
	// exchange fails on any remote_config or capability beyond AcceptsStatus.
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 1, Capabilities: acceptsRemoteConfig,
		RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: []byte{1, 2}, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED}})
	exchange(t, c, &protobufs.AgentToServer{InstanceUid: uid, SequenceNum: 2, Capabilities: acceptsRemoteConfig,
		RemoteConfigStatus: &protobufs.RemoteConfigStatus{LastRemoteConfigHash: []byte{3}, Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}})
}

// source is an in-memory configmgmt.Source.
type source struct {
	mu          sync.Mutex
	targets     []configmgmt.Target
	bodies      map[int]string
	credentials map[string]map[string]string
}

func (s *source) Targets(context.Context) ([]configmgmt.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets, nil
}

func (s *source) GetVersion(_ context.Context, configID string, version int, _ bool) (configmgmt.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.bodies[version]
	if !ok {
		return configmgmt.Version{}, fleet.ErrNotFound
	}
	return configmgmt.Version{ConfigID: configID, Version: version, Body: &body}, nil
}

func (s *source) ManagedCredentials(context.Context) (map[string]map[string]string, error) {
	return s.credentials, nil
}

func (s *source) assign(t *testing.T, agentID string, version int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := sha256.Sum256([]byte(s.bodies[version]))
	target, err := configmgmt.NewTarget(configmgmt.Assignment{ID: "a1", Name: "base", ConfigID: "c1", Version: version, Layer: 10}, digest[:], "", []string{agentID})
	if err != nil {
		t.Fatal(err)
	}
	s.targets = []configmgmt.Target{target}
}

// holds is an in-memory ConfigStore with compare-and-set semantics.
type holds struct {
	mu   sync.Mutex
	held map[string]string
	sets map[string]configmgmt.Components
}

func (h *holds) ComponentSet(_ context.Context, hash string) (configmgmt.Components, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.sets[hash]; ok {
		return c, nil
	}
	return nil, fleet.ErrNotFound
}

func (h *holds) SaveComponentSet(_ context.Context, hash string, c configmgmt.Components) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sets == nil {
		h.sets = map[string]configmgmt.Components{}
	}
	if _, ok := h.sets[hash]; !ok {
		h.sets[hash] = c
	}
	return nil
}

func (h *holds) SetConfigHold(_ context.Context, id string, _ uint64, expected, next string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held[id] != expected {
		return h.held[id], nil
	}
	h.held[id] = next
	return next, nil
}

func (h *holds) ConfigHolds(context.Context) (map[string]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]string{}
	for id, v := range h.held {
		if v != "" {
			out[id] = v
		}
	}
	return out, nil
}

func (h *holds) get(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[id]
}

func read(t *testing.T, c *websocket.Conn, wait time.Duration) *protobufs.ServerToAgent {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	_, b, err := c.ReadMessage()
	if err != nil {
		return nil
	}
	response := &protobufs.ServerToAgent{}
	if len(b) == 0 || b[0] != 0 || proto.Unmarshal(b[1:], response) != nil {
		t.Fatal("malformed frame")
	}
	if response.Command != nil || response.PackagesAvailable != nil || response.ConnectionSettings != nil {
		t.Fatal("unexpected management command")
	}
	return response
}

func status(hash []byte, s protobufs.RemoteConfigStatuses) *protobufs.RemoteConfigStatus {
	return &protobufs.RemoteConfigStatus{LastRemoteConfigHash: hash, Status: s}
}

// agent drives one OpAMP session. A read deadline breaks a gorilla
// connection, so absence is checked by ordering instead: after a finished
// sweep, the next frame must be the reply to a heartbeat.
type agent struct {
	t   *testing.T
	g   *Gateway
	c   *websocket.Conn
	uid []byte
	seq uint64
}

func (a *agent) report(caps uint64, st *protobufs.RemoteConfigStatus) *protobufs.ServerToAgent {
	a.t.Helper()
	return a.exchange(&protobufs.AgentToServer{Capabilities: caps, RemoteConfigStatus: st})
}

// exchange sends m with the agent's UID and next sequence number. A
// session's first message is a full state, as from opamp-go.
func (a *agent) exchange(m *protobufs.AgentToServer) *protobufs.ServerToAgent {
	a.t.Helper()
	a.seq++
	if a.seq == 1 && m.AgentDescription == nil {
		m.AgentDescription = &protobufs.AgentDescription{}
	}
	m.InstanceUid, m.SequenceNum = a.uid, a.seq
	send(a.t, a.c, m)
	r := read(a.t, a.c, 3*time.Second)
	if r == nil {
		a.t.Fatal("no reply")
	}
	return r
}

// changed signals desired state changed and waits for the sweep.
func (a *agent) changed() {
	a.t.Helper()
	n := a.g.config.sweeps.Load()
	a.g.ConfigChanged("")
	waitFor(a.t, func() bool { return a.g.config.sweeps.Load() > n })
}

func (a *agent) pushed() *protobufs.ServerToAgent {
	a.t.Helper()
	a.changed()
	r := read(a.t, a.c, 3*time.Second)
	if r == nil || r.RemoteConfig == nil {
		a.t.Fatal("expected a pushed config")
	}
	return r
}

func (a *agent) quiet(caps uint64) {
	a.t.Helper()
	a.changed()
	if r := a.report(caps, nil); r.RemoteConfig != nil {
		a.t.Fatal("unexpected config push")
	}
}

func serve(t *testing.T, src *source, hs *holds) (*Gateway, string, string) {
	t.Helper()
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	g, err := New(fleet.NewStore(4), a, 4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	g.SetConfigManagement(configmgmt.NewReconciler(src, 1<<20), hs)
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	return g, "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp", token
}

func TestConfigDeliveryAndFailureHold(t *testing.T) {
	uid := bytes.Repeat([]byte{9}, 16)
	id := hex.EncodeToString(uid)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n", 2: "receivers: {otlp: {protocols: {grpc: {}}}}\n"},
		credentials: map[string]map[string]string{"memory-agent": nil}}
	src.assign(t, id, 1)
	hs := &holds{held: map[string]string{}}
	g, url, token := serve(t, src, hs)
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}

	r := a.report(acceptsRemoteConfig, nil)
	want := configmgmt.Hash([]configmgmt.Entry{{Name: "10-base", Body: []byte(src.bodies[1])}})
	if r.Capabilities&uint64(protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig) == 0 || r.RemoteConfig == nil ||
		!bytes.Equal(r.RemoteConfig.ConfigHash, want) || string(r.RemoteConfig.Config.ConfigMap["10-base"].Body) != src.bodies[1] {
		t.Fatalf("connect did not deliver the desired config: %v", r)
	}

	// Applying, then failed: the hash is held and never sent again.
	if a.report(acceptsRemoteConfig, status(want, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING)).RemoteConfig != nil {
		t.Fatal("applying hash was sent again")
	}
	if a.report(acceptsRemoteConfig, status(want, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED)).RemoteConfig != nil {
		t.Fatal("failed hash was sent again")
	}
	if hs.get(id) != hex.EncodeToString(want) {
		t.Fatal("failed hash not held")
	}
	// A local rollback report keeps the hold and causes no resend.
	if a.report(acceptsRemoteConfig, status([]byte{1}, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)).RemoteConfig != nil {
		t.Fatal("held hash was sent after a rollback")
	}
	a.quiet(acceptsRemoteConfig)

	// A new version clears the hold and is pushed without waiting for the
	// agent's next message.
	src.assign(t, id, 2)
	next := configmgmt.Hash([]configmgmt.Entry{{Name: "10-base", Body: []byte(src.bodies[2])}})
	if r := a.pushed(); !bytes.Equal(r.RemoteConfig.ConfigHash, next) {
		t.Fatal("pushed the wrong config")
	}
	if hs.get(id) != "" {
		t.Fatal("hold not cleared by a new desired hash")
	}
	// The same hash is not pushed again before the agent reports it.
	a.quiet(acceptsRemoteConfig)
	// Applied: nothing more to send. Unassigning releases the agent with
	// the unmanaged entry, never an empty map.
	if a.report(acceptsRemoteConfig, status(next, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED)).RemoteConfig != nil {
		t.Fatal("applied hash was sent again")
	}
	src.mu.Lock()
	src.targets = nil
	src.mu.Unlock()
	if r := a.pushed(); len(r.RemoteConfig.Config.ConfigMap) != 1 || r.RemoteConfig.Config.ConfigMap[configmgmt.UnmanagedEntry] == nil {
		t.Fatal("unassigned agent was not released")
	}
}

func TestReconnectWaitsForFullStateBeforeResending(t *testing.T) {
	uid := bytes.Repeat([]byte{7}, 16)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n"}, credentials: map[string]map[string]string{"memory-agent": nil}}
	src.assign(t, hex.EncodeToString(uid), 1)
	g, url, token := serve(t, src, &holds{held: map[string]string{}})
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	want := a.report(acceptsRemoteConfig, nil).RemoteConfig.ConfigHash
	// The agent began applying, but its report reached a replica that died:
	// this one reconnects it with only pending changes, as opamp-go does.
	_ = a.c.Close()
	a.c = dial(t, url, token)
	r := a.report(acceptsRemoteConfig, nil)
	if r.RemoteConfig != nil || r.Flags&uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState) == 0 {
		t.Fatal("reconnect resent the config before the full state")
	}
	a.quiet(acceptsRemoteConfig)
	if r := a.exchange(&protobufs.AgentToServer{Capabilities: acceptsRemoteConfig, AgentDescription: &protobufs.AgentDescription{},
		RemoteConfigStatus: status(want, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING)}); r.RemoteConfig != nil {
		t.Fatal("applying config resent after the full state")
	}
}

func TestHoldClearedThroughTheAPIIsRetried(t *testing.T) {
	uid := bytes.Repeat([]byte{6}, 16)
	id := hex.EncodeToString(uid)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n"}, credentials: map[string]map[string]string{"memory-agent": nil}}
	src.assign(t, id, 1)
	hs := &holds{held: map[string]string{}}
	g, url, token := serve(t, src, hs)
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	want := a.report(acceptsRemoteConfig, nil).RemoteConfig.ConfigHash
	a.report(acceptsRemoteConfig, status(want, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING))
	// The supervisor rolled back to its previous config.
	a.report(acceptsRemoteConfig, status([]byte{1}, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED))
	if hs.get(id) != hex.EncodeToString(want) {
		t.Fatal("rollback not held")
	}
	// ClearHold writes the store and names the agent in its hint.
	hs.mu.Lock()
	hs.held[id] = ""
	hs.mu.Unlock()
	n := g.config.sweeps.Load()
	g.ConfigChanged(id)
	waitFor(t, func() bool { return g.config.sweeps.Load() > n })
	if r := read(t, a.c, 3*time.Second); r == nil || r.RemoteConfig == nil || !bytes.Equal(r.RemoteConfig.ConfigHash, want) {
		t.Fatal("cleared hold was not retried")
	}
}

func TestConfigDeliveryRequiresOptIn(t *testing.T) {
	uid := bytes.Repeat([]byte{8}, 16)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n"}, credentials: map[string]map[string]string{}}
	src.assign(t, hex.EncodeToString(uid), 1)
	g, url, token := serve(t, src, &holds{held: map[string]string{}})
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	// The credential has no remote_config opt-in.
	if a.report(acceptsRemoteConfig, nil).RemoteConfig != nil {
		t.Fatal("config sent without the credential opt-in")
	}
	a.quiet(acceptsRemoteConfig)
	// With the opt-in, an agent that does not accept remote config gets none.
	src.mu.Lock()
	src.credentials = map[string]map[string]string{"memory-agent": nil}
	src.mu.Unlock()
	if a.report(1, nil).RemoteConfig != nil {
		t.Fatal("config sent to an agent without AcceptsRemoteConfig")
	}
	a.quiet(1)
	// Once it accepts remote config, the opted-in agent gets its config.
	if a.report(acceptsRemoteConfig, nil).RemoteConfig == nil {
		t.Fatal("opted-in agent did not get its config")
	}
}

const reportsComponents = acceptsRemoteConfig | uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsAvailableComponents)

// build is an AvailableComponents report in the Collector's shape.
func build(hash []byte, receivers ...string) *protobufs.AvailableComponents {
	ac := &protobufs.AvailableComponents{Hash: hash}
	if len(receivers) > 0 {
		sub := map[string]*protobufs.ComponentDetails{}
		for _, r := range receivers {
			sub[r] = &protobufs.ComponentDetails{}
		}
		ac.Components = map[string]*protobufs.ComponentDetails{"receivers": {SubComponentMap: sub},
			"exporters": {SubComponentMap: map[string]*protobufs.ComponentDetails{"debug": {}}}}
	}
	return ac
}

func TestIncompatibleBuildIsWithheld(t *testing.T) {
	uid, other := bytes.Repeat([]byte{5}, 16), bytes.Repeat([]byte{4}, 16)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n", 2: "receivers: {filelog: {}}\n"},
		credentials: map[string]map[string]string{"memory-agent": nil}}
	src.assign(t, hex.EncodeToString(uid), 1)
	hs := &holds{held: map[string]string{}}
	g, url, token := serve(t, src, hs)
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	hash := []byte{0xab, 0xcd}
	asked := uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)

	// An unknown build is asked for in full, and nothing is sent until the
	// list arrives.
	r := a.exchange(&protobufs.AgentToServer{Capabilities: reportsComponents, AvailableComponents: build(hash)})
	if r.Flags&asked == 0 || r.RemoteConfig != nil {
		t.Fatalf("unknown build: flags %d, config %v", r.Flags, r.RemoteConfig != nil)
	}
	// The build lacks the otlp receiver: the list is stored, and the config
	// is withheld rather than sent to fail.
	if r := a.exchange(&protobufs.AgentToServer{Capabilities: reportsComponents, AvailableComponents: build(hash, "filelog")}); r.RemoteConfig != nil || r.Flags&asked != 0 {
		t.Fatal("config sent to an incompatible build")
	}
	if set, _ := hs.ComponentSet(context.Background(), "abcd"); len(set["receivers"]) != 1 || set["receivers"][0] != "filelog" || set["exporters"][0] != "debug" {
		t.Fatalf("component set not stored: %v", set)
	}
	a.quiet(reportsComponents)
	if g.config.withheld.Load() == 0 {
		t.Fatal("withheld config not counted")
	}

	// Another agent of the same build is not asked again.
	b := &agent{t: t, g: g, c: dial(t, url, token), uid: other}
	if r := b.exchange(&protobufs.AgentToServer{Capabilities: reportsComponents, AvailableComponents: build(hash)}); r.Flags&asked != 0 {
		t.Fatal("known build asked again")
	}
	if g.config.asked.Load() != 1 || g.config.stored.Load() != 1 {
		t.Fatalf("asked %d, stored %d", g.config.asked.Load(), g.config.stored.Load())
	}

	// A version using only available types is delivered.
	src.assign(t, hex.EncodeToString(uid), 2)
	if r := a.pushed(); string(r.RemoteConfig.Config.ConfigMap["10-base"].Body) != src.bodies[2] {
		t.Fatal("compatible config not delivered")
	}
}
