package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMetricsExportFollowsTheOTelEnvironment: export is opt-in through the
// SDK's standard variables and takes OTLP over grpc or http/protobuf.
func TestMetricsExportFollowsTheOTelEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want otlpProtocol
		err  string
	}{
		{name: "unset", env: nil},
		{name: "endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, want: "http/protobuf"},
		{name: "metrics endpoint over grpc", env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, want: "grpc"},
		{name: "metrics protocol wins", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"}, want: "http/protobuf"},
		{name: "otlp without endpoint", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp"}, want: "http/protobuf"},
		{name: "none", env: map[string]string{"OTEL_METRICS_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}},
		{name: "sdk disabled", env: map[string]string{"OTEL_SDK_DISABLED": "true", "OTEL_METRICS_EXPORTER": "otlp"}},
		{name: "prometheus", env: map[string]string{"OTEL_METRICS_EXPORTER": "prometheus"}, err: "otlp, or none"},
		{name: "http/json", env: map[string]string{"OTEL_METRICS_EXPORTER": "otlp", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"}, err: "grpc or http/protobuf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := metricsExport(func(name string) (string, bool) {
				v, ok := tc.env[name]
				return v, ok
			})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("= %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// TestTracesExportFollowsTheOTelEnvironment: traces follow the same rules
// with their own variables, and the shared endpoint turns on both signals.
func TestTracesExportFollowsTheOTelEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want otlpProtocol
		err  string
	}{
		{name: "unset", env: nil},
		{name: "shared endpoint", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, want: "http/protobuf"},
		{name: "metrics endpoint only", env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://collector:4318"}},
		{name: "traces endpoint over grpc", env: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4317", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, want: "grpc"},
		{name: "traces off, metrics on", env: map[string]string{"OTEL_TRACES_EXPORTER": "none", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}},
		{name: "zipkin", env: map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, err: "OTEL_TRACES_EXPORTER=zipkin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tracesExport(func(name string) (string, bool) {
				v, ok := tc.env[name]
				return v, ok
			})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("= %q, %v, want %q", got, err, tc.want)
			}
		})
	}
}

// TestServeExportsServerMetricsOverOTLP: with an OTLP endpoint in the
// environment, the running server says so and sends its metrics and its
// request spans there over http/protobuf.
func TestServeExportsServerMetricsOverOTLP(t *testing.T) {
	received, spans := make(chan []byte, 64), make(chan []byte, 64)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/metrics" {
			select {
			case received <- body:
			default:
			}
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			select {
			case spans <- body:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(receiver.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "100")
	t.Setenv("OTEL_BSP_SCHEDULE_DELAY", "100")

	path := writeTeam(t, teamYAML())
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 16)
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"--config", path, "--addr", "127.0.0.1:0"}, io.Discard, notices)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("serve exit = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	var url string
	metricsAnnounced, tracesAnnounced := false, false
	for !metricsAnnounced || !tracesAnnounced {
		select {
		case notice := <-notices:
			if rest, ok := strings.CutPrefix(notice, "serving API on "); ok {
				url = strings.TrimSuffix(strings.Fields(rest)[0], "/api/v1")
			}
			metricsAnnounced = metricsAnnounced || strings.HasPrefix(notice, "exporting server metrics over OTLP (http/protobuf)")
			tracesAnnounced = tracesAnnounced || strings.HasPrefix(notice, "exporting server traces over OTLP (http/protobuf)")
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not announce the metrics and traces export")
		}
	}
	go func() {
		for range notices {
		}
	}()
	// An unauthenticated request is refused, and still traced.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test server's own certificate
	if resp, err := client.Get(url + "/api/v1/runs?project=secret"); err == nil {
		resp.Body.Close()
	} else {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	gotMetrics, gotSpan := false, false
	for !gotMetrics || !gotSpan {
		select {
		case body := <-received:
			gotMetrics = gotMetrics || bytes.Contains(body, []byte("linea.run.queue.size")) && bytes.Contains(body, []byte("linea.storage.ready")) &&
				bytes.Contains(body, []byte("linea-server"))
		case body := <-spans:
			if bytes.Contains(body, []byte("secret")) {
				t.Fatal("an exported span carries the request's query")
			}
			gotSpan = gotSpan || bytes.Contains(body, []byte("GET /api/v1/runs")) && bytes.Contains(body, []byte("linea-server"))
		case <-deadline:
			t.Fatalf("exports carried metrics %v, the request span %v", gotMetrics, gotSpan)
		}
	}
}
