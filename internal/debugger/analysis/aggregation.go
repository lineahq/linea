package analysis

import "strings"

// AggregationVerification states whether an aggregation's attribution of
// input records to emitted data points is confirmed by the observed output.
type AggregationVerification string

const (
	// AggregationVerified: replaying the connector's configuration on the
	// recorded input produced exactly the data points it emitted, so each
	// point's records are the ones it counted.
	AggregationVerified AggregationVerification = "replay_verified"
	// AggregationDiffers: the replay does not reproduce the emitted data
	// points; the points are listed without records.
	AggregationDiffers AggregationVerification = "replay_differs"
	// AggregationUnsupported: the configuration uses something the replay
	// does not model; the points are listed without records.
	AggregationUnsupported AggregationVerification = "unsupported"
)

// Aggregation explains one input of an aggregating connector, such as
// count, which turns the records it consumes into data points of another
// signal. It is computed outside the run from the recorded
// input and the connector's configuration, and is authoritative only when
// Verification is replay_verified.
type Aggregation struct {
	Connector string `json:"connector"`
	// Input is the connector's stage.input.
	Input int `json:"input"`
	// Output is the delivery whose payload Points refer to (a connector
	// feeding several pipelines delivers the same payload to each); 0 when
	// the connector delivered nothing.
	Output       int                     `json:"output,omitempty"`
	Verification AggregationVerification `json:"verification"`
	Points       []AggregatedPoint       `json:"points"`
	// Uncounted are positions of input records that no metric counted.
	Uncounted []string `json:"uncounted,omitempty"`
	Notes     []string `json:"notes,omitempty"`
}

// AggregatedPoint is one emitted data point and the input records it
// aggregates.
type AggregatedPoint struct {
	// Ref is the point's position in the output payload.
	Ref        string            `json:"ref"`
	Metric     string            `json:"metric"`
	Attributes map[string]string `json:"attributes,omitempty"`
	Value      int64             `json:"value"`
	// Records are positions in the input payload: records, or for counts
	// of span events and metrics the event or metric
	// ("…span[0].event[1]", "…metric[2]").
	Records []string `json:"records,omitempty"`
}

// Contains reports whether an aggregated position (AggregatedPoint.Records)
// covers a record position or lies within it: a span covers its events, a
// metric its data points.
func Contains(aggregated, record string) bool {
	return aggregated == record || strings.HasPrefix(aggregated, record+".") || strings.HasPrefix(record, aggregated+".")
}
