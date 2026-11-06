// Package engine is the transport-neutral verification kernel underneath the
// platform: one Request produces one Report, encoded identically for CLI and
// HTTP consumers.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"

	// The optional features this runtime's manifest brings.
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// ReportSchemaVersion versions the Report JSON shape.
const ReportSchemaVersion = "0.15"

// Request selects a configuration and its inputs. The configuration is a
// path (Config) or text (ConfigYAML); the inputs are exactly one of a fixture
// path, fixture text, a scenario path or scenario text. Paths are read, never
// written.
type Request struct {
	Config     string `json:"config,omitempty"`
	ConfigYAML string `json:"config_yaml,omitempty"`
	Fixture    string `json:"fixture,omitempty"`
	// FixtureJSON is an OTLP JSON fixture as text: a quick run of sample
	// input without a scenario.
	FixtureJSON string `json:"fixture_json,omitempty"`
	Scenario    string `json:"scenario,omitempty"`
	// ScenarioYAML is scenario text. Relative input files resolve against
	// ScenarioDir; without it, inputs must be inline (data) or absolute.
	ScenarioYAML string `json:"scenario_yaml,omitempty"`
	ScenarioDir  string `json:"scenario_dir,omitempty"`
	// Receiver selects the injection receiver for Fixture or FixtureJSON.
	// Scenarios name receivers per input.
	Receiver string `json:"receiver,omitempty"`
	// ClientAddress is the sender IP address of a fixture, as a scenario
	// input's client_address.
	ClientAddress string `json:"client_address,omitempty"`
	// SetupYAML gives a fixture run mocks, environment values and execution
	// limits: scenario YAML without inputs or assertions (see
	// scenario.ParseSetup).
	SetupYAML string `json:"setup_yaml,omitempty"`
	// AssertionsYAML gives a fixture run assertions in scenario v1 syntax;
	// only assertions and an optional version are accepted.
	AssertionsYAML string `json:"assertions_yaml,omitempty"`
	// Compare also runs an uninstrumented reference Collector.
	Compare bool `json:"compare,omitempty"`
	// Normalize removes run-specific IDs and times from the report (see
	// Report.Normalize).
	Normalize bool `json:"normalize,omitempty"`
	// ConfigLabel, FixtureLabel and ScenarioLabel name inline sources in
	// provenance, as a path names a file. They are never read.
	ConfigLabel   string `json:"config_label,omitempty"`
	FixtureLabel  string `json:"fixture_label,omitempty"`
	ScenarioLabel string `json:"scenario_label,omitempty"`
	// Sources names the saved revisions inline sources were copied from. The
	// bundle records them; the engine never resolves them.
	Sources *SourceRefs `json:"-"`

	Logger *zap.Logger `json:"-"`
	// Log, if set, keeps the tail of the run's Collector log with every
	// environment value redacted.
	Log *LogTail `json:"-"`
	// RunID, if set, is the run ID to use; callers that track a run before
	// it starts (the platform) choose it.
	RunID string `json:"-"`
	// Observer, if set, receives every event of the instrumented run as it
	// is recorded. It must return quickly.
	Observer func(instrument.Event) `json:"-"`
	// ConfigSnapshot is an internal prepared provider source. It is never client
	// JSON; Prepare captures one when absent, before platform admission.
	ConfigSnapshot *config.Snapshot `json:"-"`
	// Limits is the composition's run policy, set by the platform at
	// admission and recorded in the execution bundle; it is never client
	// JSON. Zero fields are unlimited.
	Limits Limits `json:"-"`
	// Runtime is the runtime the run executes on, set by the platform at
	// admission and recorded in the execution bundle (schema 6); nil is the
	// built-in runtime. Like RunID it is not part of the prepared sources.
	Runtime  *RuntimeTarget `json:"-"`
	prepared *preparation
}

// SourceRef names a saved revision a source was copied from.
type SourceRef struct {
	ArtifactID string `json:"artifact_id"`
	RevisionID string `json:"revision_id"`
}

