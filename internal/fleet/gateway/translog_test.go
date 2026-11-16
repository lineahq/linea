package gateway

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/translog"
	"github.com/open-telemetry/opamp-go/protobufs"
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

func (l *logBuffer) lines(t *testing.T) []map[string]any {
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
	return out
}

// serveLogged is serve with the transition log bound before any agent connects.
func serveLogged(t *testing.T, src *source, hs *holds, log *translog.Logger) (*Gateway, string, string) {
	t.Helper()
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	g, err := New(fleet.NewStore(4), a, 4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	g.SetLogger(log)
	g.SetConfigManagement(configmgmt.NewReconciler(src, 1<<20), hs)
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	return g, "ws" + strings.TrimPrefix(h.URL, "http") + "/v1/opamp", token
}

func TestWithheldConfigIsLoggedOncePerSessionAndPlan(t *testing.T) {
	uid := bytes.Repeat([]byte{5}, 16)
	id := hex.EncodeToString(uid)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n"}, credentials: map[string]map[string]string{"memory-agent": {"env": "prod"}}}
	src.assign(t, id, 1)
	hs := &holds{held: map[string]string{}}
	var out logBuffer
	log := translog.New(&out)
	g, url, token := serveLogged(t, src, hs, log)
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	hash := buildHash("filelog")
	a.exchange(&protobufs.AgentToServer{Capabilities: reportsComponents, AvailableComponents: build(hash)})
	for range 3 {
		a.exchange(&protobufs.AgentToServer{Capabilities: reportsComponents, AvailableComponents: build(hash, "filelog")})
	}
	a.quiet(reportsComponents)
	var withheld []map[string]any
	for _, l := range out.lines(t) {
		if l["msg"] == "config_withheld" {
			withheld = append(withheld, l)
		}
	}
	if len(withheld) != 1 || withheld[0]["agent"] != id || withheld[0]["reason"] != "incompatible_components" {
		t.Fatalf("withheld lines = %v", withheld)
	}
	if strings.Contains(out.b.String(), "otlp") || strings.Contains(out.b.String(), "filelog") {
		t.Error("component or config text reached the log")
	}
}

func TestAgentFailureHoldIsLogged(t *testing.T) {
	uid := bytes.Repeat([]byte{9}, 16)
	id := hex.EncodeToString(uid)
	src := &source{bodies: map[int]string{1: "receivers: {otlp: {}}\n"}, credentials: map[string]map[string]string{"memory-agent": {"env": "prod"}}}
	src.assign(t, id, 1)
	hs := &holds{held: map[string]string{}}
	var out logBuffer
	g, url, token := serveLogged(t, src, hs, translog.New(&out))
	a := &agent{t: t, g: g, c: dial(t, url, token), uid: uid}
	want := configmgmt.Hash([]configmgmt.Entry{{Name: "10-base", Body: []byte(src.bodies[1])}})
	a.report(acceptsRemoteConfig, nil)
	a.report(acceptsRemoteConfig, status(want, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED))
	var held []map[string]any
	for _, l := range out.lines(t) {
		if l["msg"] == "config_withheld" {
			held = append(held, l)
		}
	}
	if len(held) != 1 || held[0]["agent"] != id || held[0]["reason"] != "agent_reported_failure" {
		t.Fatalf("hold lines = %v", held)
	}
}

func TestAgentAuthenticationFailuresAreSummarizedNotLoggedPerRequest(t *testing.T) {
	var out logBuffer
	log := translog.New(&out)
	token := strings.Repeat("a", 32)
	a, _ := auth.New(token)
	g, err := New(fleet.NewStore(4), a, 4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	g.SetLogger(log)
	h := httptest.NewServer(g)
	t.Cleanup(func() { g.Close(); h.Close() })
	for range 4 {
		r, _ := http.NewRequest("GET", h.URL+"/v1/opamp", nil)
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("z", 32))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if n := len(out.lines(t)); n != 0 {
		t.Fatalf("requests wrote %d lines", n)
	}
	log.Flush()
	lines := out.lines(t)
	if len(lines) != 1 || lines[0]["msg"] != "auth_failures" || lines[0]["listener"] != "agent" || lines[0]["count"] != float64(4) {
		t.Fatalf("summary = %v", lines)
	}
}
