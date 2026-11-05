package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type fakeBuildMetrics struct {
	m   platform.BuildMetrics
	err error
}

func (f fakeBuildMetrics) BuildMetrics(context.Context) (platform.BuildMetrics, error) {
	return f.m, f.err
}

// TestMetricsServeTheBuildQueueAsPrometheusText: GET /metrics writes the
// counts in the Prometheus text format; another method or path is not
// served, and an unreadable queue is 503 without the cause.
func TestMetricsServeTheBuildQueueAsPrometheusText(t *testing.T) {
	h := Metrics(fakeBuildMetrics{m: platform.BuildMetrics{Queued: 3, Running: 1, Builders: 2, BuilderSlots: 4}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	want := `# HELP linea_runtime_builds Runtime builds of the installation that are queued or running.
# TYPE linea_runtime_builds gauge
linea_runtime_builds{state="queued"} 3
linea_runtime_builds{state="running"} 1
# HELP linea_runtime_builds_unfinished Runtime builds of the installation that are queued or running, together: the builder slots they need.
# TYPE linea_runtime_builds_unfinished gauge
linea_runtime_builds_unfinished 4
# HELP linea_builders_connected Builders connected to this server instance.
# TYPE linea_builders_connected gauge
linea_builders_connected 2
# HELP linea_builder_slots Builds the builders connected to this server instance run at once.
# TYPE linea_builder_slots gauge
linea_builder_slots 4
`
	if rec.Code != http.StatusOK || string(body) != want || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /metrics = %d %q\n%s", rec.Code, rec.Header().Get("Content-Type"), body)
	}
	for _, tc := range []struct{ method, path string }{{http.MethodPost, "/metrics"}, {http.MethodGet, "/api/v1/runs"}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	Metrics(fakeBuildMetrics{err: errors.New("postgres://secret@db")}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body, _ := io.ReadAll(rec.Body); rec.Code != http.StatusServiceUnavailable || string(body) != "the build queue cannot be read\n" {
		t.Fatalf("unreadable queue = %d %q", rec.Code, body)
	}
}
