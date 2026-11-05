package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
)

func TestReceiverAcceptsCaptureCopiesOnly(t *testing.T) {
	store := &captureStore{}
	traces, logs, metrics := synthetic(5)
	tr, _ := (&traceReceiver{store: store}).Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(traces))
	lr, _ := (&logReceiver{store: store}).Export(context.Background(), plogotlp.NewExportRequestFromLogs(logs))
	mr, _ := (&metricReceiver{store: store}).Export(context.Background(), pmetricotlp.NewExportRequestFromMetrics(metrics))
	if tr.PartialSuccess().RejectedSpans() != 1 || lr.PartialSuccess().RejectedLogRecords() != 1 || mr.PartialSuccess().RejectedDataPoints() != 1 || store.stats().Entries != 0 {
		t.Fatal("original telemetry entered the capture viewer")
	}
	attributes := traces.ResourceSpans().At(0).Resource().Attributes()
	attributes.PutStr("capture.session.id", "test-session")
	attributes.PutStr("capture.point", "ingress")
	attributes.PutStr("capture.pipeline", "traces")
	response, _ := (&traceReceiver{store: store}).Export(context.Background(), ptraceotlp.NewExportRequestFromTraces(traces))
	if response.PartialSuccess().RejectedSpans() != 0 || store.stats().Entries != 1 {
		t.Fatal("tagged copy refused")
	}
	item := store.page(0).Entries[0]
	if item.SessionID != "test-session" || item.Point != "ingress" || item.Service != "inventory" || !json.Valid(item.OTLP) {
		t.Fatal("capture metadata or valid JSON lost")
	}
	decoded := ptraceotlp.NewExportRequest()
	if err := decoded.UnmarshalJSON(item.OTLP); err != nil {
		t.Fatal(err)
	}
	span := decoded.Traces().ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	if span.TraceID() != traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).TraceID() {
		t.Fatal("OTLP trace identity changed")
	}
}

func TestBufferIsBoundedAndCursorsSurviveClear(t *testing.T) {
	store := &captureStore{}
	for i := 0; i < maxEntries+25; i++ {
		store.add(entry{Records: 1, OTLP: json.RawMessage(`{}`)})
	}
	stats := store.stats()
	if stats.Entries != maxEntries || stats.Evicted != 25 {
		t.Fatal("entry bound not enforced")
	}
	page := store.page(1)
	if !page.Missed || len(page.Entries) != 200 {
		t.Fatal("cursor did not report missed data or limit page size")
	}
	last := store.next
	store.clear()
	store.add(entry{Records: 1, OTLP: json.RawMessage(`{}`)})
	if store.page(last).Entries[0].ID <= last {
		t.Fatal("clear reused event identities")
	}
	store.clear()
	for range 40 {
		store.add(entry{Records: 1, OTLP: json.RawMessage(strings.Repeat(" ", maxEntryBytes))})
	}
	if store.stats().Bytes > maxBufferBytes || store.stats().Entries >= 40 {
		t.Fatal("byte bound not enforced")
	}
	if store.add(entry{Records: 1, OTLP: json.RawMessage(strings.Repeat(" ", maxEntryBytes+1))}) {
		t.Fatal("oversized entry accepted")
	}
}

func TestConcurrentBufferOperations(t *testing.T) {
	store := &captureStore{}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 1000 {
				store.add(entry{Records: 1, OTLP: json.RawMessage(`{}`)})
				store.stats()
				store.page(0)
			}
		})
	}
	workers.Go(func() {
		for range 100 {
			store.clear()
		}
	})
	workers.Wait()
	if store.stats().Entries > maxEntries {
		t.Fatal("concurrent writes exceeded bound")
	}
}

func TestCaptureOptionsBuildRestrictedSession(t *testing.T) {
	rate := 0.1
	options := captureOptions{Signals: []string{"traces", "logs", "metrics"}, Points: []string{"ingress", "egress"}, Service: "checkout", ErrorsOnly: true, DurationSeconds: 30, SamplingRate: &rate}
	encoded, err := captureRequest(options)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if len(request["capture_points"].([]any)) != 6 || request["filter"].(map[string]any)["resource"] == nil {
		t.Fatal("points or resource filter missing")
	}
	if _, ok := request["delivery"]; ok {
		t.Fatal("session controls delivery")
	}
	options.Service = `checkout" or true`
	if _, err := captureRequest(options); err == nil {
		t.Fatal("unrecognized service accepted")
	}
	options.Service = ""
	options.Signals = []string{"traces", "traces"}
	if _, err := captureRequest(options); err == nil {
		t.Fatal("duplicate signals accepted")
	}
	options.Signals = []string{"logs"}
	options.DurationSeconds = 301
	if _, err := captureRequest(options); err == nil {
		t.Fatal("excessive duration accepted")
	}
}

