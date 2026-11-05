package agentsimulator

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/gateway"
)

func fixture(t *testing.T) (Config, *fleet.Store) {
	t.Helper()
	token := strings.Repeat("s", 32)
	a, _ := auth.New(token)
	store := fleet.NewStore(4)
	g, err := gateway.New(store, a, 4, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	t.Cleanup(func() { _ = g.Close(); server.Close() })
	return Config{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/opamp", Token: token, Agents: 2, ConnectRate: 10, PayloadBytes: 128, ReconnectPercent: 10, HealthChangeEvery: 3, Heartbeat: 100 * time.Millisecond, Duration: 500 * time.Millisecond, Hold: time.Second}, store
}
func TestRealGatewaySimulationReportsAndReconnects(t *testing.T) {
	c, store := fixture(t)
	c.CheckpointDirectory = t.TempDir()
	var phases []Observation
	result, err := Run(context.Background(), c, func(o Observation) {
		phases = append(phases, o)
		if o.Phase == "ready" || o.Phase == "steady_complete" || o.Phase == "reconnected" {
			if err := os.WriteFile(filepath.Join(c.CheckpointDirectory, o.Phase), nil, 0600); err != nil {
				t.Error("cannot release fixture phase")
			}
		}
	})
	if err != nil || !result.Success || result.Active != 0 || result.Ready != 2 || result.PeakActive != 2 || result.Reconnects != 1 || result.UnexpectedErrors != 0 || result.SteadyMinAcknowledged < 1 || result.HealthChanges == 0 || result.RoundTrip.All.Count == 0 || result.RoundTrip.All.Count > result.HealthChanges || result.BytesSent == 0 {
		t.Fatal("simulation failed its protocol/progress gates", err, result.Failure)
	}
	if len(phases) != 6 || phases[1].Phase != "ready" || phases[1].Active != 2 || phases[3].Phase != "reconnected" {
		t.Fatal("missing stable verification checkpoints")
	}
	// Socket closure precedes the gateway's asynchronous disconnect publication.
	deadline := time.Now().Add(2 * time.Second)
	page, err := store.ListAgents(context.Background(), fleet.AgentQuery{Limit: 4})
	for time.Now().Before(deadline) {
		connected := false
		for _, agent := range page.Items {
			connected = connected || agent.Connected
		}
		if err != nil || !connected {
			break
		}
		time.Sleep(10 * time.Millisecond)
		page, err = store.ListAgents(context.Background(), fleet.AgentQuery{Limit: 4})
	}
	if err != nil || len(page.Items) != 2 {
		t.Fatal("real inventory lost or invented identities")
	}
	advanced := 0
	for _, agent := range page.Items {
		if agent.Generation == 2 {
			advanced++
		}
		if agent.Connected {
			t.Fatal("shutdown leaked a connection")
		}
	}
	if advanced != 1 {
		t.Fatal("reconnect changed identities instead of advancing generation")
	}
	data, _ := json.Marshal(page)
	if strings.Contains(string(data), "synthetic_padding") {
		t.Fatal("synthetic metadata entered core inventory")
	}
	data, _ = json.Marshal(phases)
	if strings.Contains(string(data), c.Token) || strings.Contains(string(data), c.Endpoint) {
		t.Fatal("simulation output leaked endpoint or bearer")
	}
}
func TestWrongBearerFailsWithoutSilentRecovery(t *testing.T) {
	c, _ := fixture(t)
	c.Token = strings.Repeat("x", 32)
	result, err := Run(context.Background(), c, nil)
	if err == nil || result.Success || result.UnexpectedErrors == 0 || result.DialFailures == 0 || result.Active != 0 {
		t.Fatal("denied authentication was hidden or sockets leaked")
	}
}
func TestCancellationClosesEverySession(t *testing.T) {
	c, _ := fixture(t)
	c.CheckpointDirectory = t.TempDir() // No release: cancellation must unblock it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := Run(ctx, c, func(o Observation) {
		if o.Phase == "ready" {
			cancel()
		}
	})
	if err == nil || result.Success || result.Active != 0 || result.Ready != 2 || result.UnexpectedErrors != 0 {
		t.Fatal("cancellation failed to clean up all client sessions")
	}
}
func TestMessageBudgetRefusesUnboundedCadence(t *testing.T) {
	c, _ := fixture(t)
	c.Agents = 20000
	c.Heartbeat = time.Second
	if c.Validate() == nil {
		t.Fatal("heartbeat stream exceeds the simulation bound")
	}
	c.Agents = 2
	c.Endpoint = "ws://remote.invalid/v1/opamp"
	if c.Validate() == nil {
		t.Fatal("unacknowledged remote plaintext accepted")
	}
}

func TestCancellationAtCompletionCannotPass(t *testing.T) {
	c, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := Run(ctx, c, func(o Observation) {
		if o.Phase == "finished" {
			cancel()
		}
	})
	if err == nil || result.Success || result.Active != 0 {
		t.Fatal("late cancellation produced a false success")
	}
}

// restartable serves a real gateway on a fixed loopback port so a test can
// crash and restart it under the same endpoint.
type restartable struct {
	t      *testing.T
	addr   string
	token  string
	server *http.Server
	g      *gateway.Gateway
	store  *fleet.Store
}

