package evidence

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// The caps of a run summary.
const (
	maxChangeGroups = 20
	maxSummaryBytes = 256
	maxErrors       = 5
)

// IncompleteNote is fixed text: absence of output on an incomplete run is not
// evidence that a stage dropped anything.
const IncompleteNote = "Observations are incomplete: missing output proves no drop."

// Summary is the compact account of a finished run. It holds only
// what the run's report contains, grouped and capped; it is present once the
// run is terminal.
type Summary struct {
	// URI is the run's evidence resource. Adapters that serve evidence
	// resources (MCP) set the URI fields; they are empty otherwise.
	URI string `json:"uri,omitempty"`
	// ExecutionStatus is how the Collector run ended; absent without a report.
	ExecutionStatus runresult.Status `json:"execution_status,omitempty"`
	// Complete is true only when the execution completed, so its output is
	// final. Incomplete says why not: cancelled, interrupted or error (the run
	// has no report), or the execution status partial, cancelled or failed.
	Complete   bool   `json:"complete"`
	Incomplete string `json:"incomplete,omitempty"`
	// TruncatedReason is set when the run did not keep all of its evidence
	// (evidence_budget); counts still cover everything.
	TruncatedReason string `json:"truncated_reason,omitempty"`
	Note            string `json:"note,omitempty"`
	// StandIns counts the components that ran as stand-ins because their
	// type is not in the distribution, StandInReferences the
	// references that resolved to stand-in strings; the report's
	// topology and references name them.
	StandIns          int               `json:"stand_ins,omitempty"`
	StandInReferences int               `json:"stand_in_references,omitempty"`
	Assertions        *AssertionSummary `json:"assertions,omitempty"`
	Stages            []StageSummary    `json:"stages"`
	Outputs           []OutputSummary   `json:"outputs"`
	// EndpointOutputs are what simulated endpoints of custom components
	// received.
	EndpointOutputs []EndpointOutputSummary `json:"endpoint_outputs,omitempty"`
	// ComponentLogs counts the entries components that run for real
	// logged; absent when none did.
	ComponentLogs *LogCounts `json:"component_logs,omitempty"`
	// Errors are the first five run, execution, input, stage and
	// component (an entry a component logged at error level) errors.
	Errors          []SummaryError `json:"errors"`
	ErrorsTruncated bool           `json:"errors_truncated,omitempty"`
}

type AssertionSummary struct {
	Outcome      assertion.Outcome `json:"outcome"`
	Pass         int               `json:"pass"`
	Fail         int               `json:"fail"`
	Inconclusive int               `json:"inconclusive"`
	Error        int               `json:"error"`
}

// SummarizeAssertions counts the report's results by outcome; nil without a
// report.
func SummarizeAssertions(report *assertion.Report) *AssertionSummary {
	if report == nil {
		return nil
	}
	out := &AssertionSummary{Outcome: report.Outcome}
	for _, result := range report.Results {
		switch result.Outcome {
		case assertion.Pass:
			out.Pass++
		case assertion.Fail:
			out.Fail++
		case assertion.Inconclusive:
			out.Inconclusive++
		case assertion.Error:
			out.Error++
		}
	}
	return out
}

// StageSummary groups one stage's field-level changes by (path, op). A
// buffering stage (batch, tail_sampling) is compared through its
// reconciliation of all inputs with all emissions (Reconciled); other stages
// through their per-input diffs.
type StageSummary struct {
	Component  string                   `json:"component"`
	Pipeline   string                   `json:"pipeline"`
	Kind       string                   `json:"kind"`
	Type       string                   `json:"type"`
	Signal     string                   `json:"signal"`
	Inputs     int                      `json:"inputs"`
	Outcomes   map[analysis.Outcome]int `json:"outcomes"`
	Reconciled bool                     `json:"reconciled,omitempty"`
	// Removed and Added count records present on only one side.
	Removed int `json:"removed"`
	Added   int `json:"added"`
	// Unresolved counts relationships the diff could not establish.
	Unresolved       int           `json:"unresolved,omitempty"`
	Changes          []ChangeGroup `json:"changes"`
	ChangesTruncated bool          `json:"changes_truncated,omitempty"`
	// URI is the stage's changes resource and StatementsURI, for a transform
	// processor, its OTTL replay resource; set as Summary.URI is.
	URI           string `json:"uri,omitempty"`
	StatementsURI string `json:"statements_uri,omitempty"`
}