func TestCustomOTTLCombinesWithQuickFilters(t *testing.T) {
	options := captureOptions{Signals: []string{"traces", "logs", "metrics"}, Points: []string{"ingress"}, Service: "checkout", ErrorsOnly: true, DurationSeconds: 30}
	options.OTTL.Resource = []string{` IsString(attributes["service.name"]) `}
	options.OTTL.Span = []string{`name == "POST /api/checkout"`, `attributes["test.sequence"] > 10`}
	options.OTTL.Log = []string{`IsMatch(body, "request")`}
	options.OTTL.Metric = []string{`name == "test.request.duration"`}
	options.OTTL.Datapoint = []string{`value_double > 30`}
	encoded, err := captureRequest(options)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Filter struct {
			Resource []string
			Traces   struct{ Span []string }
			Logs     struct{ Log []string }
			Metrics  struct{ Metric, Datapoint []string }
		}
	}
	if err := json.Unmarshal(encoded, &session); err != nil {
		t.Fatal(err)
	}
	f := session.Filter
	if len(f.Resource) != 2 || f.Resource[1] != `IsString(attributes["service.name"])` || len(f.Traces.Span) != 3 || len(f.Logs.Log) != 2 || len(f.Metrics.Metric) != 1 || len(f.Metrics.Datapoint) != 2 {
		t.Fatalf("custom conditions replaced presets or lost their context: %s", encoded)
	}
	options.Signals = []string{"logs", "metrics"}
	if _, err := captureRequest(options); err == nil || !strings.Contains(err.Error(), "span OTTL requires") {
		t.Fatal("filter for an unselected signal silently accepted")
	}
	options.Signals = []string{"traces", "logs", "metrics"}
	options.OTTL.Span = []string{" "}
	if _, err := captureRequest(options); err == nil {
		t.Fatal("empty custom condition accepted")
	}
}

func TestHTTPControlsAndBrowserOriginBoundary(t *testing.T) {
	sessions := make(chan []byte, 1)
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			session, _ := io.ReadAll(r.Body)
			sessions <- session
			respond(w, 200, map[string]string{"state": "active"})
			return
		}
		respond(w, 200, map[string]any{"enabled": true, "capture_points": []any{}, "sessions": []any{}})
	}))
	defer control.Close()
	gen := &generator{state: generatorState{Enabled: true, Rate: 5}}
	server := httptest.NewServer((&labServer{store: &captureStore{}, generator: gen, controlURL: control.URL, client: control.Client()}).handler())
	defer server.Close()
	client := server.Client()
	post := func(path, body, origin string) int {
		request, _ := http.NewRequest("POST", server.URL+path, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if code := post("/api/generator", `{"enabled":false,"rate":10}`, server.URL); code != 200 || gen.snapshot().Enabled {
		t.Fatal("generator control failed")
	}
	if code := post("/api/generator", `{"enabled":true,"rate":0}`, ""); code != 400 {
		t.Fatal("invalid rate accepted")
	}
	if code := post("/api/generator", `{"enabled":true,"rate":5}`, "https://untrusted.example"); code != 403 {
		t.Fatal("cross-origin browser mutation accepted")
	}
	if code := post("/api/capture", `{"signals":["traces"],"points":["ingress"],"duration_seconds":60}`, ""); code != 200 {
		t.Fatal("capture start proxy failed")
	}
	session := <-sessions
	if !bytes.Contains(session, []byte("testlab")) || bytes.Contains(session, []byte("authorized_raw")) {
		t.Fatal("invalid control request")
	}
	if code := post("/api/capture", `{"endpoint":"attacker:4317"}`, ""); code != 400 {
		t.Fatal("unknown capture field accepted")
	}
	response, err := client.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	html, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !bytes.Contains(html, []byte("Collector Capture Lab")) || response.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("embedded web page not served")
	}
}