// SourceRefs are the saved revisions of a run's root sources, when they were
// submitted by reference.
type SourceRefs struct {
	Config   *SourceRef `json:"config,omitempty"`
	Fixture  *SourceRef `json:"fixture,omitempty"`
	Scenario *SourceRef `json:"scenario,omitempty"`
}

func (r *SourceRefs) valid() bool {
	if r == nil {
		return true
	}
	if r.Config == nil && r.Fixture == nil && r.Scenario == nil {
		return false
	}
	for _, ref := range []*SourceRef{r.Config, r.Fixture, r.Scenario} {
		if ref != nil && (ref.ArtifactID == "" || ref.RevisionID == "") {
			return false
		}
	}
	return true
}

func (r *SourceRefs) identity() [3]string {
	var out [3]string
	if r == nil {
		return out
	}
	for i, ref := range []*SourceRef{r.Config, r.Fixture, r.Scenario} {
		if ref != nil {
			out[i] = ref.ArtifactID + "/" + ref.RevisionID
		}
	}
	return out
}

// Limits bound one run. Execution is the deadline: a declared
// execution.timeout lowers it and never raises it. EvidenceBytes and Events
// bound what the run keeps (see instrument.Budget).
type Limits struct {
	Execution     time.Duration
	EvidenceBytes int64
	Events        int
}

// ReportLimits are the effective limits of a run, as reports and bundle
// metadata record them.
type ReportLimits struct {
	// Execution is the effective deadline as a Go duration; empty is none.
	Execution     string `json:"execution,omitempty"`
	EvidenceBytes int64  `json:"evidence_bytes,omitempty"`
	Events        int    `json:"events,omitempty"`
	// Clamped lists declared values the policy lowered.
	Clamped []Clamp `json:"clamped,omitempty"`
}

// Clamp records a declared value above the policy and the value used.
type Clamp struct {
	Limit     string `json:"limit"`
	Requested string `json:"requested"`
	Effective string `json:"effective"`
}

func (l Limits) record() *ReportLimits {
	r := &ReportLimits{EvidenceBytes: l.EvidenceBytes, Events: l.Events}
	if l.Execution > 0 {
		r.Execution = l.Execution.String()
	}
	return r
}

// effective applies the policy to a declared execution timeout: the lower
// of the two, with a clamp recorded when the declared value was higher.
func (l Limits) effective(declared time.Duration) (time.Duration, *ReportLimits) {
	if l == (Limits{}) {
		return declared, nil
	}
	deadline := declared
	r := l.record()
	if l.Execution > 0 && (deadline <= 0 || deadline > l.Execution) {
		if deadline > l.Execution {
			r.Clamped = append(r.Clamped, Clamp{Limit: "execution", Requested: deadline.String(), Effective: l.Execution.String()})
		}
		deadline = l.Execution
	}
	r.Execution = ""
	if deadline > 0 {
		r.Execution = deadline.String()
	}
	return deadline, r
}

func (r Request) config() config.Source { return config.Source{Path: r.Config, YAML: r.ConfigYAML} }