// ChangeGroup is one (path, op) of a stage: how many entities changed that
// way, the first change's values and the lowest match confidence among them.
type ChangeGroup struct {
	Path       string              `json:"path"`
	Op         analysis.Op         `json:"op"`
	Count      int                 `json:"count"`
	Before     *SummaryValue       `json:"before,omitempty"`
	After      *SummaryValue       `json:"after,omitempty"`
	Confidence analysis.Confidence `json:"confidence,omitempty"`
}

// SummaryValue is a recorded value with its pdata type. A string longer than
// 256 bytes keeps its first 256 bytes; any other value whose JSON encoding is
// longer becomes the first 256 bytes of that encoding, as a string.
type SummaryValue struct {
	Value     any    `json:"value"`
	Type      string `json:"type,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type OutputSummary struct {
	Exporter  string `json:"exporter"`
	Signal    string `json:"signal"`
	Batches   int    `json:"batches"`
	Resources int    `json:"resources"`
	Scopes    int    `json:"scopes"`
	Records   int    `json:"records"`
}

// EndpointOutputSummary counts what one simulated endpoint received.
type EndpointOutputSummary struct {
	Component string `json:"component"`
	Endpoint  string `json:"endpoint"`
	Signal    string `json:"signal"`
	Batches   int    `json:"batches"`
	Records   int    `json:"records"`
}

// LogCounts counts component log entries by level; Total includes info.
type LogCounts struct {
	Total    int `json:"total"`
	Warnings int `json:"warnings"`
	Errors   int `json:"errors"`
}

// SummaryError is an error recorded for the run: source is run, execution,
// input (Component is the receiver), stage or component.
type SummaryError struct {
	Source    string `json:"source"`
	Component string `json:"component,omitempty"`
	Pipeline  string `json:"pipeline,omitempty"`
	Message   string `json:"message"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Summarize summarizes a terminal run from its report; it returns nil while
// the run is queued or running.
func Summarize(run Run) *Summary {
	out := &Summary{Stages: []StageSummary{}, Outputs: []OutputSummary{}, Errors: []SummaryError{}}
	switch run.End {
	case EndCancelled:
		out.Incomplete = "cancelled"
	case EndInterrupted:
		out.Incomplete = "interrupted"
	case EndError:
		out.Incomplete = "error"
	case EndDone:
	default:
		return nil
	}
	if run.Error != "" {
		out.addError(SummaryError{Source: "run", Message: run.Error})
	}
	report := run.Report
	if report != nil && report.Result != nil {
		result := report.Result
		out.ExecutionStatus = result.Status
		if result.Completed() {
			out.Complete = true
		} else if out.Incomplete == "" {
			out.Incomplete = string(result.Status)
		}
		if result.StatusReason != "" {
			out.addError(SummaryError{Source: "execution", Message: result.StatusReason})
		}
		if result.Truncation != nil {
			out.TruncatedReason = result.Truncation.Reason
		}
		for _, input := range result.Inputs {
			if input.Error != "" {
				out.addError(SummaryError{Source: "input", Component: input.Receiver, Message: input.Error})
			}
		}
		for _, output := range result.Outputs {
			out.Outputs = append(out.Outputs, OutputSummary{Exporter: output.Exporter, Signal: output.Signal,
				Batches: output.Batches, Resources: output.Counts.Resources, Scopes: output.Counts.Scopes, Records: output.Counts.Items})
		}
		for _, o := range result.EndpointOutputs {
			out.EndpointOutputs = append(out.EndpointOutputs, EndpointOutputSummary{Component: o.Component, Endpoint: o.Endpoint,
				Signal: o.Signal, Batches: o.Batches, Records: o.Counts.Items})
		}
	} else if out.Incomplete == "" {
		out.Incomplete = "error"
	}
	if report != nil {
		if report.Topology != nil {
			for _, c := range report.Topology.Components {
				if c.Mode == topology.ModeStandIn {
					out.StandIns++
				}
			}
		}
		for _, r := range report.References {
			if r.Source == config.SourceStandIn {
				out.StandInReferences++
			}
		}
		out.Assertions = SummarizeAssertions(report.Assertions)
		for _, stage := range report.Stages {
			out.Stages = append(out.Stages, summarizeStage(stage))
			for _, input := range stage.Inputs {
				if input.Error != "" {
					out.addError(SummaryError{Source: "stage", Component: stage.Component, Pipeline: stage.Pipeline, Message: input.Error})
				}
			}
		}
	}
	if report != nil && report.Result != nil {
		// After stage errors: a stage's error matters more than what a
		// component logged about it.
		out.ComponentLogs = countLogs(report.Result.Events, out)
	}
	if !out.Complete {
		out.Note = IncompleteNote
	}
	return out
}

// countLogs counts component.log events by level and adds those at error
// level and above to the summary's errors.
func countLogs(events []instrument.Event, s *Summary) *LogCounts {
	var c LogCounts
	for _, e := range events {
		if e.Kind != instrument.KindComponentLog || e.Log == nil {
			continue
		}
		c.Total++
		switch e.Log.Level {
		case "warn":
			c.Warnings++
		case "error", "dpanic", "panic", "fatal":
			c.Errors++
			s.addError(SummaryError{Source: "component", Component: e.Component, Pipeline: e.Pipeline, Message: e.Log.Message})
		}
	}
	if c.Total == 0 {
		return nil
	}
	return &c
}

func (s *Summary) addError(e SummaryError) {
	if len(s.Errors) == maxErrors {
		s.ErrorsTruncated = true
		return
	}
	e.Message, e.Truncated = truncateString(e.Message)
	s.Errors = append(s.Errors, e)
}

func summarizeStage(stage analysis.Stage) StageSummary {
	out := StageSummary{Component: stage.Component, Pipeline: stage.Pipeline, Kind: stage.Kind, Type: stage.Type,
		Signal: stage.Signal, Inputs: len(stage.Inputs), Outcomes: map[analysis.Outcome]int{}, Changes: []ChangeGroup{}}
	var diffs []*analysis.Diff
	for _, input := range stage.Inputs {
		out.Outcomes[input.Outcome]++
		if input.Diff != nil {
			diffs = append(diffs, input.Diff)
		}
	}
	// The reconciliation already covers every input and emission; adding the
	// per-input diffs would count the same records twice.
	if stage.Reconciliation != nil {
		out.Reconciled = true
		diffs = []*analysis.Diff{stage.Reconciliation}
	}
	type key struct {
		path string
		op   analysis.Op
	}
	index := map[key]int{}
	for _, diff := range diffs {
		out.Removed += len(diff.Removed)
		out.Added += len(diff.Added)
		out.Unresolved += len(diff.Unresolved)
		for _, change := range diff.Changes {
			k := key{change.Path, change.Op}
			i, ok := index[k]
			if !ok {
				if len(out.Changes) == maxChangeGroups {
					out.ChangesTruncated = true
					continue
				}
				i = len(out.Changes)
				index[k] = i
				out.Changes = append(out.Changes, ChangeGroup{Path: change.Path, Op: change.Op,
					Before: summarize(change.Before, change.BeforeType), After: summarize(change.After, change.AfterType)})
			}
			group := &out.Changes[i]
			group.Count++
			if confidenceRank(change.Confidence) < confidenceRank(group.Confidence) {
				group.Confidence = change.Confidence
			}
		}
	}
	return out
}

// confidenceRank orders match confidence from weakest to strongest; spans,
// resources and scopes carry none and a record followed by object is the
// same record, so neither lowers a group's confidence.
func confidenceRank(c analysis.Confidence) int {
	switch c {
	case analysis.ConfidenceMedium:
		return 1
	case analysis.ConfidenceHigh:
		return 2
	case analysis.ConfidenceExact:
		return 3
	}
	return 4
}

// SummarizeValue caps a recorded value as the run summary does (256 bytes,
// with Truncated set); nil for an absent value.
func SummarizeValue(value any, typ string) *SummaryValue { return summarize(value, typ) }

func summarize(value any, typ string) *SummaryValue {
	if value == nil {
		return nil
	}
	if s, ok := value.(string); ok {
		cut, truncated := truncateString(s)
		return &SummaryValue{Value: cut, Type: typ, Truncated: truncated}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return &SummaryValue{Value: fmt.Sprintf("unencodable %T", value), Type: typ, Truncated: true}
	}
	if len(encoded) <= maxSummaryBytes {
		return &SummaryValue{Value: value, Type: typ}
	}
	cut, _ := truncateString(string(encoded))
	return &SummaryValue{Value: cut, Type: typ, Truncated: true}
}

// truncateString keeps at most 256 bytes without splitting a UTF-8 sequence.
func truncateString(s string) (string, bool) {
	if len(s) <= maxSummaryBytes {
		return s, false
	}
	cut := maxSummaryBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
