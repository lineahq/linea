// Package runresult holds the options and the result of a Collector run:
// its inputs, status, outputs, evidence and mocks, and the comparison of two
// runs. It is pure data the engine prepares, the executor produces and every
// interface reads, so it links no Collector component or executor.
package runresult

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
	"go.opentelemetry.io/collector/component"
)

// Mode names how a Result was produced.
type Mode string

const (
	ModeInstrumented Mode = "instrumented"
	ModeReference    Mode = "reference"
)

// Status is how a run ended.
type Status string

const (
	// StatusCompleted: every input was injected and the Collector shut down
	// cleanly, flushing buffering processors, so the output is final.
	StatusCompleted Status = "completed"
	// StatusPartial: the run's deadline expired first, or its evidence
	// budget was reached; observations are incomplete and absence of output
	// proves nothing.
	StatusPartial Status = "partial"
	// StatusCancelled: the caller cancelled the run.
	StatusCancelled Status = "cancelled"
	// StatusFailed: the Collector reported an error while running or
	// shutting down.
	StatusFailed Status = "failed"
)

// Result is the transport-neutral outcome of a run, consumable by the CLI
// formatter and API clients alike.
type Result struct {
	Mode         Mode                 `json:"mode"`
	RunID        string               `json:"run_id,omitempty"`
	Status       Status               `json:"status"`
	StatusReason string               `json:"status_reason,omitempty"`
	EventSchema  string               `json:"event_schema,omitempty"`
	Inputs       []InputResult        `json:"inputs"`
	Events       []instrument.Event   `json:"events,omitempty"`
	Snapshots    instrument.Snapshots `json:"snapshots,omitempty"`
	Outputs      []Output             `json:"outputs"`
	// Mocks declares the dependencies substituted for the run.
	Mocks []mock.Declaration `json:"mocks,omitempty"`
	// Settle is the wait after the last input, when there was one.
	Settle *Settle `json:"settle,omitempty"`
	// Truncation says what the run did not keep because its evidence budget
	// was reached.
	Truncation *Truncation `json:"truncation,omitempty"`
	// ComponentMetrics are the metrics components that run for real
	// recorded through their own meter providers, read when the
	// Collector stopped (report schema 0.12).
	ComponentMetrics []instrument.ComponentMetric `json:"component_metrics,omitempty"`
	// EndpointOutputs are what the simulated endpoints of components
	// that run for real received (report schema 0.12).
	EndpointOutputs []EndpointOutput `json:"endpoint_outputs,omitempty"`
	// SimulatedEndpoints are the client endpoints the run pointed at
	// loopback sinks, with the calls each refused.
	SimulatedEndpoints []mock.SimulatedEndpoint `json:"simulated_endpoints,omitempty"`
}

// Completed reports whether the run's observations are final.
func (r *Result) Completed() bool { return r.Status == StatusCompleted }

// TruncationEvidenceBudget is the reason of a run that reached its evidence
// budget.
const TruncationEvidenceBudget = "evidence_budget"

// Truncation records what a run did not keep. Counts and batches of captured
// outputs still cover everything received; their data covers only what was
// kept.
type Truncation struct {
	Reason        string `json:"reason"`
	DroppedEvents int    `json:"dropped_events"`
	DroppedBytes  int64  `json:"dropped_bytes"`
	// IncompleteOutputs name the outputs ("exporter/signal") whose data
	// lacks telemetry that reached them.
	IncompleteOutputs []string `json:"incomplete_outputs,omitempty"`
	// Omitted names report sections that were not kept because the evidence
	// archive would have exceeded its storage limit.
	Omitted []string `json:"omitted,omitempty"`
}

// InputResult records where an input was submitted and whether the pipeline
// accepted it. A rejected input (for example an OTTL error with
// error_mode: propagate) is an observation, not a run failure.
type InputResult struct {
	Receiver string           `json:"receiver"`
	Signal   string           `json:"signal"`
	Counts   telemetry.Counts `json:"counts"`
	// Injected is false when the run ended before the input was submitted.
	Injected bool   `json:"injected"`
	Error    string `json:"error,omitempty"`
	// Mock names the provider when the receiver itself produced the input
	// from a mocked dependency (for example "filelog": the records read from
	// the scenario's files); Counts are then the total it emitted.
	Mock string `json:"mock,omitempty"`
	// ClientAddress is the sender address the input was injected with.
	ClientAddress string `json:"client_address,omitempty"`
	// After is the wait before the input was injected.
	After string `json:"after,omitempty"`
}

// Output is the final telemetry of one signal delivered to one exporter,
// concatenated in delivery order. An exporter shared by pipelines of
// different signals has one Output per signal.
type Output struct {
	Exporter string           `json:"exporter"`
	Signal   string           `json:"signal"`
	Batches  int              `json:"batches"`
	Counts   telemetry.Counts `json:"counts"`
	Data     json.RawMessage  `json:"data"`
}

// EndpointOutput is the telemetry of one signal a simulated endpoint
// received, concatenated in arrival order: what a component that
// runs for real sent to a client endpoint its configuration declares.
type EndpointOutput struct {
	// Component is the topology ref of the component that sent it.
	Component string `json:"component"`
	// Endpoint is where the client configuration sits in the
	// component's configuration ("delivery").
	Endpoint string           `json:"endpoint"`
	Signal   string           `json:"signal"`
	Batches  int              `json:"batches"`
	Counts   telemetry.Counts `json:"counts"`
	Data     json.RawMessage  `json:"data"`
}

// Settle is how long a run waited after its last input before shutting the
// Collector down, and why.
type Settle struct {
	Duration string `json:"duration"`
	// Reason is "execution.settle" when the scenario declared the wait, or
	// names the components the wait was derived for.
	Reason string `json:"reason"`
}

// ClockRead is an expression in a component's configuration that reads the
// wall clock, so its outcome depends on when the run happens relative to
// the input timestamps.
type ClockRead struct {
	Component  string `json:"component"`
	Expression string `json:"expression"`
}

// ErrReferencePrerequisite identifies an unsupported reference transport setting.
var ErrReferencePrerequisite = errors.New("unsupported reference transport prerequisite")

// ReferencePrerequisiteError identifies a setting, never its private value.
// The instrumented path substitutes these transports and does not need them.
type ReferencePrerequisiteError struct {
	Kind      topology.Kind
	Component component.ID
	Setting   string
	Reason    string
}

func (e *ReferencePrerequisiteError) Error() string {
	return fmt.Sprintf("%s: %s %s setting %s: %s; run without reference comparison to inspect the pipeline", ErrReferencePrerequisite, e.Kind, e.Component, e.Setting, e.Reason)
}

func (e *ReferencePrerequisiteError) Unwrap() error { return ErrReferencePrerequisite }
