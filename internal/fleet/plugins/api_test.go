package plugins

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

func TestOpaquePluginAPIIsolationAndBounds(t *testing.T) {
	config := fixtureConfig("")
	config.APIVersion = "1"
	var incoming http.Header
	var headersMu sync.Mutex
	var forwarded atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == pluginsdk.ManifestPath {
			_ = json.NewEncoder(w).Encode(pluginsdk.Manifest{ID: config.ID, Version: config.Version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: strings.Repeat("c", 32), APIVersion: "1"})
			return
		}
		headersMu.Lock()
		incoming = req.Header.Clone()
		headersMu.Unlock()
		forwarded.Add(1)
		w.Header().Set("Set-Cookie", "must-not-forward")
		if strings.HasSuffix(req.URL.Path, "large") {
			_, _ = w.Write([]byte(`{"data":"` + strings.Repeat("x", 1<<20) + `"}`))
			return
		}
		if strings.HasSuffix(req.URL.Path, "redirect") {
			w.WriteHeader(302)
			return
		}
		w.Header().Set("ETag", `"2"`)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"plugin_owned":true}`))
	}))
	defer server.Close()
	config.Endpoint = server.URL
	r := New(context.Background(), []Config{config}, fleet.NewStore(1))
	defer r.Close()
	r.probe(r.entries[0])
	call := func(path, method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://fleet/api/v1/plugins/"+config.ID+"/api/"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer private-client-secret")
		req.Header.Set("Cookie", "private-cookie")
		req.Header.Set(pluginsdk.CallerRoleHeader, "admin")
		req.Header.Set(pluginsdk.ActingNameHeader, "forged")
		req.Header.Set("If-Match", `"1"`)
		req = req.WithContext(fleet.WithActing(req.Context(), &pluginsdk.Acting{ID: "user:alice", RequestID: "req-1"}))
		w := httptest.NewRecorder()
		r.ServeAPI(w, req, config.ID, path, auth.Principal{ID: "reader", Role: "reader"})
		return w
	}
	w := call("v1/custom", "POST", `{"opaque":"value"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), "plugin_owned") || w.Header().Get("Set-Cookie") != "" || w.Header().Get("ETag") != `"2"` {
		t.Fatal("opaque response contract failed", w.Code)
	}
	headersMu.Lock()
	if incoming.Get("Authorization") != "Bearer "+config.serviceToken || incoming.Get("Cookie") != "" || incoming.Get(pluginsdk.CallerIDHeader) != "reader" || incoming.Get(pluginsdk.CallerRoleHeader) != "reader" || incoming.Get(pluginsdk.InstanceHeader) == "" || incoming.Get(pluginsdk.EpochHeader) == "" || incoming.Get("If-Match") != `"1"` ||
		incoming.Get(pluginsdk.ActingIDHeader) != "user:alice" || incoming.Get(pluginsdk.ActingRequestHeader) != "req-1" || incoming.Values(pluginsdk.ActingNameHeader) != nil {
		t.Fatal("caller or directional credential isolation failed")
	}
	headersMu.Unlock()
	for _, path := range []string{"", "../private", "v1//private", "v1/%2fprivate", "v1/.."} {
		if got := call(path, "GET", "").Code; got != 400 {
			t.Fatal("ambiguous path accepted", path, got)
		}
	}
	if call("v1/custom", "POST", strings.Repeat("x", (64<<10)+1)).Code != 413 {
		t.Fatal("oversize request accepted")
	}
	if call("v1/custom", "POST", "invalid-json").Code != 400 {
		t.Fatal("invalid JSON accepted")
	}
	if call("v1/large", "GET", "").Code != 503 || call("v1/redirect", "GET", "").Code != 503 {
		t.Fatal("unbounded/redirect response accepted")
	}
	r.entries[0].config.APIVersion = ""
	if call("v1/custom", "GET", "").Code != 404 {
		t.Fatal("ungranted API accepted")
	}
	r.entries[0].config.APIVersion = "1"
	for i := 0; i < cap(r.entries[0].apiReads); i++ {
		r.entries[0].apiReads <- struct{}{}
	}
	busy := call("v1/custom", "GET", "")
	if busy.Code != 429 || busy.Header().Get("Retry-After") != "1" || r.apiRefused.Load() != 1 {
		t.Fatal("saturated ingress did not expose retryable overload")
	}
	for i := 0; i < cap(r.entries[0].apiReads); i++ {
		<-r.entries[0].apiReads
	}
	count := forwarded.Load()
	body := &retiringBody{Reader: strings.NewReader(`{}`), retire: func() {
		e := r.entries[0]
		e.mu.Lock()
		defer e.mu.Unlock()
		e.info.Desired = "disabled"
		e.info.State = "disabled"
		r.fenceLocked(e)
	}}
	req := httptest.NewRequest("POST", "http://fleet/api/v1/plugins/inventory-example/api/v1/custom", body)
	w = httptest.NewRecorder()
	r.ServeAPI(w, req, config.ID, "v1/custom", auth.Principal{ID: "reader", Role: "reader"})
	if w.Code != 503 || forwarded.Load() != count {
		t.Fatal("late request body bypassed retired authority")
	}
}

type retiringBody struct {
	io.Reader
	retire func()
	once   sync.Once
}

func (b *retiringBody) Read(p []byte) (int, error) { b.once.Do(b.retire); return b.Reader.Read(p) }
