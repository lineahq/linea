// Package provenance defines bounded input lineage values. Metadata never grants
// transport permissions or changes replay behavior.
package provenance

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// CaptureStream names the records of one Collector, pipeline, capture
// point and signal; empty fields match any.
type CaptureStream struct {
	CollectorID string `json:"collector_id,omitempty" yaml:"collector_id,omitempty"`
	Pipeline    string `json:"pipeline,omitempty" yaml:"pipeline,omitempty"`
	Point       string `json:"point,omitempty" yaml:"point,omitempty"`
	Signal      string `json:"signal,omitempty" yaml:"signal,omitempty"`
}

// CaptureInputSelection picks records of a stream up to a watermark, or
// only the listed sequences, as the input of a receiver.
type CaptureInputSelection struct {
	Stream    CaptureStream `json:"stream" yaml:"stream"`
	Watermark int           `json:"watermark" yaml:"watermark"`
	Sequences []int         `json:"sequences,omitempty" yaml:"sequences,omitempty"`
	Receiver  string        `json:"receiver" yaml:"receiver"`
	// RetainTags has no effect: records are stored without the Collector's
	// capture tags. It stays so saved selections still decode.
	RetainTags bool `json:"retain_tags,omitempty" yaml:"retain_tags,omitempty"`
}

// CaptureDerivation records where a derived input came from and the
// digests it was derived for.
type CaptureDerivation struct {
	SessionID               string                `json:"session_id" yaml:"session_id"`
	Source                  *CaptureSource        `json:"source,omitempty" yaml:"source,omitempty"`
	SourceSensitivity       string                `json:"source_sensitivity,omitempty" yaml:"source_sensitivity,omitempty"`
	Selection               CaptureInputSelection `json:"selection" yaml:"selection"`
	SourceDigest            string                `json:"source_digest,omitempty" yaml:"source_digest,omitempty"`
	Digest                  string                `json:"digest" yaml:"digest"`
	DestinationConfigDigest string                `json:"destination_config_digest" yaml:"destination_config_digest"`
	DestinationSetupDigest  string                `json:"destination_setup_digest,omitempty" yaml:"destination_setup_digest,omitempty"`
	Records                 int                   `json:"records" yaml:"records"`
}

// CaptureSource is the admission-time identity snapshot. It contains no
// certificate fingerprints, credentials, configuration or redaction-policy hash.
type CaptureSource struct {
	TransportSecurity   string `json:"transport_security,omitempty" yaml:"transport_security,omitempty"`
	Authentication      string `json:"authentication,omitempty" yaml:"authentication,omitempty"`
	Cluster             string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	Namespace           string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Pod                 string `json:"pod" yaml:"pod"`
	CollectorGroup      string `json:"collector_group" yaml:"collector_group"`
	CollectorVersion    string `json:"collector_version" yaml:"collector_version"`
	DistributionVersion string `json:"distribution_version,omitempty" yaml:"distribution_version,omitempty"`
}

// ValidateCaptures bounds private document metadata. A declared origin is not
// an authentication claim or proof of production coverage/redaction.
func ValidateCaptures(origins []CaptureDerivation) error {
	if len(origins) == 0 {
		return nil
	}
	invalid := errors.New("capture_origins: invalid or oversized capture provenance")
	if len(origins) > 64 {
		return invalid
	}
	text := func(v string, limit int) bool {
		return strings.TrimSpace(v) != "" && len(v) <= limit && !strings.ContainsFunc(v, unicode.IsControl)
	}
	digest := func(v string) bool {
		if len(v) != 64 {
			return false
		}
		b, err := hex.DecodeString(v)
		return err == nil && len(b) == 32 && v == strings.ToLower(v)
	}
	for _, origin := range origins {
		s := origin.Selection
		if !strings.HasPrefix(origin.SessionID, "capture-") || !text(origin.SessionID, 128) ||
			!text(s.Stream.CollectorID, 128) ||
			!text(s.Stream.Pipeline, 256) ||
			!text(s.Stream.Point, 128) ||
			!text(s.Receiver, 256) ||
			s.Watermark < 1 ||
			s.Watermark > 10000 ||
			len(s.Sequences) < 1 ||
			len(s.Sequences) > 1000 ||
			origin.Records != len(s.Sequences) ||
			origin.SourceDigest != "" && !digest(origin.SourceDigest) ||
			!digest(origin.Digest) ||
			!digest(origin.DestinationConfigDigest) ||
			origin.DestinationSetupDigest != "" && !digest(origin.DestinationSetupDigest) {
			return invalid
		}
		if origin.SourceSensitivity != "" && origin.SourceSensitivity != "raw" &&
			origin.SourceSensitivity != "redacted" {
			return invalid
		}
		if source := origin.Source; source != nil {
			if source.Authentication != "" && source.Authentication != "mtls" && source.Authentication != "api-key" &&
				source.Authentication != "none" {
				return invalid
			}
			if source.TransportSecurity != "" && source.TransportSecurity != "mtls" &&
				source.TransportSecurity != "tls" &&
				source.TransportSecurity != "plaintext" {
				return invalid
			}
			optional := func(v string, limit int) bool { return len(v) <= limit && !strings.ContainsFunc(v, unicode.IsControl) }
			if !optional(source.Cluster, 256) || !optional(source.Namespace, 256) || !text(source.Pod, 256) ||
				!text(source.CollectorGroup, 256) ||
				!text(source.CollectorVersion, 128) ||
				!optional(source.DistributionVersion, 128) {
				return invalid
			}
		}
		switch s.Stream.Signal {
		case "traces", "logs", "metrics":
		default:
			return invalid
		}
		seen := make(map[int]bool, len(s.Sequences))
		for _, sequence := range s.Sequences {
			if sequence < 1 || sequence > s.Watermark || seen[sequence] {
				return invalid
			}
			seen[sequence] = true
		}
	}
	raw, err := json.Marshal(origins)
	if err != nil || len(raw) > 128<<10 {
		return invalid
	}
	return nil
}
