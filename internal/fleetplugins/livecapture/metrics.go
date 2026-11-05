package livecapture

import (
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/pluginsdk/metrics"
)

type pluginMetrics struct {
	metadata         atomic.Pointer[metadataView]
	delivery         [3]metrics.Duration
	deliveryErrors   atomic.Uint64
	deliveryCapacity atomic.Uint64
	uncertain        atomic.Uint64
	accepted         [3]atomic.Uint64
	rejected         [3]atomic.Uint64
	acks             atomic.Uint64
	ackRetries       atomic.Uint64
	admitted         atomic.Uint64
	admissionRefused atomic.Uint64
	withdrawn        atomic.Uint64
	credentialClosed atomic.Uint64
	desiredRejected  atomic.Uint64
}
type metadataView struct {
	gauges map[string]float64
	at     time.Time
}
type storageMetrics struct {
	wait     metrics.Duration
	duration metrics.Duration
	errors   atomic.Uint64
	waiters  atomic.Int64
}
type MetricSnapshot struct {
	Gauges    map[string]float64          `json:"gauges"`
	Counters  map[string]uint64           `json:"counters"`
	Durations map[string]metrics.Snapshot `json:"durations"`
	Limits    map[string]uint64           `json:"limits"`
}

func signalIndex(signal string) int {
	switch signal {
	case "traces":
		return 0
	case "logs":
		return 1
	default:
		return 2
	}
}

// Metrics reads only in-memory aggregates, never catalogs, telemetry or storage.
func (s *Server) Metrics() MetricSnapshot {
	m := MetricSnapshot{Gauges: map[string]float64{}, Counters: map[string]uint64{}, Durations: map[string]metrics.Snapshot{}, Limits: map[string]uint64{
		"peers": uint64(s.config.peerLimit()), "control_connections": uint64(s.config.peerLimit() + 16), "data_connections": 64, "http_connections": 64, "operations": 32, "channels": 32, "channel_bytes": 1 << 20, "channel_batches": 16, "channel_receipts": 1024, "otlp_bytes": 256 << 10, "deliveries": 16, "peer_page_bytes": PeerPageBytes, "peer_page_count": 64,
	}}
	var view *metadataView
	if s.mu.TryLock() {
		m.Gauges["draining"] = 0
		if s.draining {
			m.Gauges["draining"] = 1
		}
		m.Gauges["peers"] = float64(len(s.peers))
		m.Gauges["channels"] = float64(len(s.channels))
		m.Gauges["operations"] = float64(len(s.operations))
		if s.hostOK {
			m.Gauges["host_ready"] = 1
		} else {
			m.Gauges["host_ready"] = 0
		}
		if s.dbOK {
			m.Gauges["storage_ready"] = 1
		} else {
			m.Gauges["storage_ready"] = 0
		}
		for _, state := range []string{"starting", "active", "stopping", "completed", "stopped", "failed", "expired", "interrupted"} {
			m.Gauges["operations_"+state] = 0
		}
		for _, a := range s.operations {
			m.Gauges["operations_"+a.op.State]++
		}
		for _, key := range []string{"pending_batches", "pending_bytes", "retained_receipts"} {
			m.Gauges[key] = 0
		}
		for _, c := range s.channels {
			m.Gauges["pending_batches"] += float64(len(c.batches))
			m.Gauges["pending_bytes"] += float64(c.bytes)
			m.Gauges["retained_receipts"] += float64(len(c.receipts))
		}
		// Do not let a scrape queue behind storage IO performed by admission.
		// Publish an immutable copy for scrapes that encounter a busy authority.
		values := make(map[string]float64, len(m.Gauges))
		for key, value := range m.Gauges {
			values[key] = value
		}
		view = &metadataView{gauges: values, at: time.Now()}
		s.observed.metadata.Store(view)
		s.mu.Unlock()
		m.Gauges["metadata_snapshot_stale"] = 0
	} else {
		view = s.observed.metadata.Load()
		if view != nil {
			for key, value := range view.gauges {
				m.Gauges[key] = value
			}
		}
		m.Gauges["metadata_snapshot_stale"] = 1
	}
	if view != nil {
		m.Gauges["metadata_snapshot_available"] = 1
		m.Gauges["metadata_snapshot_age_seconds"] = time.Since(view.at).Seconds()
	} else {
		m.Gauges["metadata_snapshot_available"] = 0
	}
	m.Gauges["inflight_deliveries"] = float64(len(s.dataGate))
	for i, name := range []string{"control", "data", "http"} {
		m.Gauges["connections_"+name] = float64(len(s.budgets[i].slots))
		m.Counters["connections_refused_"+name+"_total"] = s.budgets[i].refused.Load()
	}
	m.Counters["delivery_errors_total"] = s.observed.deliveryErrors.Load()
	m.Counters["delivery_capacity_total"] = s.observed.deliveryCapacity.Load()
	m.Counters["receipt_uncertain_total"] = s.observed.uncertain.Load()
	m.Counters["receipts_total"] = s.observed.acks.Load()
	m.Counters["receipt_retries_total"] = s.observed.ackRetries.Load()
	m.Counters["captures_admitted_total"] = s.observed.admitted.Load()
	m.Counters["capture_admission_refused_total"] = s.observed.admissionRefused.Load()
	m.Counters["captures_withdrawn_total"] = s.observed.withdrawn.Load()
	m.Counters["peers_credential_closed_total"] = s.observed.credentialClosed.Load()
	m.Counters["desired_state_rejected_total"] = s.observed.desiredRejected.Load()
	for i, signal := range []string{"traces", "logs", "metrics"} {
		m.Counters["consumer_accepted_"+signal+"_records_total"] = s.observed.accepted[i].Load()
		m.Counters["consumer_rejected_"+signal+"_records_total"] = s.observed.rejected[i].Load()
		m.Durations["delivery_"+signal+"_duration_seconds"] = s.observed.delivery[i].Snapshot()
	}
	if repo, ok := s.repo.(interface{ storageObservations() *storageMetrics }); ok {
		storage := repo.storageObservations()
		m.Gauges["storage_waiters"] = float64(storage.waiters.Load())
		m.Counters["storage_errors_total"] = storage.errors.Load()
		m.Durations["storage_wait_seconds"] = storage.wait.Snapshot()
		m.Durations["storage_operation_seconds"] = storage.duration.Snapshot()
	}
	return m
}
func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func (m MetricSnapshot) Write(w io.Writer) {
	const prefix = "linea_fleet_livecapture_"
	for _, key := range sortedKeys(m.Gauges) {
		fmt.Fprintf(w, "# TYPE %s%s gauge\n%s%s %g\n", prefix, key, prefix, key, m.Gauges[key])
	}
	for _, key := range sortedKeys(m.Counters) {
		fmt.Fprintf(w, "# TYPE %s%s counter\n%s%s %d\n", prefix, key, prefix, key, m.Counters[key])
	}
	for _, key := range sortedKeys(m.Limits) {
		fmt.Fprintf(w, "# TYPE %slimit_%s gauge\n%slimit_%s %d\n", prefix, key, prefix, key, m.Limits[key])
	}
	for _, key := range sortedKeys(m.Durations) {
		m.Durations[key].Write(w, prefix+key)
	}
}

func (s *Server) observeDelivery(signal string, started time.Time, err error) {
	s.observed.delivery[signalIndex(signal)].Observe(time.Since(started))
	if err != nil {
		s.observed.deliveryErrors.Add(1)
	}
}