// Report is the complete outcome of a Request.
type Report struct {
	Schema     string      `json:"report_schema"`
	Scenario   string      `json:"scenario,omitempty"`
	Provenance *Provenance `json:"provenance"`
	// Runtime is the runtime the run executed on: the built-in one or a
	// built runtime's container (report schema 0.9).
	Runtime  *ReportRuntime  `json:"runtime,omitempty"`
	Topology *topology.Graph `json:"topology"`
	// References are the configuration's ${scheme:...} references and
	// where each value came from: the scenario, the host, a URI default or
	// a stand-in for an unavailable value (report schema 0.8).
	References []config.Reference `json:"references,omitempty"`
	// Limits are the run's effective limits, recorded whenever the request
	// carried a policy (platform runs always do).
	Limits *ReportLimits     `json:"limits,omitempty"`
	Result *runresult.Result `json:"result"`
	Stages []analysis.Stage  `json:"stages"`
	// Lineage follows each injected record to the exporters it reached or
	// the stage where it stopped, and each data point an aggregating
	// connector derived from them.
	Lineage []analysis.RecordLineage `json:"lineage"`
	// Aggregations explain the inputs of aggregating connectors (count):
	// which input records each emitted data point counts.
	Aggregations []*analysis.Aggregation `json:"aggregations"`
	// Routing explains the inputs of routing connectors: which rule of the
	// routing table sent each record where.
	Routing []*analysis.Routing `json:"routing"`
	// Sampling explains the per-trace decisions of tail sampling
	// processors: the policies and what each one found in the trace.
	Sampling []*analysis.Sampling `json:"sampling"`
	// Batching explains when and why each batch processor sent what it
	// held: its arrivals, its flushes and their triggers (report schema
	// 0.15).
	Batching []*analysis.Batching `json:"batching,omitempty"`
	// Grouping explains how each groupbyattrs processor regrouped every
	// request into resources and where each record went (report schema
	// 0.15).
	Grouping []*analysis.Grouping `json:"grouping,omitempty"`
	// Filtering explains every filter processor input: which condition
	// removed each item, and what each condition was evaluated on, matched
	// and failed on (report schema 0.15).
	Filtering []*analysis.Filtering `json:"filtering,omitempty"`
	OTTL      []*ottldebug.Report   `json:"ottl"`
	// Timestamps relates the input timestamps to the run's wall clock and
	// says whether they were rebased.
	Timestamps *Timestamps           `json:"timestamps,omitempty"`
	Reference  *runresult.Result     `json:"reference,omitempty"`
	Comparison *runresult.Comparison `json:"comparison,omitempty"`
	// Assertions is present when the request declares assertions.
	Assertions *assertion.Report `json:"assertions,omitempty"`
}

// ErrInvalidRequest marks errors caused by the request itself rather than by
// the configuration or the run.
var ErrInvalidRequest = errors.New("invalid request")

// Execute runs the request. Errors wrapping ErrInvalidRequest describe a
// malformed request; other errors come from configuration or execution.
func Execute(ctx context.Context, req Request) (*Report, error) {
	var err error
	req, err = Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.Log != nil {
		var secrets []string
		if req.ConfigSnapshot != nil {
			secrets = req.ConfigSnapshot.EnvironmentValues()
		}
		req.Logger = withRunLog(req.Logger, req.Log, secrets)
	}
	data, err := loadInputs(req, req.prepared.read)
	if err != nil {
		return nil, err
	}
	rep := &Report{Schema: ReportSchemaVersion, Scenario: data.name, Runtime: BuiltInRuntime()}
	opts, assertions, rebase := data.opts, data.assertions, data.rebase
	timeout := ""
	if data.timeout > 0 {
		timeout = data.timeout.String()
	}
	deadline, limits := req.Limits.effective(data.timeout)
	rep.Limits = limits
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}
	if limits != nil {
		opts.Budget = instrument.NewBudget(req.Limits.EvidenceBytes, req.Limits.Events)
	}
	c, factories, err := inProcessFactories()
	if err != nil {
		return nil, err
	}
	resolved, err := req.ConfigSnapshot.Resolve(ctx, factories)
	if err != nil {
		return nil, err
	}
	req.ConfigSnapshot, opts.ConfigSnapshot = resolved.Snapshot, resolved.Snapshot
	if rep.Provenance, err = provenance(req, opts.Env); err != nil {
		return nil, err
	}
	rep.Provenance.Settings.Timeout = timeout
	g := graph(c, resolved, factories)
	rep.Topology = &g
	rep.References = resolved.References
	rep.Timestamps = timestamps(opts.Inputs, rebase, c.WallClockReads(resolved.Config), time.Now())

	if rep.Result, err = c.Run(ctx, opts); err != nil {
		return nil, err
	}
	if req.Normalize {
		rep.Result.Canonicalize()
	}
	// Missing output is final only when the run completed: a clean Collector
	// shutdown flushes buffering processors.
	if rep.Stages, err = analysis.Analyze(rep.Result.Events, rep.Result.Snapshots, rep.Result.Completed()); err != nil {
		return nil, err
	}
	if rep.Aggregations, err = explainAggregations(ctx, resolved, rep.Stages, rep.Result); err != nil {
		return nil, err
	}
	if rep.Routing, err = explainRouting(ctx, resolved, rep.Stages, rep.Result); err != nil {
		return nil, err
	}
	if rep.Lineage, err = analysis.Lineage(rep.Result.Events, rep.Result.Snapshots, rep.Stages, rep.Aggregations, rep.Result.Completed()); err != nil {
		return nil, err
	}
	if rep.Sampling, err = explainSampling(ctx, resolved, rep.Stages, rep.Result); err != nil {
		return nil, err
	}
	if rep.Batching, err = explainBatching(resolved, rep.Stages, rep.Lineage, rep.Result, opts.Inputs); err != nil {
		return nil, err
	}
	if rep.Grouping, err = explainGrouping(resolved, rep.Stages, rep.Lineage, rep.Result); err != nil {
		return nil, err
	}
	if rep.Filtering, err = explainFiltering(ctx, resolved, rep.Stages, rep.Lineage, rep.Result); err != nil {
		return nil, err
	}
	if rep.OTTL, err = replayTransforms(ctx, resolved, rep.Stages, rep.Result); err != nil {
		return nil, err
	}
	if len(assertions) > 0 {
		if rep.Assertions, err = assertion.Evaluate(assertions, rep.Topology, rep.Result); err != nil {
			return nil, err
		}
	}
	if req.Compare {
		if rep.Reference, err = c.RunReference(ctx, opts); err != nil {
			return nil, fmt.Errorf("reference run: %w", err)
		}
		if req.Normalize {
			rep.Reference.Canonicalize()
		}
		cmp := runresult.Compare(rep.Result, rep.Reference)
		rep.Comparison = &cmp
	}
	if req.Normalize {
		rep.Normalize()
	}
	return rep, nil
}

