package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

func TestHostIgnoresUnknownFieldsAndManifestFields(t *testing.T) {
	registry, _, _ := readyHost(t, fleet.NewStore(1), pluginsdk.AgentsRead)
	if w := hostPost(registry, pluginsdk.AgentsResolvePath, `{"attributes":{"a":"b"},"extra":1}`); w.Code != 200 {
		t.Fatalf("unknown request field refused: %d", w.Code)
	}
	if w := hostPost(registry, pluginsdk.AgentsResolvePath, `{"attributes":{"a":"b"}}{}`); w.Code != 400 {
		t.Fatalf("trailing data accepted: %d", w.Code)
	}
	config := fixtureConfig("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + config.ID + `","version":"` + config.Version + `","protocol_version":"` + pluginsdk.ProtocolVersion + `","instance_id":"` + strings.Repeat("c", 32) + `","added_later":{"x":1}}`))
	}))
	defer server.Close()
	config.Endpoint = server.URL
	probed := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer probed.Close()
	probed.probe(probed.entries[0])
	if got := probed.entries[0].snapshot().State; got != "ready" {
		t.Fatalf("manifest with an unknown field: %s", got)
	}
}

func TestHostAnswers421ForStaleInstanceOrEpoch(t *testing.T) {
	registry, _, _ := readyHost(t, fleet.NewStore(1), pluginsdk.AgentsRead)
	info := registry.entries[0].snapshot()
	call := func(path, instance, epoch string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 64))
		req.Header.Set(pluginsdk.InstanceHeader, instance)
		req.Header.Set(pluginsdk.EpochHeader, epoch)
		w := httptest.NewRecorder()
		registry.ServeHTTP(w, req)
		return w.Code
	}
	other := strings.Repeat("9", 32)
	for _, tc := range []struct {
		name, path, instance, epoch string
		want                        int
	}{
		{"session ok", pluginsdk.SessionPath, info.InstanceID, "", 200},
		{"session other instance", pluginsdk.SessionPath, other, "", 421},
		{"agents ok", pluginsdk.AgentsPath, info.InstanceID, info.Epoch, 200},
		{"agents other instance", pluginsdk.AgentsPath, other, info.Epoch, 421},
		{"agents other epoch", pluginsdk.AgentsPath, info.InstanceID, other, 421},
	} {
		if got := call(tc.path, tc.instance, tc.epoch); got != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestSessionRouteHasItsOwnHostSlot(t *testing.T) {
	registry, _, _ := readyHost(t, fleet.NewStore(1), pluginsdk.AgentsRead)
	e := registry.entries[0]
	for range cap(e.reads) {
		e.reads <- struct{}{}
	}
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath, strings.Repeat("a", 64)); w.Code != 503 {
		t.Fatalf("general slots full: got %d", w.Code)
	}
	if w := hostRequest(registry, "GET", pluginsdk.SessionPath, strings.Repeat("a", 64)); w.Code != 200 {
		t.Fatalf("session polled through full general slots: got %d", w.Code)
	}
	for range cap(e.reads) {
		<-e.reads
	}
	e.sessions <- struct{}{}
	if w := hostRequest(registry, "GET", pluginsdk.SessionPath, strings.Repeat("a", 64)); w.Code != 503 {
		t.Fatalf("session slot full: got %d", w.Code)
	}
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath, strings.Repeat("a", 64)); w.Code != 200 {
		t.Fatalf("general calls blocked by the session slot: got %d", w.Code)
	}
	<-e.sessions
}

func TestPlugin421IsNeverForwarded(t *testing.T) {
	config := fixtureConfig("")
	config.APIVersion = "1"
	var answer int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == pluginsdk.ManifestPath {
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32), APIVersion: "1"})
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(answer)
		_, _ = w.Write([]byte(`{"error":{"code":"plugin_detail","message":"plugin text"}}`))
	}))
	defer server.Close()
	config.Endpoint = server.URL
	r := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer r.Close()
	r.probe(r.entries[0])
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://fleet/api/v1/plugins/"+config.ID+"/api/v1/custom", nil)
		w := httptest.NewRecorder()
		r.ServeAPI(w, req, config.ID, "v1/custom", auth.Principal{ID: "reader", Role: "reader", PluginAccess: map[string]string{config.ID: "read"}})
		return w
	}
	answer = 421
	w := call()
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || strings.Contains(w.Body.String(), "plugin text") {
		t.Fatalf("421 forwarded or not retryable: %d %q retry-after %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
	}
	answer = 409
	if w := call(); w.Code != 409 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("a domain conflict must be forwarded: %d", w.Code)
	}
	answer = 421
	if _, err := r.Status(context.Background(), config.ID, auth.Principal{ID: "reader", Role: "reader", PluginAccess: map[string]string{config.ID: "read"}}); !errors.Is(err, fleet.ErrUnavailable) {
		t.Fatalf("status 421 must read as unavailable: %v", err)
	}
}
