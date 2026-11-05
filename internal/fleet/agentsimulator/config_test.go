package agentsimulator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/gateway"
)

// managed serves one assignment for every simulated agent and keeps holds
// and component sets in memory.
type managed struct {
	mu     sync.Mutex
	target configmgmt.Target
	body   string
	held   map[string]string
	sets   map[string]configmgmt.Components
}

func (m *managed) Targets(context.Context) ([]configmgmt.Target, error) {
	return []configmgmt.Target{m.target}, nil
}

func (m *managed) GetVersion(_ context.Context, configID string, version int, _ bool) (configmgmt.Version, error) {
	return configmgmt.Version{ConfigID: configID, Version: version, Body: &m.body}, nil
}

func (m *managed) ManagedCredentials(context.Context) (map[string]map[string]string, error) {
	return map[string]map[string]string{"memory-agent": nil}, nil
}

func (m *managed) SetConfigHold(_ context.Context, id string, _ uint64, expected, next string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held[id] != expected {
		return m.held[id], nil
	}
	m.held[id] = next
	return next, nil
}

func (m *managed) ConfigHolds(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for id, v := range m.held {
		if v != "" {
			out[id] = v
		}
	}
	return out, nil
}

func (m *managed) ComponentSet(_ context.Context, hash string) (configmgmt.Components, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.sets[hash]; ok {
		return c, nil
	}
	return nil, fleet.ErrNotFound
}

func (m *managed) SaveComponentSet(_ context.Context, hash string, c configmgmt.Components) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sets[hash]; !ok {
		m.sets[hash] = c
	}
	return nil
}

func managedFixture(t *testing.T) (Config, *managed) {
	t.Helper()
	m := &managed{body: "receivers: {filelog: {}}\n", held: map[string]string{}, sets: map[string]configmgmt.Components{}}
	digest := sha256.Sum256([]byte(m.body))
	target, err := configmgmt.NewTarget(configmgmt.Assignment{ID: "a1", Name: "base", ConfigID: "c1", Version: 1, Layer: 10, Selector: "service.name=linea-fleet-simulator"}, digest[:], "", nil)
	if err != nil {
		t.Fatal(err)
	}
	m.target = target
	token := strings.Repeat("s", 32)
	a, _ := auth.New(token)
	g, err := gateway.New(fleet.NewStore(8), a, 8, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	g.SetConfigManagement(configmgmt.NewReconciler(m, 1<<20), m)
	server := httptest.NewServer(g)
	t.Cleanup(func() { _ = g.Close(); server.Close() })
	return Config{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/opamp", Token: token, Agents: 4, ConnectRate: 20, ReconnectPercent: 25,
		Heartbeat: 100 * time.Millisecond, Duration: 1500 * time.Millisecond, Hold: time.Second, AcceptConfig: true, ApplyDelay: 50 * time.Millisecond, Builds: 1}, m
}

func TestAcceptedConfigIsAppliedOnceAcrossRestarts(t *testing.T) {
	c, m := managedFixture(t)
	result, err := Run(context.Background(), c, nil)
	// The planned reconnect keeps the applied status, so nothing is resent.
	if err != nil || !result.Success || result.ConfigsOffered != 4 || result.ConfigsApplied != 4 || result.ConfigsFailed != 0 {
		t.Fatalf("offered %d, applied %d, failed %d: %v %s", result.ConfigsOffered, result.ConfigsApplied, result.ConfigsFailed, err, result.Failure)
	}
	full := builds(1)[0]
	if set, err := m.ComponentSet(context.Background(), hex.EncodeToString(full.Hash)); err != nil || len(set["receivers"]) != 4 || m.count() != 1 {
		t.Fatalf("component set not stored once: %v", set)
	}
}

func TestFailedConfigIsHeld(t *testing.T) {
	c, m := managedFixture(t)
	c.FailPercent, c.Builds = 100, 0
	result, err := Run(context.Background(), c, nil)
	if err != nil || !result.Success || result.ConfigsFailed != 4 || result.ConfigsApplied != 0 || result.ConfigsOffered != 4 {
		t.Fatalf("offered %d, applied %d, failed %d: %v %s", result.ConfigsOffered, result.ConfigsApplied, result.ConfigsFailed, err, result.Failure)
	}
	if holds, _ := m.ConfigHolds(context.Background()); len(holds) != 4 {
		t.Fatalf("failed agents not held: %d", len(holds))
	}
}

func TestMinimalBuildIsNotOfferedTheConfig(t *testing.T) {
	c, m := managedFixture(t)
	c.MinimalPercent = 100
	result, err := Run(context.Background(), c, nil)
	if err != nil || !result.Success || result.ConfigsOffered != 0 {
		t.Fatalf("offered %d: %v %s", result.ConfigsOffered, err, result.Failure)
	}
	minimal := builds(1)[1]
	if set, err := m.ComponentSet(context.Background(), hex.EncodeToString(minimal.Hash)); err != nil || len(set["receivers"]) != 3 {
		t.Fatalf("minimal set not stored: %v", set)
	}
}

func TestReportingAgentIsNotOfferedConfig(t *testing.T) {
	c, _ := managedFixture(t)
	c.AcceptConfig, c.ApplyDelay = false, 0
	// opamp-go drops the offer; the run still passes because nothing reached
	// the simulated agent. The gateway does not offer config to agents
	// without the capability in the first place.
	result, err := Run(context.Background(), c, nil)
	if err != nil || !result.Success || result.ConfigsOffered != 0 {
		t.Fatalf("offered %d: %v %s", result.ConfigsOffered, err, result.Failure)
	}
	c.FailPercent = 5
	if c.Validate() == nil {
		t.Fatal("fail percent accepted without config acceptance")
	}
}

func (m *managed) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sets)
}
