package fleetcapture

import "time"

// These types mirror the linea-fleet livecapture plugin API (schema 2.3.0) and
// the fleet core group and agent reads. testdata holds copies of both
// OpenAPI documents; TestTypesMatchContracts keeps the mirrors honest.

// CapturePoint is a capture point a peer reports in its catalog.
type CapturePoint struct {
	Pipeline    string `json:"pipeline"`
	Point       string `json:"point"`
	Signal      string `json:"signal"`
	Sensitivity string `json:"sensitivity"`
}

// Identity is what a Collector reports about itself. Fleet authenticates the
// credential, not these fields.
type Identity struct {
	Cluster             string         `json:"cluster,omitempty"`
	Namespace           string         `json:"namespace,omitempty"`
	Pod                 string         `json:"pod"`
	CollectorGroup      string         `json:"collector_group"`
	CollectorVersion    string         `json:"collector_version,omitempty"`
	DistributionVersion string         `json:"distribution_version,omitempty"`
	Enabled             bool           `json:"enabled"`
	CapturePoints       []CapturePoint `json:"capture_points"`
	// FilterLimits is the largest filter the Collector compiles. A plugin
	// before schema 2.3.0 omits it; the version 1 limits then apply.
	FilterLimits *FilterLimits `json:"filter_limits,omitempty"`
}

// FilterLimits bound a capture filter on one Collector. Lengths count
// Unicode code points.
type FilterLimits struct {
	// MaxConditions counts every list of the filter together.
	MaxConditions     int `json:"max_conditions"`
	MaxConditionChars int `json:"max_condition_chars"`
	// MaxRegexChars bounds an IsMatch pattern; the Collector checks it.
	MaxRegexChars int `json:"max_regex_chars"`
}

// Peer is a connected Collector the plugin can capture from. Its ID is
// stable across reconnects; the generation advances with each one.
type Peer struct {
	AuthorityID    string   `json:"authority_id"`
	PluginInstance string   `json:"plugin_instance"`
	ID             string   `json:"id"`
	Generation     uint64   `json:"generation"`
	CredentialID   string   `json:"credential_id"`
	AgentID        string   `json:"agent_id,omitempty"`
	Identity       Identity `json:"identity"`
}

