package livecaptureextension

import (
	"time"
)

// Signal identifies a Collector signal. Metrics capture units are datapoints.
type Signal string

const (
	Traces  Signal = "traces"
	Metrics Signal = "metrics"
	Logs    Signal = "logs"
)

// Point identifies a configured pipeline/point pair, never a remote destination.
type Point struct {
	Pipeline string `json:"pipeline"`
	Point    string `json:"point"`
}

type Target struct {
	CollectorGroup string `json:"collector_group"`
	Pod            string `json:"pod,omitempty"`
}

type Filter struct {
	Resource []string `json:"resource,omitempty"`
	Traces   struct {
		Span []string `json:"span,omitempty"`
	} `json:"traces,omitempty"`
	Logs struct {
		Log []string `json:"log,omitempty"`
	} `json:"logs,omitempty"`
	Metrics struct {
		Metric    []string `json:"metric,omitempty"`
		Datapoint []string `json:"datapoint,omitempty"`
	} `json:"metrics,omitempty"`
}

type Sampling struct {
	Rate float64 `json:"rate"`
	Mode string  `json:"mode,omitempty"` // hash (default), or random for logs/metrics.
}

type SessionLimits struct {
	BytesPerSecond   int64 `json:"bytes_per_second,omitempty"`
	RecordsPerSecond int64 `json:"records_per_second,omitempty"`
	TotalBytes       int64 `json:"total_bytes,omitempty"`
	TotalRecords     int64 `json:"total_records,omitempty"`
	RecordBytes      int64 `json:"record_bytes,omitempty"`
}

// Request is ephemeral desired state. Destination and redaction are Git-owned.
type Request struct {
	SessionID     string        `json:"session_id"`
	Target        Target        `json:"target"`
	CapturePoints []Point       `json:"capture_points"`
	Signals       []Signal      `json:"signals"`
	Filter        Filter        `json:"filter"`
	Sampling      *Sampling     `json:"sampling,omitempty"`
	Limits        SessionLimits `json:"limits,omitempty"`
	ExpiresAt     time.Time     `json:"expires_at,omitempty"`
	Principal     string        `json:"principal"`
}

// Access is supplied by the authenticated control adapter, never by a Request.
type Access struct{ AllowRaw bool }

type Stats struct {
	Selected        int64            `json:"selected"`
	EnqueuedRecords int64            `json:"enqueued_records"`
	EnqueuedBytes   int64            `json:"enqueued_bytes"`
	FilterErrors    int64            `json:"filter_errors"`
	Dropped         map[string]int64 `json:"dropped"`
}

type Status struct {
	SessionID string    `json:"session_id"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	Stats     Stats     `json:"stats"`
}

type CapturePoint struct {
	Point
	Signal      Signal `json:"signal"`
	Sensitivity string `json:"sensitivity"`
}