// Validate checks that the request names its sources unambiguously. The
// error wraps ErrInvalidRequest.
func (req Request) Validate() error {
	if (req.Config == "") == (req.ConfigYAML == "") {
		return fmt.Errorf("%w: exactly one of config or config_yaml is required", ErrInvalidRequest)
	}
	if nonEmpty(req.Fixture, req.FixtureJSON, req.Scenario, req.ScenarioYAML) != 1 {
		return fmt.Errorf("%w: exactly one of fixture, fixture_json, scenario or scenario_yaml is required", ErrInvalidRequest)
	}
	if req.ScenarioDir != "" && req.ScenarioYAML == "" {
		return fmt.Errorf("%w: scenario_dir applies to scenario_yaml only", ErrInvalidRequest)
	}
	for _, l := range []struct{ label, text, name string }{
		{req.ConfigLabel, req.ConfigYAML, "config"}, {req.FixtureLabel, req.FixtureJSON, "fixture"}, {req.ScenarioLabel, req.ScenarioYAML, "scenario"},
	} {
		if l.label != "" && l.text == "" {
			return fmt.Errorf("%w: a %s label applies to inline %s text only", ErrInvalidRequest, l.name, l.name)
		}
	}
	if s := req.Sources; !s.valid() || s != nil && (s.Config != nil && req.ConfigYAML == "" ||
		s.Fixture != nil && req.FixtureJSON == "" || s.Scenario != nil && req.ScenarioYAML == "") {
		return fmt.Errorf("%w: source references describe inline sources only", ErrInvalidRequest)
	}
	if !req.fixture() && req.Receiver != "" {
		return fmt.Errorf("%w: receiver applies to fixtures only; scenarios name receivers per input", ErrInvalidRequest)
	}
	if !req.fixture() && (req.ClientAddress != "" || req.SetupYAML != "") {
		return fmt.Errorf("%w: client_address and setup_yaml apply to fixtures only; a scenario declares its own", ErrInvalidRequest)
	}
	if !req.fixture() && req.AssertionsYAML != "" {
		return fmt.Errorf("%w: assertions_yaml applies to fixtures only; a scenario declares its own", ErrInvalidRequest)
	}
	if a := req.ClientAddress; a != "" && !scenario.ValidClientAddress(a) {
		return fmt.Errorf("%w: client_address %q is not an IP address (optionally with a port)", ErrInvalidRequest, a)
	}
	return nil
}

