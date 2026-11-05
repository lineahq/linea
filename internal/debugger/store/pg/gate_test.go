package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// gateToken is the workers' registration token in the gate test.
const gateToken = "registration-token-for-the-instance-gate"

// instance is one team server instance on the shared store, behind a listener
// that kill closes with every connection it accepted, as a killed process
// loses its sockets.
type instance struct {
	service *platform.Service
	store   *Store
	srv     *httptest.Server
	mu      sync.Mutex
	conns   []net.Conn
	dead    bool
}

func (r *instance) track(c net.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns = append(r.conns, c)
}

func (r *instance) alive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.dead
}

// kill ends the instance without closing its service: its store handle and
// sockets close, so it writes nothing more and its workers lose it.
func (r *instance) kill() {
	r.mu.Lock()
	r.dead = true
	conns := r.conns
	r.mu.Unlock()
	r.srv.Listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	r.store.Close()
}

type trackingListener struct {
	net.Listener
	r *instance
}

func (l trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.r.track(c)
	}
	return c, err
}

func openInstance(t *testing.T, f fixture, lease time.Duration) *instance {
	t.Helper()
	store, err := Open(context.Background(), f.config(artifact.Limits{}))
	if err != nil {
		t.Fatal(err)
	}
	s, err := platform.Open(store, platform.Options{Shared: true, Lease: lease, WorkerToken: gateToken,
		Paths: platform.PathPolicy{Unrestricted: true}})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	r := &instance{service: s, store: store}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.ConnectWorker))
	r.srv.Listener = trackingListener{Listener: r.srv.Listener, r: r}
	r.srv.Start()
	t.Cleanup(func() {
		r.srv.Close()
		s.Close()
		store.Close()
	})
	return r
}

// balancer proxies each worker connection to a live instance: the first
// connections to a, the rest to b, and every one to b once a is dead.
func balancer(t *testing.T, a, b *instance, toA int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	lb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		target := b
		if n < toA && a.alive() {
			target = a
		}
		n++
		mu.Unlock()
		u, _ := url.Parse(target.srv.URL)
		httputil.NewSingleHostReverseProxy(u).ServeHTTP(w, r)
	}))
	t.Cleanup(lb.Close)
	return lb
}

// TestTwoInstancesAndThreeWorkersEndEveryRunWhenOneIsKilled is the shared
// instances gate: two instances on one PostgreSQL schema serve three connected
// workers; an instance killed mid-run loses no run, and every run ends.
func TestTwoInstancesAndThreeWorkersEndEveryRunWhenOneIsKilled(t *testing.T) {
	f := newFixture(t)
	const lease = 2 * time.Second
	a, b := openInstance(t, f, lease), openInstance(t, f, lease)
	// Two workers dial a and one dials b, each with two slots.
	lb := balancer(t, a, b, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	workers, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := worker.Connect(workers, worker.ConnectOptions{URL: lb.URL, Token: gateToken, Name: "team", Capacity: 2})
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
			}
		}()
	}
	defer func() { stop(); wg.Wait() }()

	hello, err := worker.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	caller := platform.Local("gate")
	if _, err := a.service.RegisterRuntime(ctx, caller, platform.RegisterRuntimeRequest{
		Name: "team", Image: "registry.example/linea-runtime:team", Hello: raw}); err != nil {
		t.Fatal(err)
	}
	// Either instance counts the workers of both.
	for _, r := range []*instance{a, b} {
		for {
			info, err := r.service.GetRuntime(ctx, caller, "team")
			if err != nil {
				t.Fatal(err)
			}
			if c := info.Catalog; c.Workers == 3 && c.Slots == 6 && !c.Confirmed.IsZero() {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("an instance counts %d workers with %d slots, want 3 with 6", info.Catalog.Workers, info.Catalog.Slots)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	// Each run waits between its inputs, so the workers' six slots are all
	// busy when a dies.
	sample := filepath.Join("..", "..", "..", "..", "samples", "logs-cleanup")
	input, err := filepath.Abs(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario := fmt.Sprintf("version: 1\nname: slow\ninputs:\n  - receiver: otlp\n    file: %q\n  - after: 3s\n    receiver: otlp\n    file: %q\n",
		filepath.ToSlash(input), filepath.ToSlash(input))
	const total = 9
	ids := make([]string, total)
	for i := range total {
		on := []*instance{a, b}[i%2]
		run, err := on.service.SubmitRun(ctx, caller, platform.RunRequest{Mode: platform.ModeScenario, Runtime: "team",
			Config: platform.Source{Path: filepath.Join(sample, "config.yaml")}, Scenario: &platform.ScenarioInput{Scenario: platform.Source{Text: scenario}}})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = run.ID
	}

	views := func() []platform.Run {
		vs := make([]platform.Run, total)
		for i, id := range ids {
			v, err := b.service.GetRun(ctx, caller, id)
			if err != nil {
				t.Fatal(err)
			}
			vs[i] = v
		}
		return vs
	}
	poll := func(what string, cond func([]platform.Run) bool) []platform.Run {
		t.Helper()
		for {
			vs := views()
			if cond(vs) {
				return vs
			}
			select {
			case <-ctx.Done():
				for _, v := range vs {
					t.Logf("%s: %s attempt %d %s", v.ID, v.State, v.Attempt, v.Error)
				}
				t.Fatalf("timed out waiting for %s", what)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	poll("six running runs", func(vs []platform.Run) bool {
		running := 0
		for _, v := range vs {
			if v.State == platform.StateRunning {
				running++
			}
		}
		return running == 6
	})
	a.kill()

	ended := poll("every run to end", func(vs []platform.Run) bool {
		for _, v := range vs {
			if v.State != platform.StateDone && v.State != platform.StateError && v.State != platform.StateCancelled && v.State != platform.StateLost {
				return false
			}
		}
		return true
	})
	retried := 0
	for i, v := range ended {
		t.Logf("run %d %s: %s attempt %d", i, v.ID, v.State, v.Attempt)
		if v.State != platform.StateDone {
			t.Errorf("run %s ended %s: %s", v.ID, v.State, v.Error)
		}
		if v.Attempt > 1 {
			retried++
		}
	}
	// a's two workers held four runs, which b requeued.
	if retried < 4 {
		t.Errorf("%d runs ran again, want at least the four a's workers held", retried)
	}
}
