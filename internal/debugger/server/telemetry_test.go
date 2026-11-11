package server

import (
	"strings"
	"testing"
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
