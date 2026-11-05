package analysis

// Sampling explains the per-trace decisions of one sampling processor
// instance (tail_sampling in one pipeline). Decisions are observations: the
// processor's own decision hooks recorded them, with the spans evaluated.
// Policy results and their evidence come from replaying the processor's
// configuration on those spans; the replay is verified against the
// recorded decision per trace.
type Sampling struct {
	Processor string `json:"processor"`
	Pipeline  string `json:"pipeline"`
	// DecisionWait is the configured decision_wait.
	DecisionWait string           `json:"decision_wait"`
	Policies     []SamplingPolicy `json:"policies"`
	Traces       []SampledTrace   `json:"traces"`
	Notes        []string         `json:"notes,omitempty"`
}

// SamplingPolicy is one configured top-level policy.
type SamplingPolicy struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Rule says in words what the policy samples (or drops).
	Rule string `json:"rule"`
}

// Sampling decisions of a trace.
const (
	TraceSampled        = "sampled"
	TraceNotSampled     = "not_sampled"
	TraceDropped        = "dropped"
	TracePendingDropped = "pending_dropped"
	TracePending        = "pending"
)

// Replay verification of a trace's decision.
const (
	ReplayVerified = "replay_verified"
	ReplayDiffers  = "replay_differs"
	ReplayFailed   = "replay_failed"
)

// SampledTrace is the decision for one trace and why.
type SampledTrace struct {
	TraceID string `json:"trace_id"`
	// Name is the root span's name, or else the earliest span's.
	Name string `json:"name"`
	// Decision is sampled, not_sampled, dropped (a drop policy matched),
	// pending_dropped (no decision was made before the processor shut
	// down, with drop_pending_traces_on_shutdown) or pending (the run did
	// not complete before a decision).
	Decision string `json:"decision"`
	// Policy sampled or dropped the trace.
	Policy string `json:"policy,omitempty"`
	// Event is the stage.decision event; 0 for pending_dropped.
	Event int `json:"event,omitempty"`
	// Inputs are the stage inputs that carried spans of the trace.
	Inputs []int `json:"inputs"`
	// Spans were evaluated together; Duration is from the earliest start to
	// the latest end among them, as the latency policy measures it.
	Spans    int    `json:"spans"`
	Duration string `json:"duration"`
	// LateSpans arrived after the decision and followed it: Late says
	// whether they were forwarded or dropped.
	LateSpans int    `json:"late_spans,omitempty"`
	Late      string `json:"late,omitempty"`
	// Policies are each top-level policy's result on its own.
	Policies []PolicyResult `json:"policies"`
	// Verification compares the replayed decision with the recorded one.
	Verification string `json:"verification"`
	// Records are the trace's spans as record refs in their input
	// snapshots ("<seq>:resource[0].scope[0].span[1]"), so the client can
	// link them to the stage's changes.
	Records []string `json:"records,omitempty"`
}

// PolicyResult is what one policy decided for a trace, evaluated alone.
type PolicyResult struct {
	Policy string `json:"policy"`
	// Result is sampled, not_sampled, dropped (a drop policy matched) or
	// error.
	Result string `json:"result"`
	// Evidence is what the policy looked at in this trace, for example
	// "lasted 40ms" or "span status ERROR".
	Evidence string `json:"evidence,omitempty"`
}
