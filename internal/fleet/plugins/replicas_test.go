package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/lineahq/linea/pluginsdk"
)

// manifestServer answers the manifest probe; refuse makes it answer 503.
func manifestServer(t *testing.T, config Config, instance string, refuse *atomic.Bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse != nil && refuse.Load() {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: instance})
	}))
	t.Cleanup(server.Close)
	return server
}

// replicas are two registries of one plugin on one memory store and clock.
func replicas(t *testing.T, endpointA, endpointB string) (a, b *Registry, clock *fakeClock) {
	t.Helper()
	store := fleet.NewStore(1)
	a = New(context.Background(), []Config{fixtureConfig(endpointA)}, store)
	b = New(context.Background(), []Config{fixtureConfig(endpointB)}, store)
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	return a, b, useClock(store, a, b)
}

func syncNow(t *testing.T, r *Registry) {
	t.Helper()
	if err := r.sync(context.Background(), translog.ReasonSharedState); err != nil {
		t.Fatal(err)
	}
}

func sessionOf(t *testing.T, r *Registry, instance string) (int, pluginsdk.Session) {
	t.Helper()
	req := httptest.NewRequest("GET", pluginsdk.SessionPath, nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	req.Header.Set(pluginsdk.InstanceHeader, instance)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var s pluginsdk.Session
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, s
}

func leaseOf(e *entry) context.Context {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lease
}

// One replica's probe makes the plugin ready on every replica, and a replica
// that cannot reach the plugin rotates nothing while another sees it. Only a
// plugin no replica saw for the whole window is fenced (T4).
func TestReplicasShareOneEpoch(t *testing.T) {
	instance := strings.Repeat("c", 32)
	server := manifestServer(t, fixtureConfig(""), instance, nil)
	a, b, clock := replicas(t, server.URL, "http://127.0.0.1:1")
	ea, eb := a.entries[0], b.entries[0]
	a.probe(ea)
	syncNow(t, b)
	if !a.ready(ea) || !b.ready(eb) || ea.snapshot().Epoch != eb.snapshot().Epoch {
		t.Fatalf("one good probe: a %+v b %+v", a.view(ea), b.view(eb))
	}
	epoch := ea.snapshot().Epoch
	lease := leaseOf(eb)
	clock.advance(time.Second)
	b.probe(eb)
	a.probe(ea)
	syncNow(t, b)
	if ea.snapshot().Epoch != epoch || eb.snapshot().Epoch != epoch || lease.Err() != nil {
		t.Fatal("one replica's unreachable plugin rotated the epoch")
	}
	if !b.ready(eb) {
		t.Fatal("b is not ready on the evidence of a")
	}
	codeA, sa := sessionOf(t, a, instance)
	codeB, sb := sessionOf(t, b, instance)
	if codeA != 200 || codeB != 200 || sa.Epoch != sb.Epoch || sa.Epoch != epoch || sa.Revision != sb.Revision {
		t.Fatalf("sessions differ: %d %+v / %d %+v", codeA, sa, codeB, sb)
	}
	clock.advance(fleet.PluginSeenWindow)
	b.probe(eb)
	if eb.snapshot().Epoch == epoch || lease.Err() == nil || b.ready(eb) {
		t.Fatal("a plugin unseen for the whole window kept its epoch")
	}
}

// A refusal rotates the epoch once: the other replica adopts it, cancels its
// admitted calls and does not rotate again for the same refusal.
func TestRefusalRotatesOnce(t *testing.T) {
	var refuse atomic.Bool
	server := manifestServer(t, fixtureConfig(""), strings.Repeat("c", 32), &refuse)
	a, b, _ := replicas(t, server.URL, server.URL)
	ea, eb := a.entries[0], b.entries[0]
	a.probe(ea)
	syncNow(t, b)
	before := eb.snapshot().Epoch
	lease := leaseOf(eb)
	refuse.Store(true)
	a.probe(ea)
	rotated := ea.snapshot().Epoch
	if rotated == before || a.ready(ea) {
		t.Fatal("a refusal kept the epoch")
	}
	syncNow(t, b)
	if eb.snapshot().Epoch != rotated || lease.Err() == nil || b.ready(eb) {
		t.Fatal("b did not adopt the rotation")
	}
	b.probe(eb)
	a.probe(ea)
	if ea.snapshot().Epoch != rotated || eb.snapshot().Epoch != rotated {
		t.Fatal("the same refusal rotated the epoch again")
	}
	refuse.Store(false)
	b.probe(eb)
	syncNow(t, a)
	if !a.ready(ea) || !b.ready(eb) || ea.snapshot().Epoch != rotated {
		t.Fatal("the plugin did not recover under the rotated epoch")
	}
}

// A replica whose registration does not match the manifest stops forwarding
// and writes nothing (T3a); the others keep serving.
func TestMismatchedRegistrationWritesNothing(t *testing.T) {
	instance := strings.Repeat("c", 32)
	server := manifestServer(t, fixtureConfig(""), instance, nil)
	a, b, _ := replicas(t, server.URL, server.URL)
	ea, eb := a.entries[0], b.entries[0]
	eb.config.Version = "0.2.0"
	a.probe(ea)
	epoch := ea.snapshot().Epoch
	b.probe(eb)
	if got := b.view(eb).State; got != "incompatible" || b.ready(eb) {
		t.Fatalf("b with another version: %s", got)
	}
	a.probe(ea)
	if ea.snapshot().Epoch != epoch || !a.ready(ea) {
		t.Fatal("a mismatched replica disturbed the others")
	}
	if code, _ := sessionOf(t, b, instance); code != 403 {
		t.Fatalf("session from the mismatched replica: %d", code)
	}
}

// A lifecycle change on one replica is the session answer of every replica
// at once, and the other replicas' caches follow on their next read.
func TestLifecycleOnOneReplicaIsSeenByAll(t *testing.T) {
	instance := strings.Repeat("c", 32)
	server := manifestServer(t, fixtureConfig(""), instance, nil)
	a, b, _ := replicas(t, server.URL, server.URL)
	ea, eb := a.entries[0], b.entries[0]
	admin := auth.Principal{ID: "admin-1", Role: "admin"}
	a.probe(ea)
	syncNow(t, b)
	lease := leaseOf(eb)
	if _, err := a.Lifecycle(context.Background(), ea.config.ID, "drain", admin, 0); err != nil {
		t.Fatal(err)
	}
	if code, _ := sessionOf(t, b, instance); code != 403 {
		t.Fatalf("session from b after a drain on a: %d", code)
	}
	syncNow(t, b)
	if got := b.view(eb); got.State != "draining" || got.Revision != 1 || lease.Err() == nil {
		t.Fatalf("b after the drain: %+v", got)
	}
	if _, err := b.Lifecycle(context.Background(), eb.config.ID, "enable", admin, 0); !errors.Is(err, fleet.ErrPrecondition) {
		t.Fatalf("stale revision on b: %v", err)
	}
	if _, err := b.Lifecycle(context.Background(), eb.config.ID, "enable", admin, 1); err != nil {
		t.Fatal(err)
	}
	b.probe(eb)
	syncNow(t, a)
	if !a.ready(ea) || !b.ready(eb) || ea.snapshot().Epoch != eb.snapshot().Epoch {
		t.Fatal("the re-enabled plugin is not ready on both replicas")
	}
}

// A host request with an epoch newer than this replica's cache reads the row
// once and is served.
func TestHostRequestRefreshesAStaleCache(t *testing.T) {
	server := manifestServer(t, fixtureConfig(""), strings.Repeat("c", 32), nil)
	a, b, _ := replicas(t, server.URL, server.URL)
	ea, eb := a.entries[0], b.entries[0]
	a.probe(ea)
	syncNow(t, b)
	other := strings.Repeat("d", 32)
	ea.config.Endpoint = manifestServer(t, fixtureConfig(""), other, nil).URL
	a.probe(ea) // the plugin restarted: T2
	if ea.snapshot().Epoch == eb.snapshot().Epoch {
		t.Fatal("a new instance kept the epoch")
	}
	req := httptest.NewRequest("GET", pluginsdk.AgentsPath, nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
	req.Header.Set(pluginsdk.InstanceHeader, other)
	req.Header.Set(pluginsdk.EpochHeader, ea.snapshot().Epoch)
	w := httptest.NewRecorder()
	b.ServeHTTP(w, req)
	if w.Code != 200 || eb.snapshot().Epoch != ea.snapshot().Epoch {
		t.Fatalf("stale cache: %d", w.Code)
	}
}
