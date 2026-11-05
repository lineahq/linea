// Package comparison compares two terminal runs from their recorded reports.
// Like internal/debugger/evidence it is a pure projection: it
// reads the two reports and their recorded snapshots and never executes,
// replays or reads anything else.
package comparison

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/evidence"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// Schema versions the comparison document.
const Schema = "0.1"

// Caps, as the run summary's: change groups per exporter and values;
// and listed records and their field changes.
const (
	maxGroups         = 20
	maxRecords        = 100
	maxRecordChanges  = 20
	connectorStageKey = "connector"
)

// ErrInvalidRule: an ignore rule is not an OTTL path this comparison can
// match.
var ErrInvalidRule = errors.New("invalid ignore rule")

// Outcome is the comparison's verdict.
type Outcome string

const (
	// Same: every exporter received the same records (in content, not merely
	// in number), and nothing else compared differs.
	Same Outcome = "same"
	// Different: at least one compared fact differs.
	Different Outcome = "different"
	// Inconclusive: the evidence cannot show the runs same or different; the
	// reasons say why. It is a result, not an error.
	Inconclusive Outcome = "inconclusive"
)

// Reason codes of an inconclusive comparison.
const (
	ReasonBeforeIncomplete = "before_incomplete"
	ReasonAfterIncomplete  = "after_incomplete"
	ReasonBeforeTruncated  = "before_truncated"
	ReasonAfterTruncated   = "after_truncated"
	ReasonBuildsDiffer     = "builds_differ"
	// ReasonInputsDiffer: the runs' captured inputs are not proven
	// identical, so no difference can be attributed to the configurations.
	ReasonInputsDiffer = "inputs_differ"
)

// Side is one run to compare, with what its captured bundle says about it.
type Side struct {
	Run  evidence.Run
	Mode string
	// InputsDigest identifies the run's captured inputs; empty when unknown.
	InputsDigest string
	// InputIdentity identifies what the run injected, as the self-contained
	// scenario its captured inputs derive to without its name and assertions
	// (scenario.InputIdentity); empty when they cannot be derived.
	InputIdentity string
	// Build names the debugger build that admitted the run; empty when
	// unknown.
	Build string
}

// Request compares Before with After. Ignore lists OTTL paths whose
// differences are counted but do not count as differences.
type Request struct {
	Before, After Side
	Ignore        []string
}

// Comparison is what two runs' evidence shows about their differences.
type Comparison struct {
	Schema  string  `json:"comparison_schema"`
	Outcome Outcome `json:"outcome"`
	// Reasons say why the outcome is inconclusive.
	Reasons []Reason `json:"reasons,omitempty"`
	Before  RunInfo  `json:"before"`
	After   RunInfo  `json:"after"`
	// InputsIdentical is true only when both runs' captured inputs have the
	// same digest, or derive to one self-contained scenario; only then are injected records compared one by one and
	// can the outcome be other than inconclusive.
	InputsIdentical bool       `json:"inputs_identical"`
	Exporters       []Exporter `json:"exporters"`
	// Records follows each injected record through both runs; absent when
	// the inputs are not proven identical.
	Records    *Records     `json:"records,omitempty"`
	Routes     []Route      `json:"routes"`
	Errors     []ErrorCount `json:"errors"`
	Assertions []Assertion  `json:"assertions"`
	// Ignored counts the record differences each ignore rule matched.
	Ignored []Ignored `json:"ignored"`
	Notes   []string  `json:"notes,omitempty"`
}

type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RunInfo is how one compared run ended.
type RunInfo struct {
	ID              string            `json:"id"`
	Mode            string            `json:"mode,omitempty"`
	End             evidence.End      `json:"end"`
	ExecutionStatus runresult.Status  `json:"execution_status,omitempty"`
	Complete        bool              `json:"complete"`
	Incomplete      string            `json:"incomplete,omitempty"`
	TruncatedReason string            `json:"truncated_reason,omitempty"`
	InputsDigest    string            `json:"inputs_digest,omitempty"`
	Build           string            `json:"build,omitempty"`
	Assertions      assertion.Outcome `json:"assertions,omitempty"`
	// Runtime is the runtime the run executed on. Runs on different
	// runtimes compare like any others: the runtime alone is never a reason
	// to be inconclusive.
	Runtime *engine.ReportRuntime `json:"runtime,omitempty"`
}

