package plugins

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/lineahq/linea/pluginsdk"
)

// inventoryFixture is a minimal out-of-process plugin: it answers the manifest
// and reads one host inventory page for status, using only the public SDK.
func inventoryFixture(config Config, hostEndpoint string) http.Handler {
	instance := pluginsdk.RandomID()
	client := pluginsdk.NewHTTPClient()
	host := pluginsdk.HostClient{Endpoint: hostEndpoint, Token: config.hostToken, InstanceID: instance, HTTP: client}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+config.serviceToken {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case pluginsdk.ManifestPath:
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: instance})
		case pluginsdk.StatusPath:
			session, err := host.Session(r.Context())
			var page pluginsdk.AgentPage
			if err == nil {
				err = pluginsdk.GetJSON(r.Context(), client, hostEndpoint+pluginsdk.AgentsPath+"?limit=100", config.hostToken, host.Headers(session), 128<<10, &page)
			}
			if err != nil {
				w.WriteHeader(503)
				return
			}
			connected := 0
			for _, agent := range page.Items {
				if agent.Connected {
					connected++
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"returned": len(page.Items), "connected": connected})
		default:
			w.WriteHeader(404)
		}
	})
}

func fixtureConfig(endpoint string) Config {
	return Config{ID: "inventory-example", Version: "0.1.0", Endpoint: endpoint, Enabled: true, Permissions: []string{pluginsdk.AgentsRead}, hostToken: strings.Repeat("a", 64), serviceToken: strings.Repeat("b", 64)}
}

func hostRequest(r *Registry, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if len(r.entries) > 0 {
		info := r.entries[0].snapshot()
		req.Header.Set(pluginsdk.InstanceHeader, info.InstanceID)
		req.Header.Set(pluginsdk.EpochHeader, info.Epoch)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestReadOnlyPluginRoundTripAndIsolation(t *testing.T) {
	store := fleet.NewStore(3)
	_, _ = store.Connect("00000000000000000000000000000001", fleet.Report{}, time.Now())
	config := fixtureConfig("")
	registry := New(context.Background(), nil, store)
	host := httptest.NewServer(registry)
	defer host.Close()
	server := httptest.NewServer(inventoryFixture(config, host.URL))
	defer server.Close()
	config.Endpoint = server.URL
	prepared := New(context.Background(), []Config{config}, store)
	registry.entries = prepared.entries
	defer registry.Close()
	defer prepared.Close()
	registry.probe(registry.entries[0])
	info, err := registry.Get(config.ID)
	if err != nil || info.State != "ready" {
		t.Fatal("manifest handshake failed")
	}
	body, err := registry.Status(context.Background(), config.ID, auth.Principal{ID: "reader-fixture", Role: "reader"})
	var status struct{ Returned, Connected int }
	if err != nil || json.Unmarshal(body, &status) != nil || status.Returned != 1 || status.Connected != 1 {
		t.Fatal("independent service did not read host inventory")
	}
	if _, err := registry.Status(context.Background(), config.ID, auth.Principal{Role: "agent"}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("agent role reached plugin status")
	}
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", pluginsdk.AgentsPath, "", 401},
		{"GET", pluginsdk.AgentsPath, config.serviceToken, 401},
		{"GET", "/api/v1/credentials", config.hostToken, 404},
		{"POST", pluginsdk.AgentsPath, config.hostToken, 405},
		{"GET", pluginsdk.AgentsPath + "?limit=101", config.hostToken, 400},
		{"GET", pluginsdk.AgentsPath + "?limit=1&limit=2", config.hostToken, 400},
		{"GET", pluginsdk.AgentsPath + "?after=invalid", config.hostToken, 400},
		{"GET", pluginsdk.AgentsPath + "?scope=admin", config.hostToken, 400},
		{"GET", pluginsdk.AgentsPath + "?limit=1", config.hostToken, 200},
	} {
		if got := hostRequest(registry, tc.method, tc.path, tc.token); got.Code != tc.want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, got.Code, tc.want)
		}
	}
	registry.entries[0].config.Permissions = []string{}
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath, config.hostToken); w.Code != 403 {
		t.Fatal("ungranted inventory permission was accepted")
	}
	registry.entries[0].config.Permissions = []string{pluginsdk.AgentsRead}
	registry.entries[0].reads <- struct{}{}
	registry.entries[0].reads <- struct{}{}
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath, config.hostToken); w.Code != 503 {
		t.Fatal("host read budget queued excess work")
	}
	<-registry.entries[0].reads
	<-registry.entries[0].reads
	registry.Close()
	if w := hostRequest(registry, "GET", pluginsdk.AgentsPath, config.hostToken); w.Code != 403 {
		t.Fatal("closed host accepted inventory work")
	}
}

