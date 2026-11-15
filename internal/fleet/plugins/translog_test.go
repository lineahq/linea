package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/lineahq/linea/pluginsdk"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// take returns the lines written since the last call as event name and fields.
func (l *logBuffer) take(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.b.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not a JSON line: %q", line)
		}
		out = append(out, m)
	}
	l.b.Reset()
	return out
}

type lifecycleStore struct{ revision int64 }

func (lifecycleStore) PluginLifecycles(context.Context) ([]fleet.PluginLifecycle, error) {
	return nil, nil
}

func (s *lifecycleStore) SetPluginLifecycle(_ context.Context, _ string, item fleet.PluginLifecycle, _ int64) (fleet.PluginLifecycle, error) {
	s.revision++
	item.Revision = s.revision
	return item, nil
}

func TestPluginTransitionsAreLoggedOnceWithReasons(t *testing.T) {
	var instance atomic.Value
	instance.Store(strings.Repeat("c", 32))
	var down atomic.Bool
	config := fixtureConfig("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: instance.Load().(string)})
	}))
	defer server.Close()
	config.Endpoint = server.URL
	var out logBuffer
	r := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer r.Close()
	r.SetLogger(translog.New(&out))
	if err := r.RestoreLifecycle(context.Background(), &lifecycleStore{}); err != nil {
		t.Fatal(err)
	}
	e := r.entries[0]

	r.probe(e)
	r.probe(e) // no transition, so no line
	lines := out.take(t)
	if len(lines) != 1 || lines[0]["msg"] != "plugin_state_changed" || lines[0]["plugin"] != config.ID || lines[0]["state"] != "ready" || lines[0]["reason"] != "probe_ok" {
		t.Fatalf("first probe: %v", lines)
	}

	instance.Store(strings.Repeat("d", 32))
	r.probe(e)
	lines = out.take(t)
	if len(lines) != 1 || lines[0]["msg"] != "plugin_epoch_changed" || lines[0]["reason"] != "instance_changed" {
		t.Fatalf("instance change: %v", lines)
	}

	down.Store(true)
	r.probe(e)
	r.probe(e)
	lines = out.take(t)
	if len(lines) != 2 || lines[0]["msg"] != "plugin_epoch_changed" || lines[0]["reason"] != "probe_lost" ||
		lines[1]["msg"] != "plugin_state_changed" || lines[1]["state"] != "unavailable" || lines[1]["reason"] != "probe_failed" {
		t.Fatalf("outage: %v", lines)
	}

	if _, err := r.Lifecycle(context.Background(), config.ID, "disable", auth.Principal{ID: "admin-1", Role: "admin"}, 0); err != nil {
		t.Fatal(err)
	}
	lines = out.take(t)
	if len(lines) != 2 || lines[0]["msg"] != "plugin_epoch_changed" || lines[0]["reason"] != "lifecycle_disable" ||
		lines[1]["msg"] != "plugin_state_changed" || lines[1]["state"] != "disabled" || lines[1]["reason"] != "lifecycle_disable" || lines[1]["actor"] != "admin-1" {
		t.Fatalf("lifecycle: %v", lines)
	}
	for _, secret := range []string{config.hostToken, config.serviceToken, server.URL} {
		if strings.Contains(out.b.String(), secret) {
			t.Fatal("a secret or endpoint reached the log")
		}
	}
}

func TestPluginHostAuthenticationFailuresAreCountedNotLogged(t *testing.T) {
	var out logBuffer
	log := translog.New(&out)
	r := New(context.Background(), []Config{fixtureConfig("http://127.0.0.1:1")}, fleet.NewStore(1))
	defer r.Close()
	r.SetLogger(log)
	for range 3 {
		if w := hostRequest(r, "GET", "/v1/session", strings.Repeat("x", 64)); w.Code != 401 {
			t.Fatalf("status %d", w.Code)
		}
	}
	if lines := out.take(t); len(lines) != 0 {
		t.Fatalf("a request wrote a line: %v", lines)
	}
	log.Flush()
	lines := out.take(t)
	if len(lines) != 1 || lines[0]["msg"] != "auth_failures" || lines[0]["listener"] != "plugin_host" || lines[0]["count"] != float64(3) {
		t.Fatalf("summary: %v", lines)
	}
}
