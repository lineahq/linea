package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/pluginsdk"
)

func newPlugin(t *testing.T) *plugin {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := &plugin{instance: pluginsdk.RandomID(), serviceToken: strings.Repeat("b", 64), newSession: make(chan struct{}, 1)}
	p.epochCtx, p.endEpoch = context.WithCancel(ctx)
	return p
}

func TestStaleInstanceOrEpochIs421NotConflict(t *testing.T) {
	p := newPlugin(t)
	epoch := strings.Repeat("e", 32)
	p.session, p.current = pluginsdk.Session{Epoch: epoch}, true
	call := func(instance, epoch string) int {
		req := httptest.NewRequest("GET", pluginsdk.APIPath+"v1/summary", nil)
		req.Header.Set("Authorization", "Bearer "+p.serviceToken)
		req.Header.Set(pluginsdk.CallerIDHeader, "caller")
		req.Header.Set(pluginsdk.CallerAccessHeader, "read")
		req.Header.Set(pluginsdk.InstanceHeader, instance)
		req.Header.Set(pluginsdk.EpochHeader, epoch)
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		return w.Code
	}
	if got := call(p.instance, strings.Repeat("0", 32)); got != 421 {
		t.Fatalf("other epoch: %d", got)
	}
	if got := call(strings.Repeat("0", 32), epoch); got != 421 {
		t.Fatalf("other instance: %d", got)
	}
	if got := call(p.instance, epoch); got != 503 { // authorized; no summary yet
		t.Fatalf("current authority: %d", got)
	}
}

// A silent host needs two polls in a row to end the session; a refusal ends it
// at once.
func TestSessionPollEndsOnRefusalAtOnceAndSilenceTwice(t *testing.T) {
	var refuse atomic.Bool
	p := newPlugin(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(421)
			return
		}
		_ = json.NewEncoder(w).Encode(pluginsdk.Session{PluginID: pluginID, InstanceID: p.instance, Epoch: strings.Repeat("1", 32), Permissions: []string{pluginsdk.AgentsRead}})
	}))
	t.Cleanup(server.Close)
	host := pluginsdk.HostClient{Endpoint: server.URL, Token: strings.Repeat("a", 64), InstanceID: p.instance, HTTP: pluginsdk.NewHTTPClient(), SessionHTTP: pluginsdk.NewHTTPClient()}
	p.host = host
	current := func() bool { _, _, ok := p.admitted(); return ok }
	ctx := context.Background()
	p.pollSession(ctx)
	if !current() {
		t.Fatal("no session")
	}
	// Silence: nothing listens on the closed port.
	p.host.Endpoint = "http://127.0.0.1:1"
	p.pollSession(ctx)
	if !current() {
		t.Fatal("one silent poll ended the session")
	}
	p.pollSession(ctx)
	if current() {
		t.Fatal("two silent polls kept the session")
	}
	// An answer in between resets the count.
	p.host.Endpoint = server.URL
	p.pollSession(ctx)
	p.host.Endpoint = "http://127.0.0.1:1"
	p.pollSession(ctx)
	p.host.Endpoint = server.URL
	p.pollSession(ctx)
	p.host.Endpoint = "http://127.0.0.1:1"
	p.pollSession(ctx)
	if !current() {
		t.Fatal("silent polls separated by an answer ended the session")
	}
	// A refusal ends it at once.
	p.host.Endpoint = server.URL
	refuse.Store(true)
	p.pollSession(ctx)
	if current() {
		t.Fatal("421 kept the session")
	}
}