// Exporter compares what one exporter received for one signal. Equal means
// the same multiset of records, each with its resource and scope; records
// are counted, but equal counts alone are never equal.
type Exporter struct {
	Exporter string `json:"exporter"`
	Signal   string `json:"signal"`
	Before   int    `json:"before"`
	After    int    `json:"after"`
	// OnlyIn names the run that has output at this exporter when the other
	// has none.
	OnlyIn string `json:"only_in,omitempty"`
	Equal  bool   `json:"equal"`
	// EqualAfterIgnore is set when the records differ only in ignored paths.
	EqualAfterIgnore bool   `json:"equal_after_ignore,omitempty"`
	Note             string `json:"note,omitempty"`
	// Changes group the field differences of the records both runs delivered
	// here by (path, op): from the per-record comparison when it follows
	// records here, otherwise from pairing the differing records by shared
	// content, with medium confidence (Note says so).
	Changes          []evidence.ChangeGroup `json:"changes"`
	ChangesTruncated bool                   `json:"changes_truncated,omitempty"`
}

// Records compares the injected records of runs with identical inputs.
type Records struct {
	// Aligned records exist in both runs: the same input and position.
	Aligned int `json:"aligned"`
	// Changed records end differently or reach an exporter changed.
	Changed int `json:"changed"`
	// Unaligned records were injected in one run only.
	Unaligned int `json:"unaligned"`
	// Uncertain records could not be followed with confidence in at least one
	// run; they are neither changed nor unchanged.
	Uncertain int `json:"uncertain"`
	// Items are the changed records, at most 100.
	Items     []Record `json:"items"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Record is one changed injected record.
type Record struct {
	// Input is the record's input, from 1; Ref its position in the input.
	Input  int    `json:"input"`
	Ref    string `json:"ref"`
	Signal string `json:"signal"`
	Name   string `json:"name,omitempty"`
	Before Fate   `json:"before"`
	After  Fate   `json:"after"`
	// FateChanged is set when the record ends differently.
	FateChanged bool `json:"fate_changed"`
	// DivergesAt is the first step of the before run's path where the runs
	// differ ("filter/pager (logs)"): a component one run did not reach, or
	// one that changed a different number of its fields or ended it
	// differently. It is empty when the paths match and only the delivered
	// fields differ.
	DivergesAt string `json:"diverges_at,omitempty"`
	// Changes are the field differences at exporters both runs delivered it
	// to, at most 20.
	Changes          []FieldChange `json:"changes,omitempty"`
	ChangesTruncated bool          `json:"changes_truncated,omitempty"`
	// Confidence is "medium" when either run matched the record across a
	// stage by content with medium confidence.
	Confidence analysis.Confidence `json:"confidence,omitempty"`
}

// Fate is where a record ended in one run: the exporters it reached
// ("otlp/backend (logs)", by pipeline) and where its other paths stopped
// ("dropped at filter/health (logs)"). LineageID names the record's lineage in
// that run's report, so a client can follow it there, and Path lists
// the components it passed through.
type Fate struct {
	Delivered []string `json:"delivered"`
	Stopped   []string `json:"stopped"`
	LineageID string   `json:"lineage_id"`
	Path      []Step   `json:"path"`
}

// Step is one component on a record's path through one run, in the order of
// its hops; a fan-out continues with one branch before the next.
type Step struct {
	Component string `json:"component"`
	Pipeline  string `json:"pipeline,omitempty"`
	// Changes counts what the stage changed on the record.
	Changes int `json:"changes,omitempty"`
	// End is how a path ended here, such as delivered or dropped.
	End analysis.End `json:"end,omitempty"`
}

// FieldChange is one field of a record that differs at an exporter.
type FieldChange struct {
	// Exporter names the exporter and the pipeline that delivered to it.
	Exporter string `json:"exporter"`
	exporter string
	Path     string                 `json:"path"`
	Op       analysis.Op            `json:"op"`
	Before   *evidence.SummaryValue `json:"before,omitempty"`
	After    *evidence.SummaryValue `json:"after,omitempty"`
}

// Route counts the records a connector delivered into a pipeline.
type Route struct {
	Connector string `json:"connector"`
	Pipeline  string `json:"pipeline"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
	Changed   bool   `json:"changed"`
}

