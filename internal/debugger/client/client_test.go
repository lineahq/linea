package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestNewAcceptsOnlyLoopbackHTTP(t *testing.T) {
	for _, ok := range []string{"http://127.0.0.1:7070", "http://localhost:7070/", "http://[::1]:7070", "http://127.0.0.1"} {
		if _, err := New(ok); err != nil {
			t.Errorf("New(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		"https://127.0.0.1:7070", "http://example.com:7070", "http://10.0.0.1:7070", "http://0.0.0.0:7070",
		"http://127.0.0.2:7070", "http://localhost.example.com", "http://user:pw@127.0.0.1:7070",
		"http://127.0.0.1:7070/api", "http://127.0.0.1:7070/?x=1", "127.0.0.1:7070", "",
	} {
		if _, err := New(bad); err == nil {
			t.Errorf("New(%q) accepted", bad)
		}
	}
}

// TestTokenReachesTeamServersOverHTTPSOnly: with an API token the client
// also accepts https URLs on any host and presents the token on every
// request; a token never goes over plain HTTP off this machine.
func TestTokenReachesTeamServersOverHTTPSOnly(t *testing.T) {
	const token = "odt_secret"
	for _, ok := range []string{"https://debugger.example.com", "https://10.0.0.1:8443/", "http://127.0.0.1:7070"} {
		if _, err := NewWithToken(ok, token); err != nil {
			t.Errorf("NewWithToken(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"http://debugger.example.com", "https://user:pw@debugger.example.com", "https://debugger.example.com/api", "https://"} {
		if _, err := NewWithToken(bad, token); err == nil {
			t.Errorf("NewWithToken(%q) accepted", bad)
		}
	}
	if _, err := NewWithToken("https://debugger.example.com", "odt_a b"); err == nil {
		t.Error("a token with a space was accepted")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthenticated"}`)
			return
		}
		fmt.Fprint(w, `[{"id":"t1","label":"laptop"}]`)
	}))
	defer srv.Close()
	c, err := NewWithToken(srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport = srv.Client().Transport
	if list, err := c.ListTokens(context.Background(), false); err != nil || len(list) != 1 || list[0].ID != "t1" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestUnreachableServerSaysSo(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c, err := New("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Inspect(context.Background(), api.InspectRequest{ConfigYAML: "x"})
	if err == nil || !strings.Contains(err.Error(), "cannot reach the debugger server at http://"+addr) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefusalsCarryTheServersMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"path outside the allowed roots"}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.Run(context.Background(), api.Request{})
	var refused *Error
	if !errors.As(err, &refused) || refused.Status != http.StatusForbidden || refused.Message != "path outside the allowed roots" {
		t.Fatalf("err = %v", err)
	}
}

// fakeSuites records suite submissions and serves one pass per member.
type fakeSuites struct {
	mu        sync.Mutex
	sizes     []int
	cancelled []string
	block     chan struct{}
	waiting   chan struct{}
}

func (f *fakeSuites) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/suites":
		var req api.SuiteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := fmt.Sprintf("s%d", len(f.sizes))
		f.sizes = append(f.sizes, len(req.Scenarios))
		suite := api.Suite{ID: id, Config: req.ConfigLabel}
		for i, s := range req.Scenarios {
			suite.Members = append(suite.Members, api.SuiteMember{Label: s.ScenarioLabel, RunID: fmt.Sprintf("%s-r%d", id, i)})
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(suite)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/report"):
		if f.block != nil {
			f.mu.Unlock()
			close(f.waiting)
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		n := f.sizes[len(f.sizes)-1]
		scenarios := make([]map[string]any, n)
		for i := range scenarios {
			outcome := assertion.Pass
			if len(f.sizes) == 2 {
				outcome = assertion.Fail
			}
			scenarios[i] = map[string]any{"path": fmt.Sprint(i), "outcome": outcome, "duration_seconds": 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"test_schema": "x", "config": "c", "outcome": "pass", "scenarios": scenarios})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
		f.cancelled = append(f.cancelled, strings.Split(r.URL.Path, "/")[4])
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

func scenarios(n int) api.SuiteRequest {
	req := api.SuiteRequest{ConfigYAML: "c", ConfigLabel: "c.yaml"}
	for i := range n {
		req.Scenarios = append(req.Scenarios, api.SuiteScenario{ScenarioYAML: "s", ScenarioLabel: fmt.Sprint(i)})
	}
	return req
}

func TestRunSuiteSplitsIntoSuitesOfThirtyTwo(t *testing.T) {
	f := &fakeSuites{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := c.RunSuite(context.Background(), scenarios(33), "c.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.sizes) != "[32 1]" || len(rep.Scenarios) != 33 || rep.Outcome != assertion.Fail || rep.Config != "c.yaml" || rep.Schema != "x" {
		t.Fatalf("sizes %v, report %+v", f.sizes, rep)
	}
}

func TestInterruptedSuiteCancelsItsRuns(t *testing.T) {
	f := &fakeSuites{block: make(chan struct{}), waiting: make(chan struct{})}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-f.waiting; cancel() }()
	_, err = c.RunSuite(ctx, scenarios(2), "c.yaml")
	if err == nil || !strings.Contains(err.Error(), "interrupted: cancelled 2 submitted run(s)") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.cancelled) != "[s0-r0 s0-r1]" {
		t.Fatalf("cancelled %v", f.cancelled)
	}
}

// fakeComparisons admits two runs and answers one comparison, or blocks it.
type fakeComparisons struct {
	mu        sync.Mutex
	query     url.Values
	cancelled []string
	block     bool
	waiting   chan struct{}
}

func (f *fakeComparisons) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/comparisons":
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(api.ComparisonRuns{Before: api.Run{ID: "a"}, After: api.Run{ID: "b"}})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/a/compare/b":
		f.mu.Lock()
		f.query = r.URL.Query()
		f.mu.Unlock()
		if f.block {
			close(f.waiting)
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"comparison_schema":"0.1","outcome":"different","before":{"id":"a"},"after":{"id":"b"},"exporters":[{"exporter":"e","signal":"logs","before":1,"after":1,"changes":[{"path":"log.body","op":"modified","count":1,"before":{"value":12345678901234567890}}]}]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
		f.mu.Lock()
		f.cancelled = append(f.cancelled, strings.Split(r.URL.Path, "/")[4])
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{}`)
	default:
		http.NotFound(w, r)
	}
}

func TestCompareConfigsWaitsAndKeepsNumbersAsWritten(t *testing.T) {
	f := &fakeComparisons{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, cmp, err := c.CompareConfigs(context.Background(), api.ComparisonRequest{}, []string{`log.attributes["id"]`, "span.name"})
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Outcome != platform.ComparisonDifferent || len(raw) == 0 || fmt.Sprint(f.query["ignore"]) != `[log.attributes["id"] span.name]` || f.query.Get("wait") != "true" {
		t.Fatalf("comparison %+v, query %v", cmp, f.query)
	}
	if v := cmp.Exporters[0].Changes[0].Before.Value; fmt.Sprint(v) != "12345678901234567890" {
		t.Fatalf("value = %v (%T)", v, v)
	}
}

func TestInterruptedComparisonCancelsBothRuns(t *testing.T) {
	f := &fakeComparisons{block: true, waiting: make(chan struct{})}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-f.waiting; cancel() }()
	_, _, err = c.CompareConfigs(ctx, api.ComparisonRequest{}, nil)
	if err == nil || !strings.Contains(err.Error(), "interrupted: cancelled 2 submitted run(s)") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.cancelled) != "[a b]" {
		t.Fatalf("cancelled %v", f.cancelled)
	}
}

// TestAWaitOutlivesAStoppedInstance: a wait the load balancer answers with a
// gateway error, as when the instance serving it stops, is sent again; the
// server's own refusal is not.
func TestAWaitOutlivesAStoppedInstance(t *testing.T) {
	var waits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/runs/run-1/report":
			waits++
			if waits == 1 {
				http.Error(w, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"report":true}`))
		case "/api/v1/runs/run-2/report":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"no worker","code":"unavailable"}`))
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.RunReport(context.Background(), "run-1", true)
	if err != nil || string(raw) != `{"report":true}` || waits != 2 {
		t.Fatalf("wait = %s, %v after %d requests", raw, err, waits)
	}
	var e *Error
	if _, err := c.RunReport(context.Background(), "run-2", true); !errors.As(err, &e) || e.Code != "unavailable" {
		t.Fatalf("refused wait = %v", err)
	}
}
