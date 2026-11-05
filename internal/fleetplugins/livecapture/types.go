// Package livecapture implements the optional server plugin, independent of core.
package livecapture

import (
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

var ErrInvalid = errors.New("invalid capture request")
var ErrConflict = errors.New("capture state conflicts")

// ErrPeerBusy refuses a named target that serves another capture.
var ErrPeerBusy = fmt.Errorf("collector is serving another capture: %w", ErrConflict)
var ErrForbidden = errors.New("capture access denied")
var ErrNotFound = errors.New("capture resource not found")
var ErrUnavailable = errors.New("capture service unavailable")
var ErrCapacity = errors.New("capture capacity reached")
var ErrPrecondition = errors.New("capture revision changed")
var ErrCursorExpired = errors.New("capture peer cursor expired; refresh discovery")

type Point struct {
	Pipeline string `json:"pipeline"`
	Point    string `json:"point"`
}
type CatalogPoint struct {
	Pipeline    string `json:"pipeline"`
	Point       string `json:"point"`
	Signal      string `json:"signal"`
	Sensitivity string `json:"sensitivity"`
}
type Identity struct {
	Cluster             string         `json:"cluster"`
	Namespace           string         `json:"namespace"`
	Pod                 string         `json:"pod"`
	Group               string         `json:"collector_group"`
	CollectorVersion    string         `json:"collector_version"`
	DistributionVersion string         `json:"distribution_version"`
	Enabled             bool           `json:"enabled"`
	Points              []CatalogPoint `json:"capture_points"`
	FilterLimits        FilterLimits   `json:"filter_limits"`
}

// FilterLimits is the largest filter a Collector compiles, as it reports in
// the control protocol. Lengths count Unicode code points.
type FilterLimits struct {
	MaxConditions     int `json:"max_conditions"`
	MaxConditionChars int `json:"max_condition_chars"`
	MaxRegexChars     int `json:"max_regex_chars"`
}

// v1FilterLimits apply to a Collector that reports none, or a zero field.
var v1FilterLimits = FilterLimits{MaxConditions: 16, MaxConditionChars: 256, MaxRegexChars: 256}

// orV1 fills each unset limit with the version 1 limit.
func (l FilterLimits) orV1() FilterLimits {
	for _, f := range []struct{ to, v1 *int }{{&l.MaxConditions, &v1FilterLimits.MaxConditions}, {&l.MaxConditionChars, &v1FilterLimits.MaxConditionChars}, {&l.MaxRegexChars, &v1FilterLimits.MaxRegexChars}} {
		if *f.to <= 0 {
			*f.to = *f.v1
		}
	}
	return l
}

type PeerInfo struct {
	AuthorityID    string `json:"authority_id"`
	PluginInstance string `json:"plugin_instance"`
	ID             string `json:"id"`
	Generation     uint64 `json:"generation"`
	// CredentialID is the fleet agent credential the Collector presented.
	CredentialID string `json:"credential_id"`
	// AgentID is the uniquely matching connected fleet agent, when linked.
	AgentID  string   `json:"agent_id,omitempty"`
	Identity Identity `json:"identity"`
}
type TargetRequest struct {
	PeerID     string `json:"peer_id"`
	Generation uint64 `json:"generation"`
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
	Mode string  `json:"mode,omitempty"`
}
type Limits struct {
	BytesPerSecond   int64 `json:"bytes_per_second,omitempty"`
	RecordsPerSecond int64 `json:"records_per_second,omitempty"`
	TotalBytes       int64 `json:"total_bytes,omitempty"`
	TotalRecords     int64 `json:"total_records,omitempty"`
	RecordBytes      int64 `json:"record_bytes,omitempty"`
}
type CreateRequest struct {
	AuthorityID    string `json:"authority_id"`
	PluginInstance string `json:"plugin_instance"`
	ID             string `json:"id"`
	ChannelID      string `json:"channel_id"`
	// Exactly one of targets (peer generations), agents (fleet agent IDs,
	// each with one linked peer), selector or group_id selects the peers.
	// Selector and group targets admit only linked peers able to capture.
	Targets   []TargetRequest `json:"targets,omitempty"`
	Agents    []string        `json:"agents,omitempty"`
	Selector  string          `json:"selector,omitempty"`
	GroupID   string          `json:"group_id,omitempty"`
	Points    []Point         `json:"capture_points"`
	Signals   []string        `json:"signals"`
	Filter    Filter          `json:"filter"`
	Sampling  *Sampling       `json:"sampling,omitempty"`
	Limits    Limits          `json:"limits,omitempty"`
	ExpiresAt time.Time       `json:"expires_at"`
}
type Target struct {
	PeerID     string       `json:"peer_id"`
	Generation uint64       `json:"generation"`
	AgentID    string       `json:"agent_id,omitempty"`
	WireID     string       `json:"-"`
	State      string       `json:"state"`
	Released   bool         `json:"-"`
	Stats      *TargetStats `json:"stats,omitempty"`
}

// TargetStats are the Collector's last reported counters for its session.
// Reports replace the value; it is never mutated in place.
type TargetStats struct {
	Selected        int64            `json:"selected"`
	EnqueuedRecords int64            `json:"enqueued_records"`
	EnqueuedBytes   int64            `json:"enqueued_bytes"`
	FilterErrors    int64            `json:"filter_errors"`
	Dropped         map[string]int64 `json:"dropped,omitempty"`
}

// StoredTarget is private durable metadata; wire IDs are never in public reads.
type StoredTarget struct {
	PeerID     string       `json:"peer_id"`
	Generation uint64       `json:"generation"`
	AgentID    string       `json:"agent_id,omitempty"`
	WireID     string       `json:"wire_id"`
	State      string       `json:"state"`
	Released   bool         `json:"released"`
	Stats      *TargetStats `json:"stats,omitempty"`
}
type Operation struct {
	AuthorityID string    `json:"authority_id"`
	ID          string    `json:"id"`
	Owner       string    `json:"-"`
	Hash        []byte    `json:"-"`
	ChannelID   string    `json:"-"`
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	Revision    int64     `json:"revision"`
	ExpiresAt   time.Time `json:"expires_at"`
	Targets     []Target  `json:"targets"`
	// ActingFor is the user the owning credential acted for when it
	// created the operation. Attribution only; Owner stays the authority.
	ActingFor *pluginsdk.Acting `json:"acting_for,omitempty"`
}
type Envelope struct {
	AuthorityID    string    `json:"authority_id"`
	PluginInstance string    `json:"plugin_instance"`
	PluginEpoch    string    `json:"plugin_epoch"`
	ID             string    `json:"batch_id"`
	CaptureID      string    `json:"capture_id"`
	PeerID         string    `json:"peer_id"`
	Generation     uint64    `json:"generation"`
	Signal         string    `json:"signal"`
	Records        int64     `json:"records"`
	Encoding       string    `json:"encoding"`
	Data           []byte    `json:"data"`
	ObservedAt     time.Time `json:"observed_at"`
}
type Acknowledgement struct {
	ID       string `json:"batch_id"`
	Accepted int64  `json:"accepted_records"`
}

func terminal(state string) bool {
	switch state {
	case "completed", "expired", "limit_reached", "failed", "disabled":
		return true
	}
	return false
}
func storedTargets(targets []Target) []StoredTarget {
	result := make([]StoredTarget, len(targets))
	for i, t := range targets {
		result[i] = StoredTarget(t)
	}
	return result
}