func (req Request) fixture() bool { return req.Fixture != "" || req.FixtureJSON != "" }

func nonEmpty(values ...string) int {
	n := 0
	for _, v := range values {
		if v != "" {
			n++
		}
	}
	return n
}

// loadScenario loads a scenario file, or parses scenario text whose relative
// input files resolve against dir.
func loadScenario(path, text, dir string) (*scenario.Scenario, error) {
	if path != "" {
		return scenario.Load(path)
	}
	s, err := scenario.Parse([]byte(text), dir)
	if err != nil {
		return nil, fmt.Errorf("%w: scenario_yaml: %v", ErrInvalidRequest, err)
	}
	return s, nil
}

func input(receiver string, p telemetry.Payload) (runresult.Input, error) {
	in := runresult.Input{Payload: p}
	if receiver != "" {
		var id component.ID
		if err := id.UnmarshalText([]byte(receiver)); err != nil {
			return in, fmt.Errorf("%w: receiver %q: %v", ErrInvalidRequest, receiver, err)
		}
		in.Receiver = &id
	}
	return in, nil
}

// replayTransforms replays every transform stage input whose outcome is
// known: one correlated output, or a completed run with no output.
func replayTransforms(ctx context.Context, resolved *config.Resolved, stages []analysis.Stage, result *runresult.Result) ([]*ottldebug.Report, error) {
	reports := []*ottldebug.Report{}
	replay := features.Replay()
	if replay == nil {
		return reports, nil // The runtime lacks the feature; its report says so.
	}
	bySeq := map[int]instrument.Event{}
	for _, e := range result.Events {
		bySeq[e.Seq] = e
	}
	for _, s := range stages {
		if s.Type != "transform" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		cfg := resolved.Config.Processors[id]
		for _, in := range s.Inputs {
			if in.Outcome != analysis.OutcomeEmitted && in.Outcome != analysis.OutcomeNoOutput {
				continue
			}
			if len(in.Outputs) > 1 {
				continue
			}
			payload, err := result.Snapshots.Payload(bySeq[in.Seq].Snapshot)
			if err != nil {
				return nil, err
			}
			var out *telemetry.Payload
			outSeq := 0
			if len(in.Outputs) == 1 {
				outSeq = in.Outputs[0]
				p, err := result.Snapshots.Payload(bySeq[outSeq].Snapshot)
				if err != nil {
					return nil, err
				}
				out = &p
			}
			r, err := replay(ctx, s.Component, cfg, in.Seq, payload, outSeq, out)
			if err != nil {
				return nil, err
			}
			reports = append(reports, r)
		}
	}
	return reports, nil
}

// explainAggregations replays every count connector input whose outcome is
// known: delivered, or rejected with an error in a completed run.
func explainAggregations(ctx context.Context, resolved *config.Resolved, stages []analysis.Stage, result *runresult.Result) ([]*analysis.Aggregation, error) {
	aggs := []*analysis.Aggregation{}
	count := features.Count()
	if count == nil {
		return aggs, nil
	}
	bySeq := map[int]instrument.Event{}
	for _, e := range result.Events {
		bySeq[e.Seq] = e
	}
	for _, s := range stages {
		if s.Kind != analysis.KindConnector || s.Type != "count" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		cfg := resolved.Config.Connectors[id]
		for _, in := range s.Inputs {
			if in.Outcome != analysis.OutcomeEmitted && in.Outcome != analysis.OutcomeNoOutput {
				continue
			}
			payload, err := result.Snapshots.Payload(bySeq[in.Seq].Snapshot)
			if err != nil {
				return nil, err
			}
			var out *telemetry.Payload
			outSeq := 0
			if len(in.Outputs) > 0 {
				outSeq = in.Outputs[0]
				p, err := result.Snapshots.Payload(bySeq[outSeq].Snapshot)
				if err != nil {
					return nil, err
				}
				out = &p
			}
			a, err := count(ctx, s.Component, cfg, in.Seq, payload, outSeq, out)
			if err != nil {
				return nil, err
			}
			aggs = append(aggs, a)
		}
	}
	return aggs, nil
}

