// Package ottldebug holds the report of a transform processor's OTTL
// statement replay: per statement group and statement, what matched, what
// changed and whether the replay agrees with the processor's output.
//
// The replay itself is package ottlreplay, an optional feature compiled into
// a runtime only when its manifest has the transform processor; this
// package imports no Collector component.
package ottldebug

import "github.com/lineahq/linea/internal/debugger/analysis"

// Status of a statement group in the replay.
type Status string

const (
	StatusReplayed     Status = "replayed"
	StatusUnsupported  Status = "unsupported"
	StatusNotEvaluated Status = "not_evaluated"
)

// Verification states how far the replay explains the observed output.
type Verification string

const (
	// VerificationReplayVerified: the replayed result equals the processor's
	// actual output (or both rejected the input).
	VerificationReplayVerified Verification = "replay_verified"
	// VerificationDisagrees: the replay produced something else; statement
	// results must not be read as attribution.
	VerificationDisagrees Verification = "disagrees"
	// VerificationNotVerifiable: part of the configuration was not replayed
	// or no authoritative output exists to compare with.
	VerificationNotVerifiable Verification = "not_verifiable"
)

// Report is the replay of one transform processor over one stage input.
type Report struct {
	Processor string `json:"processor"`
	// Input and Output are the seqs of the stage.input replayed and of the
	// correlated stage.output it is checked against (0 when none).
	Input        int          `json:"input"`
	Output       int          `json:"output,omitempty"`
	Signal       string       `json:"signal"`
	Groups       []Group      `json:"groups"`
	Verification Verification `json:"verification"`
	// Error is the error that stopped processing under error_mode propagate.
	Error string `json:"error,omitempty"`
	// Mismatch is the diff from actual output to replay output when the
	// replay disagrees.
	Mismatch *analysis.Diff `json:"mismatch,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
}

// Group is one configured statement group, in configuration order.
type Group struct {
	Index      int         `json:"index"`
	Context    string      `json:"context"`
	ErrorMode  string      `json:"error_mode"`
	Conditions []string    `json:"conditions,omitempty"`
	Status     Status      `json:"status"`
	Reason     string      `json:"reason,omitempty"`
	Statements []Statement `json:"statements"`
}

// Statement aggregates one statement's executions over all entities of its
// context. Matched counts where clauses that evaluated true (statements
// without a where clause always match), NotMatched those that evaluated
// false, NotEvaluated entities where an earlier error stopped processing.
type Statement struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	// Effective is the statement as parsed, when the parser collection
	// rewrote it (for example by prefixing path context names).
	Effective    string            `json:"effective,omitempty"`
	Matched      int               `json:"matched"`
	NotMatched   int               `json:"not_matched"`
	NotEvaluated int               `json:"not_evaluated,omitempty"`
	Errors       []ExecutionError  `json:"errors,omitempty"`
	Changes      []analysis.Change `json:"changes,omitempty"`
	Unchanged    int               `json:"matched_without_change,omitempty"`
}

// ExecutionError is a statement error on one entity. Phase is "condition"
// when the where clause failed and "function" when the editor failed.
type ExecutionError struct {
	Entity  string `json:"entity"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
}
