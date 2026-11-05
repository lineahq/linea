package agentsimulator

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lineahq/linea/pluginsdk/metrics"
)

type Counters struct {
	Active           int    `json:"active"`
	PeakActive       int    `json:"peak_active"`
	Ready            int    `json:"ready"`
	DialAttempts     uint64 `json:"dial_attempts"`
	DialFailures     uint64 `json:"dial_failures"`
	Acknowledged     uint64 `json:"messages_acknowledged"`
	ServerErrors     uint64 `json:"server_errors"`
	RequestErrors    uint64 `json:"request_errors"`
	UnexpectedErrors uint64 `json:"unexpected_errors"`
	Reconnects       uint64 `json:"planned_reconnects"`
	ReconnectRetries uint64 `json:"reconnect_retries"`
	Resyncs          uint64 `json:"full_state_requests"`
	BytesSent        uint64 `json:"bytes_sent"`
	BytesReceived    uint64 `json:"bytes_received"`
	HealthChanges    uint64 `json:"health_changes"`
	Refusals         uint64 `json:"admission_refusals"`
	Churned          uint64 `json:"identities_churned"`
	StormLost        int    `json:"storm_sessions_lost"`
	StormRecovered   int    `json:"storm_sessions_recovered"`
	ConfigsOffered   uint64 `json:"configs_offered"`
	ConfigsApplied   uint64 `json:"configs_applied"`
	ConfigsFailed    uint64 `json:"configs_failed"`
}
type Latency struct {
	RecentSamples int              `json:"recent_samples"`
	P50Millis     float64          `json:"p50_ms"`
	P95Millis     float64          `json:"p95_ms"`
	MaxMillis     float64          `json:"max_ms"`
	All           metrics.Snapshot `json:"all"`
}
type Observation struct {
	Phase                 string  `json:"phase"`
	Success               bool    `json:"success"`
	Failure               string  `json:"failure,omitempty"`
	ElapsedSeconds        float64 `json:"elapsed_seconds"`
	SteadyMinAcknowledged uint64  `json:"steady_min_acknowledged"`
	// The current storm, the sessions it lost and recovered, and its timings:
	// from the first lost session, and from the first recovered one, until
	// every lost session recovered.
	Storm                          int     `json:"storm,omitempty"`
	StormLostNow                   int64   `json:"storm_lost,omitempty"`
	StormRecoveredNow              int64   `json:"storm_recovered,omitempty"`
	StormSeconds                   float64 `json:"storm_seconds,omitempty"`
	StormFromFirstReconnectSeconds float64 `json:"storm_from_first_reconnect_seconds,omitempty"`
	Counters
	// RoundTrip times health reports, from SetHealth to the next response,
	// including the client's own queueing.
	RoundTrip Latency `json:"health_round_trip"`
}
type statistics struct {
	mu      sync.Mutex
	c       Counters
	samples [4096]time.Duration
	n       uint64
	max     time.Duration
	rtt     metrics.Duration
	// Bytes on the wire, counted per socket read and write.
	sent, received atomic.Uint64
}

func (s *statistics) update(fn func(*Counters)) { s.mu.Lock(); defer s.mu.Unlock(); fn(&s.c) }
func (s *statistics) latency(d time.Duration) {
	s.rtt.Observe(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples[s.n%uint64(len(s.samples))] = d
	s.n++
	s.max = max(s.max, d)
}
func (s *statistics) snapshot() Observation {
	s.mu.Lock()
	c := s.c
	c.BytesSent, c.BytesReceived = s.sent.Load(), s.received.Load()
	count := min(s.n, uint64(len(s.samples)))
	samples := append([]time.Duration(nil), s.samples[:count]...)
	maximum := s.max
	s.mu.Unlock()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	latency := Latency{RecentSamples: len(samples), MaxMillis: float64(maximum) / float64(time.Millisecond), All: s.rtt.Snapshot()}
	if len(samples) > 0 {
		latency.P50Millis = float64(samples[(len(samples)-1)*50/100]) / float64(time.Millisecond)
		latency.P95Millis = float64(samples[(len(samples)-1)*95/100]) / float64(time.Millisecond)
	}
	return Observation{Counters: c, RoundTrip: latency}
}
