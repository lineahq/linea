package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestRequestSpansNameTheRouteAndNothingElse: a request span is named by its
// route, records only the method, route and status, continues an incoming
// traceparent, and upgrades get none.
func TestRequestSpansNameTheRouteAndNothingElse(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" && !trace.SpanContextFromContext(r.Context()).IsValid() {
			t.Error("the handler's context carries no span")
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /api/v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("the response cannot be flushed")
		}
	})
	route := func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		return pattern
	}
	srv := httptest.NewServer(traced(mux, provider, route))
	t.Cleanup(srv.Close)
	send := func(path string, header map[string]string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	const parent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	send("/api/v1/runs/run-123?token=secret", map[string]string{"traceparent": parent, "Authorization": "Bearer secret", "User-Agent": "secret-agent"})
	send("/api/v1/stream", nil)
	send("/assets/app.js", nil)
	send("/api/v1/runs/run-123", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"})

	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3 (none for the upgrade)", len(spans))
	}
	for i, want := range []struct {
		name, route string
		status      int64
		failed      bool
	}{
		{"GET /api/v1/runs/{id}", "/api/v1/runs/{id}", 500, true},
		{"GET /api/v1/stream", "/api/v1/stream", 200, false},
		{"GET", "", 404, false},
	} {
		sp := spans[i]
		if sp.Name() != want.name || sp.SpanKind() != trace.SpanKindServer {
			t.Errorf("span %d: %q %v, want %q", i, sp.Name(), sp.SpanKind(), want.name)
		}
		attrs := map[string]string{}
		for _, kv := range sp.Attributes() {
			attrs[string(kv.Key)] = kv.Value.String()
			if strings.Contains(kv.Value.String(), "secret") || strings.Contains(kv.Value.String(), "run-123") {
				t.Errorf("span %d records %s=%s", i, kv.Key, kv.Value.String())
			}
		}
		wantAttrs := 2
		if want.route != "" {
			wantAttrs = 3
			if attrs["http.route"] != want.route {
				t.Errorf("span %d: route %q", i, attrs["http.route"])
			}
		}
		if len(attrs) != wantAttrs || attrs["http.request.method"] != "GET" {
			t.Errorf("span %d: attributes %v", i, attrs)
		}
		var status int64
		for _, kv := range sp.Attributes() {
			if kv.Key == "http.response.status_code" {
				status = kv.Value.AsInt64()
			}
		}
		if status != want.status || (sp.Status().Code == codes.Error) != want.failed {
			t.Errorf("span %d: status %d, %+v", i, status, sp.Status())
		}
	}
	if got := spans[0].Parent(); got.TraceID().String() != "0af7651916cd43dd8448eb211c80319c" || got.SpanID().String() != "b7ad6b7169203331" {
		t.Errorf("first span's parent %v, want the incoming traceparent", got)
	}
}

// TestNoProviderLeavesRequestsUntraced: without export the handler is served
// as it is.
func TestNoProviderLeavesRequestsUntraced(t *testing.T) {
	h := http.NewServeMux()
	if got := traced(h, nil, func(*http.Request) string { return "" }); got != http.Handler(h) {
		t.Fatal("a server without trace export wraps its handler")
	}
}

func TestSpanNamesComeFromTheRoutePattern(t *testing.T) {
	for _, tc := range []struct{ method, pattern, name, route string }{
		{"GET", "GET /api/v1/runs/{id}", "GET /api/v1/runs/{id}", "/api/v1/runs/{id}"},
		{"POST", "/mcp", "POST /mcp", "/mcp"},
		{"GET", "", "GET", ""},
	} {
		if name, route := spanName(tc.method, tc.pattern); name != tc.name || route != tc.route {
			t.Errorf("spanName(%q, %q) = %q, %q", tc.method, tc.pattern, name, route)
		}
	}
}