// PeerPage is a page of connected peers.
type PeerPage struct {
	Items      []Peer `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Channel is an owner-bound delivery queue for capture batches.
type Channel struct {
	AuthorityID      string    `json:"authority_id"`
	PluginInstance   string    `json:"plugin_instance"`
	ID               string    `json:"id"`
	ExpiresAt        time.Time `json:"expires_at"`
	HeartbeatSeconds int       `json:"heartbeat_seconds"`
}

type channelRequest struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// Envelope is one delivered batch: an OTLP export request of one signal from
// one peer, for one capture.
type Envelope struct {
	AuthorityID    string    `json:"authority_id"`
	PluginInstance string    `json:"plugin_instance"`
	PluginEpoch    string    `json:"plugin_epoch"`
	BatchID        string    `json:"batch_id"`
	CaptureID      string    `json:"capture_id"`
	PeerID         string    `json:"peer_id"`
	Generation     uint64    `json:"generation"`
	Signal         string    `json:"signal"`
	Records        int64     `json:"records"`
	Encoding       string    `json:"encoding"`
	Data           []byte    `json:"data"`
	ObservedAt     time.Time `json:"observed_at"`
}

// Ack reports how many records of a batch the debugger accepted.
type Ack struct {
	BatchID  string `json:"batch_id"`
	Accepted int64  `json:"accepted_records"`
}

// Target names a peer at the generation the caller selected.
type Target struct {
	PeerID     string `json:"peer_id"`
	Generation uint64 `json:"generation"`
}

// Point selects a capture point of a pipeline.
type Point struct {
	Pipeline string `json:"pipeline"`
	Point    string `json:"point"`
}

// Filter holds OTTL conditions the Collector applies before sending.
type Filter struct {
	Resource []string      `json:"resource,omitempty"`
	Traces   *TraceFilter  `json:"traces,omitempty"`
	Logs     *LogFilter    `json:"logs,omitempty"`
	Metrics  *MetricFilter `json:"metrics,omitempty"`
}

// TraceFilter holds span conditions.
type TraceFilter struct {
	Span []string `json:"span,omitempty"`
}

// LogFilter holds log record conditions.
type LogFilter struct {
	Log []string `json:"log,omitempty"`
}

// MetricFilter holds metric and data point conditions.
type MetricFilter struct {
	Metric    []string `json:"metric,omitempty"`
	Datapoint []string `json:"datapoint,omitempty"`
}

// Sampling samples selected records at a rate.
type Sampling struct {
	Rate float64 `json:"rate"`
	Mode string  `json:"mode,omitempty"`
}

// Limits bounds what each target sends; zero keeps the Collector default.
type Limits struct {
	BytesPerSecond   int64 `json:"bytes_per_second,omitempty"`
	RecordsPerSecond int64 `json:"records_per_second,omitempty"`
	TotalBytes       int64 `json:"total_bytes,omitempty"`
	TotalRecords     int64 `json:"total_records,omitempty"`
	RecordBytes      int64 `json:"record_bytes,omitempty"`
}

// CaptureRequest admits a capture. Exactly one of Targets, Agents, Selector
// or GroupID selects the peers. Retries reuse the ID with identical content.
type CaptureRequest struct {
	AuthorityID    string    `json:"authority_id"`
	PluginInstance string    `json:"plugin_instance"`
	ID             string    `json:"id"`
	ChannelID      string    `json:"channel_id"`
	Targets        []Target  `json:"targets,omitempty"`
	Agents         []string  `json:"agents,omitempty"`
	Selector       string    `json:"selector,omitempty"`
	GroupID        string    `json:"group_id,omitempty"`
	CapturePoints  []Point   `json:"capture_points"`
	Signals        []string  `json:"signals"`
	Filter         Filter    `json:"filter"`
	Sampling       *Sampling `json:"sampling,omitempty"`
	Limits         Limits    `json:"limits"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// TargetStats are a target Collector's last reported counters.
type TargetStats struct {
	Selected        int64            `json:"selected"`
	EnqueuedRecords int64            `json:"enqueued_records"`
	EnqueuedBytes   int64            `json:"enqueued_bytes"`
	FilterErrors    int64            `json:"filter_errors"`
	Dropped         map[string]int64 `json:"dropped,omitempty"`
}

// TargetOutcome is a capture's state on one peer.
type TargetOutcome struct {
	PeerID     string       `json:"peer_id"`
	Generation uint64       `json:"generation"`
	AgentID    string       `json:"agent_id,omitempty"`
	State      string       `json:"state"`
	Stats      *TargetStats `json:"stats,omitempty"`
}

// Operation is an admitted capture and its target outcomes.
type Operation struct {
	AuthorityID string          `json:"authority_id"`
	ID          string          `json:"id"`
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"`
	Revision    int64           `json:"revision"`
	ExpiresAt   time.Time       `json:"expires_at"`
	Targets     []TargetOutcome `json:"targets"`
	ActingFor   *Acting         `json:"acting_for,omitempty"`
}

// Terminal reports whether the operation has ended.
func (o *Operation) Terminal() bool {
	switch o.State {
	case "completed", "stopped", "failed", "expired", "interrupted":
		return true
	}
	return false
}

// Group is a fleet collector group, of listed agents or a selector.
type Group struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Revision    int64    `json:"revision"`
	MemberCount *int     `json:"member_count,omitempty"`
	AgentIDs    []string `json:"agent_ids,omitempty"`
	Selector    string   `json:"selector,omitempty"`
}

// GroupPage is a page of fleet groups.
type GroupPage struct {
	Items     []Group `json:"items"`
	NextAfter string  `json:"next_after,omitempty"`
}

// Agent is the part of a fleet agent the debugger shows.
type Agent struct {
	ID           string            `json:"id"`
	Connected    bool              `json:"connected"`
	CredentialID string            `json:"credential_id,omitempty"`
	Attributes   map[string]string `json:"attributes"`
}

// AgentPage is a page of fleet agents.
type AgentPage struct {
	Items     []Agent `json:"items"`
	NextAfter string  `json:"next_after,omitempty"`
}
