package server

import (
	"bytes"
	"context"
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

// TestServeExportsServerMetricsOverOTLP: with an OTLP endpoint in the
// environment, the running server says so and sends its metrics there over
// http/protobuf.
func TestServeExportsServerMetricsOverOTLP(t *testing.T) {
	received := make(chan []byte, 64)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/metrics" {
			select {
			case received <- body:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(receiver.Close)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "100")

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
	announced := false
	for !announced {
		select {
		case notice := <-notices:
			announced = strings.HasPrefix(notice, "exporting server metrics over OTLP (http/protobuf)")
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not announce the metrics export")
		}
	}
	go func() {
		for range notices {
		}
	}()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case body := <-received:
			if bytes.Contains(body, []byte("linea.run.queue.size")) && bytes.Contains(body, []byte("linea.storage.ready")) &&
				bytes.Contains(body, []byte("linea-server")) {
				return
			}
		case <-deadline:
			t.Fatal("no export carried the server's metrics")
		}
	}
}
