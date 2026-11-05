package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// BuildMetricsSource is the platform operation the metrics endpoint maps.
type BuildMetricsSource interface {
	BuildMetrics(ctx context.Context) (platform.BuildMetrics, error)
}

// Metrics serves GET /metrics in the Prometheus text format:
// the build queue and this instance's builders, counts only, for an
// autoscaler. It takes no credentials; linea-server exposes it on a listener of
// its own, never beside the API.
func Metrics(source BuildMetricsSource) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		m, err := source.BuildMetrics(r.Context())
		if err != nil {
			http.Error(w, "the build queue cannot be read", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(formatMetrics(m)))
	})
	return mux
}

// formatMetrics writes the metrics in the Prometheus text format.
func formatMetrics(m platform.BuildMetrics) string {
	var b strings.Builder
	b.WriteString("# HELP linea_runtime_builds Runtime builds of the installation that are queued or running.\n")
	b.WriteString("# TYPE linea_runtime_builds gauge\n")
	fmt.Fprintf(&b, "linea_runtime_builds{state=\"queued\"} %d\n", m.Queued)
	fmt.Fprintf(&b, "linea_runtime_builds{state=\"running\"} %d\n", m.Running)
	b.WriteString("# HELP linea_runtime_builds_unfinished Runtime builds of the installation that are queued or running, together: the builder slots they need.\n")
	b.WriteString("# TYPE linea_runtime_builds_unfinished gauge\n")
	fmt.Fprintf(&b, "linea_runtime_builds_unfinished %d\n", m.Queued+m.Running)
	b.WriteString("# HELP linea_builders_connected Builders connected to this server instance.\n")
	b.WriteString("# TYPE linea_builders_connected gauge\n")
	fmt.Fprintf(&b, "linea_builders_connected %d\n", m.Builders)
	b.WriteString("# HELP linea_builder_slots Builds the builders connected to this server instance run at once.\n")
	b.WriteString("# TYPE linea_builder_slots gauge\n")
	fmt.Fprintf(&b, "linea_builder_slots %d\n", m.BuilderSlots)
	return b.String()
}
