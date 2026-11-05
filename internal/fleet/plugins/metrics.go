package plugins

import (
	"fmt"
	"io"
	"time"
)

func (r *Registry) WriteMetrics(w io.Writer) {
	states := map[string]int{"ready": 0, "unavailable": 0, "incompatible": 0, "disabled": 0, "draining": 0, "unknown": 0}
	reads, writes := 0, 0
	for _, e := range r.entries {
		state := "unknown"
		if e.mu.TryRLock() {
			state = e.info.State
			if state == "ready" && (e.info.Desired != "enabled" || e.info.CheckedAt == nil || time.Since(*e.info.CheckedAt) >= 3*time.Second) {
				state = "unavailable"
			}
			e.mu.RUnlock()
		}
		states[state]++
		reads += len(e.apiReads)
		writes += len(e.apiWrites)
	}
	fmt.Fprint(w, "# TYPE linea_fleet_plugins gauge\n")
	for _, state := range []string{"ready", "unavailable", "incompatible", "disabled", "draining", "unknown"} {
		fmt.Fprintf(w, "linea_fleet_plugins{state=\"%s\"} %d\n", state, states[state])
	}
	fmt.Fprintf(w, "# TYPE linea_fleet_plugin_api_inflight gauge\nlinea_fleet_plugin_api_inflight{method=\"read\"} %d\nlinea_fleet_plugin_api_inflight{method=\"write\"} %d\n# TYPE linea_fleet_plugin_api_refused_total counter\nlinea_fleet_plugin_api_refused_total %d\n# TYPE linea_fleet_plugin_api_errors_total counter\nlinea_fleet_plugin_api_errors_total %d\n", reads, writes, r.apiRefused.Load(), r.apiErrors.Load())
	r.apiDuration.Snapshot().Write(w, "linea_fleet_plugin_api_duration_seconds")
}