// ErrorCount is an error recorded a different number of times in each run.
type ErrorCount struct {
	Source    string `json:"source"`
	Component string `json:"component,omitempty"`
	Pipeline  string `json:"pipeline,omitempty"`
	Message   string `json:"message"`
	Truncated bool   `json:"truncated,omitempty"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
}

// Assertion is one assertion's outcome in each run; empty where the run has
// no assertion of that name.
type Assertion struct {
	Name    string            `json:"name"`
	Before  assertion.Outcome `json:"before,omitempty"`
	After   assertion.Outcome `json:"after,omitempty"`
	Changed bool              `json:"changed"`
}

// Ignored is how many record differences one ignore rule matched.
type Ignored struct {
	Rule  string `json:"rule"`
	Count int    `json:"count"`
}

// Compare compares two terminal runs. A run that is not terminal is the
// caller's to refuse; here it only makes the comparison inconclusive.
func Compare(req Request) (*Comparison, error) {
	rules, err := parseRules(req.Ignore)
	if err != nil {
		return nil, err
	}
	out := &Comparison{Schema: Schema, Exporters: []Exporter{}, Routes: []Route{}, Errors: []ErrorCount{},
		Assertions: []Assertion{}, Ignored: []Ignored{}}
	for _, r := range rules {
		out.Ignored = append(out.Ignored, Ignored{Rule: r.text})
	}
	out.Before = info(req.Before)
	out.After = info(req.After)
	out.InputsIdentical = req.Before.InputsDigest != "" && req.Before.InputsDigest == req.After.InputsDigest
	if !out.InputsIdentical && req.Before.InputIdentity != "" && req.Before.InputIdentity == req.After.InputIdentity {
		out.InputsIdentical = true
		out.Notes = append(out.Notes, "the runs' captured inputs differ as written but inline to one scenario, so they are compared as identical inputs")
	}
	if !out.Before.Complete {
		out.reason(ReasonBeforeIncomplete, "the before run is incomplete ("+out.Before.Incomplete+"): missing output proves no difference")
	}
	if !out.After.Complete {
		out.reason(ReasonAfterIncomplete, "the after run is incomplete ("+out.After.Incomplete+"): missing output proves no difference")
	}
	if out.Before.TruncatedReason != "" {
		out.reason(ReasonBeforeTruncated, "the before run did not keep all of its evidence ("+out.Before.TruncatedReason+")")
	}
	if out.After.TruncatedReason != "" {
		out.reason(ReasonAfterTruncated, "the after run did not keep all of its evidence ("+out.After.TruncatedReason+")")
	}
	if !out.InputsIdentical {
		out.reason(ReasonInputsDiffer, "the runs' captured inputs are not proven identical, so they cannot be compared; the exporter outputs are listed for information only")
	}
	if b, a := req.Before.Build, req.After.Build; b != "" && a != "" && b != a {
		out.reason(ReasonBuildsDiffer, "the runs were admitted by different debugger builds, so a difference may come from the build")
	}
	before, after := newSide(req.Before), newSide(req.After)
	if before.report != nil && after.report != nil {
		if mocked := before.mocked || after.mocked; mocked {
			out.Notes = append(out.Notes, "mocked components: attribute order and collector.instance.id are not compared")
		}
		out.compareExporters(before, after, rules)
		if out.InputsIdentical {
			out.compareRecords(before, after, rules)
		}
		out.compareRoutes(before, after)
	}
	out.compareErrors(req.Before.Run, req.After.Run)
	out.compareAssertions(req.Before.Run.Report, req.After.Run.Report)
	for i, r := range rules {
		out.Ignored[i].Count = r.count
	}
	out.decide()
	return out, nil
}

func info(s Side) RunInfo {
	out := RunInfo{ID: s.Run.ID, Mode: s.Mode, End: s.Run.End, InputsDigest: s.InputsDigest, Build: s.Build}
	if s.Run.Report != nil {
		out.Runtime = s.Run.Report.Runtime
	}
	if summary := evidence.Summarize(s.Run); summary != nil {
		out.ExecutionStatus = summary.ExecutionStatus
		out.Complete = summary.Complete
		out.Incomplete = summary.Incomplete
		out.TruncatedReason = summary.TruncatedReason
		if summary.Assertions != nil {
			out.Assertions = summary.Assertions.Outcome
		}
	} else {
		out.Incomplete = "not finished"
	}
	return out
}

func (c *Comparison) reason(code, message string) {
	c.Reasons = append(c.Reasons, Reason{Code: code, Message: message})
}

func (c *Comparison) decide() {
	if len(c.Reasons) > 0 {
		c.Outcome = Inconclusive
		return
	}
	different := len(c.Errors) > 0
	for _, e := range c.Exporters {
		different = different || !(e.Equal || e.EqualAfterIgnore)
	}
	if c.Records != nil {
		different = different || c.Records.Changed > 0
	}
	for _, r := range c.Routes {
		different = different || r.Changed
	}
	for _, a := range c.Assertions {
		different = different || a.Changed
	}
	if different {
		c.Outcome = Different
	} else {
		c.Outcome = Same
	}
}

// compareRoutes counts what each connector delivered into each pipeline.
func (c *Comparison) compareRoutes(before, after *side) {
	type key struct{ connector, pipeline string }
	counts := func(s *side) map[key]int {
		connectors := map[string]bool{}
		for _, st := range s.report.Stages {
			if st.Kind == connectorStageKey {
				connectors[st.Component] = true
			}
		}
		out := map[key]int{}
		for _, e := range s.report.Result.Events {
			if e.Kind == instrument.KindEgress && connectors[e.Component] && e.Counts != nil {
				out[key{e.Component, e.Pipeline}] += e.Counts.Items
			}
		}
		return out
	}
	b, a := counts(before), counts(after)
	keys := map[key]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	for k := range keys {
		c.Routes = append(c.Routes, Route{Connector: k.connector, Pipeline: k.pipeline, Before: b[k], After: a[k], Changed: b[k] != a[k]})
	}
	slices.SortFunc(c.Routes, func(x, y Route) int {
		return strings.Compare(x.Connector+"\x00"+x.Pipeline, y.Connector+"\x00"+y.Pipeline)
	})
}

// compareErrors lists errors recorded a different number of times: run,
// input and stage errors. Execution status differences make a run
// incomplete instead.
func (c *Comparison) compareErrors(before, after evidence.Run) {
	type key struct{ source, component, pipeline, message string }
	collect := func(run evidence.Run) map[key]int {
		out := map[key]int{}
		if run.Error != "" {
			out[key{"run", "", "", run.Error}]++
		}
		if run.Report == nil {
			return out
		}
		if run.Report.Result != nil {
			for _, in := range run.Report.Result.Inputs {
				if in.Error != "" {
					out[key{"input", in.Receiver, "", in.Error}]++
				}
			}
		}
		for _, st := range run.Report.Stages {
			for _, in := range st.Inputs {
				if in.Error != "" {
					out[key{"stage", st.Component, st.Pipeline, in.Error}]++
				}
			}
		}
		return out
	}
	b, a := collect(before), collect(after)
	keys := map[key]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	for k := range keys {
		if b[k] == a[k] {
			continue
		}
		e := ErrorCount{Source: k.source, Component: k.component, Pipeline: k.pipeline, Before: b[k], After: a[k]}
		if v := evidence.SummarizeValue(k.message, ""); v != nil {
			e.Message, e.Truncated = v.Value.(string), v.Truncated
		}
		c.Errors = append(c.Errors, e)
	}
	slices.SortFunc(c.Errors, func(x, y ErrorCount) int {
		return strings.Compare(fmt.Sprint(x.Source, x.Component, x.Pipeline, x.Message), fmt.Sprint(y.Source, y.Component, y.Pipeline, y.Message))
	})
}

// compareAssertions pairs assertions by name, in the before run's order.
// Only an assertion both runs carry can change.
func (c *Comparison) compareAssertions(before, after *engineReport) {
	results := func(r *engineReport) []assertion.Result {
		if r == nil || r.Assertions == nil {
			return nil
		}
		return r.Assertions.Results
	}
	b, a := results(before), results(after)
	index := map[string]int{}
	for _, r := range b {
		index[r.Name] = len(c.Assertions)
		c.Assertions = append(c.Assertions, Assertion{Name: r.Name, Before: r.Outcome})
	}
	for _, r := range a {
		if i, ok := index[r.Name]; ok {
			c.Assertions[i].After = r.Outcome
			c.Assertions[i].Changed = c.Assertions[i].Before != r.Outcome
			continue
		}
		c.Assertions = append(c.Assertions, Assertion{Name: r.Name, After: r.Outcome})
	}
}