func TestManifestFailuresAndBoundedStatus(t *testing.T) {
	var mode atomic.Int32
	var forwarded atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("b", 64) {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == pluginsdk.StatusPath {
			forwarded.Store(r.Header.Get(pluginsdk.CallerRoleHeader) == "reader" && r.Header.Get(pluginsdk.CallerIDHeader) == "caller-id" && r.Header.Get("Cookie") == "")
			switch mode.Load() {
			case 10:
				_, _ = w.Write([]byte(`{"payload":"` + strings.Repeat("x", 33<<10) + `"}`))
			case 11:
				_, _ = w.Write([]byte(`[]`))
			case 12:
				<-r.Context().Done()
			default:
				_, _ = w.Write([]byte(`{"ok":true}`))
			}
			return
		}
		m := pluginsdk.Manifest{ID: "inventory-example", Version: "0.1.0", ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32)}
		switch mode.Load() {
		case 1:
			m.ID = "foreign"
		case 2:
			m.Version = "unsupported"
		case 3:
			m.ProtocolVersion = "2"
		case 4:
			http.Redirect(w, r, "/untrusted", http.StatusFound)
			return
		case 5:
			_, _ = w.Write([]byte(strings.Repeat("x", 4097)))
			return
		case 6:
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(m)
	}))
	defer server.Close()
	registry := New(context.Background(), []Config{fixtureConfig(server.URL)}, fleet.NewStore(1))
	defer registry.Close()
	e := registry.entries[0]
	for _, tc := range []struct {
		mode int32
		want string
	}{{0, "ready"}, {1, "incompatible"}, {2, "incompatible"}, {3, "incompatible"}, {4, "unavailable"}, {5, "unavailable"}, {6, "unavailable"}, {0, "ready"}} {
		mode.Store(tc.mode)
		registry.probe(e)
		if e.snapshot().State != tc.want {
			t.Fatalf("mode %d: want %s got %s", tc.mode, tc.want, e.snapshot().State)
		}
	}
	caller := auth.Principal{ID: "caller-id", Role: "reader"}
	if _, err := registry.Status(context.Background(), e.config.ID, caller); err != nil || !forwarded.Load() {
		t.Fatal("caller context was not safely propagated")
	}
	for _, bad := range []int32{10, 11, 12} {
		mode.Store(bad)
		if _, err := registry.Status(context.Background(), e.config.ID, caller); !errors.Is(err, fleet.ErrUnavailable) {
			t.Fatal("invalid or stalled plugin response was accepted")
		}
	}
	e.calls <- struct{}{}
	e.calls <- struct{}{}
	if _, err := registry.Status(context.Background(), e.config.ID, caller); !errors.Is(err, fleet.ErrUnavailable) {
		t.Fatal("full outbound budget did not refuse work")
	}
	<-e.calls
	<-e.calls
	server.Close()
	registry.probe(e)
	if e.snapshot().State != "unavailable" {
		t.Fatal("stopped service remained ready")
	}
}

func TestRegistrationConfigurationRejectsUnsafeInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "service"), []byte(strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	base := Config{ID: "sample", Version: "0.1.0", Endpoint: "http://127.0.0.1:8090", Enabled: true, Permissions: []string{pluginsdk.AgentsRead}, HostTokenFile: "host", ServiceTokenFile: "service"}
	for _, tc := range []struct {
		name string
		edit func(*Config)
		ok   bool
	}{
		{"valid", func(*Config) {}, true},
		{"untrusted-http", func(c *Config) { c.Endpoint = "http://service:8090" }, false},
		{"acknowledged-private-http", func(c *Config) { c.Endpoint = "http://service:8090"; c.AllowInsecureHTTP = true }, true},
		{"userinfo", func(c *Config) { c.Endpoint = "https://user:secret@service:8090" }, false},
		{"query", func(c *Config) { c.Endpoint += "?secret=value" }, false},
		{"path", func(c *Config) { c.Endpoint += "/other" }, false},
		{"bad-id", func(c *Config) { c.ID = "../api" }, false},
		{"unknown-permission", func(c *Config) { c.Permissions = []string{"credentials.read"} }, false},
		{"both-permissions", func(c *Config) { c.Permissions = []string{pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect} }, true},
		{"duplicate-permission", func(c *Config) { c.Permissions = []string{pluginsdk.AgentsRead, pluginsdk.AgentsRead} }, false},
		{"no-permissions", func(c *Config) { c.Permissions = []string{} }, true},
		{"same-direction-token", func(c *Config) { c.ServiceTokenFile = "host" }, false},
		{"disabled", func(c *Config) { c.Enabled = false }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			tc.edit(&c)
			body, _ := json.Marshal(map[string]any{"plugins": []Config{c}})
			path := filepath.Join(dir, "plugins.json")
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			entries, err := Load(path)
			if (err == nil) != tc.ok {
				t.Fatal("unexpected configuration acceptance")
			}
			if err == nil && !c.Enabled {
				r := New(context.Background(), entries, fleet.NewStore(1))
				defer r.Close()
				r.Start()
				if info := r.List(); info[0].State != "disabled" {
					t.Fatal("disabled service was advertised")
				}
			}
		})
	}
	for _, body := range []string{`{"plugins":[],"unknown":true}`, `{"plugins":[]} {}`, strings.Repeat("x", 65537)} {
		path := filepath.Join(dir, "invalid.json")
		_ = os.WriteFile(path, []byte(body), 0600)
		if _, err := Load(path); err == nil {
			t.Fatal("invalid configuration was accepted")
		}
	}
	body, _ := json.Marshal(map[string]any{"plugins": []Config{base}})
	path := filepath.Join(dir, "reserved.json")
	_ = os.WriteFile(path, body, 0600)
	if _, err := Load(path, strings.Repeat("a", 64)); err == nil {
		t.Fatal("plugin credential reused a management/Collector secret")
	}
	body, _ = json.Marshal(map[string]any{"plugins": []Config{base, base}})
	_ = os.WriteFile(path, body, 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("duplicate registration identity was accepted")
	}
	other := base
	other.ID = "another"
	body, _ = json.Marshal(map[string]any{"plugins": []Config{base, other}})
	_ = os.WriteFile(path, body, 0600)
	if _, err := Load(path); err == nil {
		t.Fatal("registrations shared host/service credentials")
	}
}