func (s *restartable) start() {
	a, _ := auth.New(s.token)
	s.store = fleet.NewStore(16)
	g, err := gateway.New(s.store, a, 16, 128<<10)
	if err != nil {
		s.t.Fatal(err)
	}
	var l net.Listener
	for attempt := 0; attempt < 50; attempt++ {
		if l, err = net.Listen("tcp", s.addr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		s.t.Fatal(err)
	}
	s.addr, s.g, s.server = l.Addr().String(), g, &http.Server{Handler: g}
	go func() { _ = s.server.Serve(l) }()
}
func (s *restartable) crash() { _ = s.g.Close(); _ = s.server.Close() }

func TestStormRecoversEverySessionAfterServerCrash(t *testing.T) {
	s := &restartable{t: t, addr: "127.0.0.1:0", token: strings.Repeat("s", 32)}
	s.start()
	t.Cleanup(s.crash)
	c := Config{Endpoint: "ws://" + s.addr + "/v1/opamp", Token: s.token, Agents: 6, ConnectRate: 50, ReconnectPercent: 0, Heartbeat: 200 * time.Millisecond, Duration: 500 * time.Millisecond, Hold: time.Second, Storms: 1, StormTimeout: 20 * time.Second}
	var stormed Observation
	result, err := Run(context.Background(), c, func(o Observation) {
		switch o.Phase {
		case "storm_armed":
			s.crash()
			time.Sleep(300 * time.Millisecond)
			s.start()
		case "recovered":
			stormed = o
		}
	})
	if err != nil || !result.Success || result.StormLost != 6 || result.StormRecovered != 6 || result.UnexpectedErrors != 0 || stormed.StormSeconds <= 0 || stormed.StormFromFirstReconnectSeconds > stormed.StormSeconds || result.ReconnectRetries == 0 {
		t.Fatal("storm did not recover every session with backoff", err, result.Failure, result.Counters)
	}
	page, err := s.store.ListAgents(context.Background(), fleet.AgentQuery{Limit: 16})
	if err != nil || len(page.Items) != 6 {
		t.Fatal("restarted server did not see every identity once")
	}
}

func TestStormWaitsOnlyForLostSessions(t *testing.T) {
	s := &restartable{t: t, addr: "127.0.0.1:0", token: strings.Repeat("s", 32)}
	s.start()
	t.Cleanup(s.crash)
	c := Config{Endpoint: "ws://" + s.addr + "/v1/opamp", Token: s.token, Agents: 6, ConnectRate: 50, ReconnectPercent: 0, Heartbeat: 200 * time.Millisecond, Duration: 500 * time.Millisecond, Hold: time.Second, Storms: 2, StormTimeout: 20 * time.Second}
	var lost []int64
	result, err := Run(context.Background(), c, func(o Observation) {
		switch o.Phase {
		case "recovered":
			lost = append(lost, o.StormLostNow)
		case "storm_armed":
			page, err := s.store.ListAgents(context.Background(), fleet.AgentQuery{Limit: 16})
			if err != nil || len(page.Items) != 6 {
				t.Error("agents missing before the partial outage")
				return
			}
			// The second outage overlaps a session lost in the first.
			for _, a := range page.Items[o.Storm-1 : 3*o.Storm-1] {
				s.g.CloseAgent(a.ID, ^uint64(0))
			}
		}
	})
	if err != nil || !result.Success || len(lost) != 2 || lost[0] != 2 || lost[1] != 4 || result.StormLost != 6 || result.StormRecovered != 6 || result.UnexpectedErrors != 0 {
		t.Fatal("partial outages were not recovered per storm", err, result.Failure, lost, result.Counters)
	}
}

func TestDriverEndsStormWithoutLoss(t *testing.T) {
	c, _ := fixture(t)
	c.ReconnectPercent = 0
	c.Storms, c.StormTimeout = 1, 10*time.Second
	c.CheckpointDirectory = t.TempDir()
	release := func(name string) {
		if err := os.WriteFile(filepath.Join(c.CheckpointDirectory, name), nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	var recovered Observation
	result, err := Run(context.Background(), c, func(o Observation) {
		switch o.Phase {
		case "storm_armed":
			release("storm_end")
		case "recovered":
			recovered = o
			release("recovered")
		case "ready", "steady_complete", "reconnected":
			release(o.Phase)
		}
	})
	if err != nil || !result.Success || recovered.Storm != 1 || recovered.StormLostNow != 0 || recovered.StormSeconds != 0 {
		t.Fatal("driver could not end a storm without loss", err, result.Failure)
	}
}

func TestChurnReplacesIdentities(t *testing.T) {
	c, store := fixture(t)
	c.ReconnectPercent = 0
	c.ChurnRate = 10
	c.Duration = time.Second
	result, err := Run(context.Background(), c, nil)
	if err != nil || !result.Success || result.Churned == 0 || result.UnexpectedErrors != 0 {
		t.Fatal("churn failed", err, result.Failure)
	}
	page, err := store.ListAgents(context.Background(), fleet.AgentQuery{Limit: 4})
	if err != nil || len(page.Items) <= 2 {
		t.Fatal("churned identities were not new UIDs")
	}
}

// Admission refusals are retried by the opamp-go client with its own
// backoff and the server's Retry-After, and counted, not failed.
func TestRefusalsRetryWithClientBackoff(t *testing.T) {
	token := strings.Repeat("s", 32)
	a, _ := auth.New(token)
	g, err := gateway.New(fleet.NewStore(4), a, 4, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if refused.Add(1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		g.ServeHTTP(w, req)
	}))
	t.Cleanup(func() { _ = g.Close(); server.Close() })
	c := Config{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/opamp", Token: token, Agents: 1, ConnectRate: 10, Heartbeat: 100 * time.Millisecond, Duration: 300 * time.Millisecond, Hold: time.Second}
	started := time.Now()
	result, err := Run(context.Background(), c, nil)
	if err != nil || !result.Success || result.Refusals != 2 || result.DialFailures != 2 || result.DialAttempts != 3 {
		t.Fatal("refusals were not retried once each", err, result.Failure, result.Counters)
	}
	if time.Since(started) < 2*time.Second {
		t.Fatal("client ignored Retry-After")
	}
}