// explainRouting replays every routing connector input whose outcome is
// known, against the deliveries the run recorded for it.
func explainRouting(ctx context.Context, resolved *config.Resolved, stages []analysis.Stage, result *runresult.Result) ([]*analysis.Routing, error) {
	out := []*analysis.Routing{}
	explain := features.Routing()
	if explain == nil {
		return out, nil
	}
	bySeq := map[int]instrument.Event{}
	for _, e := range result.Events {
		bySeq[e.Seq] = e
	}
	for _, s := range stages {
		if s.Kind != analysis.KindConnector || s.Type != "routing" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		cfg := resolved.Config.Connectors[id]
		for _, in := range s.Inputs {
			if in.Outcome != analysis.OutcomeEmitted && in.Outcome != analysis.OutcomeNoOutput {
				continue
			}
			payload, err := result.Snapshots.Payload(bySeq[in.Seq].Snapshot)
			if err != nil {
				return nil, err
			}
			var deliveries []features.Delivery
			for _, r := range in.Routes {
				p, err := result.Snapshots.Payload(bySeq[r.Seq].Snapshot)
				if err != nil {
					return nil, err
				}
				deliveries = append(deliveries, features.Delivery{Pipeline: r.Pipeline, Payload: p})
			}
			x, err := explain(ctx, s.Component, cfg, in.Seq, payload, in.Error, deliveries)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
	}
	return out, nil
}

// explainSampling explains the recorded decisions of every tail_sampling
// processor.
func explainSampling(ctx context.Context, resolved *config.Resolved, stages []analysis.Stage, result *runresult.Result) ([]*analysis.Sampling, error) {
	out := []*analysis.Sampling{}
	tail := features.Sampling()
	if tail == nil {
		return out, nil
	}
	for _, s := range stages {
		if s.Kind != analysis.KindProcessor || s.Type != "tail_sampling" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		var inputs []features.SamplingInput
		var decisions []features.SamplingDecision
		for _, e := range result.Events {
			if e.Component != s.Component || e.Pipeline != s.Pipeline || e.Snapshot == "" {
				continue
			}
			if e.Kind != instrument.KindIngress && (e.Kind != instrument.KindDecision || e.Decision == nil) {
				continue
			}
			p, err := result.Snapshots.Payload(e.Snapshot)
			if err != nil {
				return nil, err
			}
			if e.Kind == instrument.KindIngress {
				inputs = append(inputs, features.SamplingInput{Seq: e.Seq, Payload: p})
				continue
			}
			decisions = append(decisions, features.SamplingDecision{Seq: e.Seq, TraceID: e.Decision.TraceID, Outcome: e.Decision.Outcome, Policy: e.Decision.Policy, Spans: p})
		}
		x, err := tail(ctx, s.Component, s.Pipeline, resolved.Config.Processors[id], inputs, decisions, result.Completed())
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

// Encode writes an engine result (Report or Inspection) as indented JSON.
// Both the CLI and the API use it, so their output is byte-identical for the
// same request.
func Encode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// Decode reads one engine result (Report, Inspection, OTTLEvaluation or an
// event) written by Encode or json.Marshal, as an out-of-process runtime
// sends it. Numbers stay json.Number, as stored reports are read, so
// re-encoding preserves them; unknown fields and trailing data are refused.
func Decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// Mismatch reports whether a reference comparison found differences. An
// incomplete comparison (outputs not compared) is no mismatch.
func (r *Report) Mismatch() bool {
	return r.Comparison != nil && !r.Comparison.Equal && r.Comparison.Incomplete == ""
}
