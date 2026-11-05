package analysis

// RoutingVerification states whether an explanation of a routing connector's
// decisions is confirmed by what the connector actually delivered.
type RoutingVerification string

const (
	// RoutingVerified: replaying the connector on the recorded input
	// delivered exactly the records each pipeline received, so each
	// record's rules are the ones that routed it.
	RoutingVerified RoutingVerification = "replay_verified"
	// RoutingDiffers: the replay does not reproduce the recorded
	// deliveries; no record is attributed to a rule.
	RoutingDiffers RoutingVerification = "replay_differs"
	// RoutingUnsupported: a rule reads something the run does not record
	// (client metadata), so the connector cannot be replayed.
	RoutingUnsupported RoutingVerification = "unsupported"
)

// Routing explains one input of a routing connector: which routing table
// rule matched each record, and which records went to the default
// pipelines. It is computed outside the run by replaying the real connector
// on the recorded input, and is authoritative only when Verification is
// replay_verified.
type Routing struct {
	Connector string `json:"connector"`
	// Input is the connector's stage.input.
	Input        int                 `json:"input"`
	Verification RoutingVerification `json:"verification"`
	// Rules is the routing table in evaluation order.
	Rules            []RoutingRule `json:"rules"`
	DefaultPipelines []string      `json:"default_pipelines,omitempty"`
	// Records lists every input record with the rules that routed it; only
	// present when the replay is verified.
	Records []RoutedRecord `json:"records,omitempty"`
	Notes   []string       `json:"notes,omitempty"`
}

// RoutingRule is one entry of the routing table.
type RoutingRule struct {
	Index int `json:"index"`
	// Context is the configured OTTL context; empty when the connector
	// infers it from the condition.
	Context string `json:"context,omitempty"`
	// Condition is the configured condition or statement.
	Condition string `json:"condition"`
	// Action is "move" (the record leaves) or "copy" (the record also
	// continues to the next rules and the default).
	Action    string   `json:"action"`
	Pipelines []string `json:"pipelines"`
}

// RoutedRecord is one record of the input and where the table sent it.
type RoutedRecord struct {
	// Ref is the record's position in the input payload, for example
	// "resource[0].scope[0].log_record[2]".
	Ref string `json:"ref"`
	// Rules are the indexes of the rules that matched it, in table order.
	Rules []int `json:"rules,omitempty"`
	// Default is true when the record went to the default pipelines.
	Default bool `json:"default,omitempty"`
}
