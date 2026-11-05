package engine

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/release"
)

var tracesFixture = filepath.Join("..", "..", "..", "testdata", "traces", "traces.json")

// TestExecuteReportsRejectedInput: with error_mode propagate a failing OTTL
// statement makes the Collector reject the input. The instrumented and the
// reference run must agree on that, the transform stage must not be read as
// filtering, and the replay must reproduce the rejection.
func TestExecuteReportsRejectedInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	cfg := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform:
    error_mode: propagate
    trace_statements:
      - context: span
        statements:
          - merge_maps(attributes, attributes["http.status_code"], "insert") where attributes["http.status_code"] != nil
exporters: {otlp: {endpoint: "collector.example.invalid:4317"}}
service: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [otlp]}}}
`
	if err := os.WriteFile(cfg, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	rep, err := Execute(ctx, Request{Config: cfg, Fixture: tracesFixture, Compare: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Result.Inputs[0].Error == "" || rep.Reference.Inputs[0].Error == "" {
		t.Fatalf("input not rejected: %+v / %+v", rep.Result.Inputs, rep.Reference.Inputs)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("comparison: %v", rep.Comparison.Differences)
	}
	if len(rep.Stages) != 1 || rep.Stages[0].Inputs[0].Outcome != analysis.OutcomeNoOutput {
		t.Fatalf("stages = %+v", rep.Stages)
	}
	if len(rep.OTTL) != 1 || rep.OTTL[0].Verification != ottldebug.VerificationReplayVerified || rep.OTTL[0].Error == "" {
		t.Fatalf("ottl = %+v", rep.OTTL)
	}
}

// TestExecuteMultisignalScenarioMatchesReference covers the basic processors
// (resource, attributes, filter, transform on logs, batch) in two pipelines
// that share processors and an exporter: the instrumented run must deliver
// exactly what an uninstrumented Collector delivers, per exporter and signal.
func TestExecuteMultisignalScenarioMatchesReference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{
		Config:   multisignalConfig,
		Scenario: filepath.Join("..", "..", "..", "testdata", "multisignal", "scenario.yaml"),
		Compare:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("comparison: %v", rep.Comparison.Differences)
	}
	var outputs []string
	for _, o := range rep.Result.Outputs {
		outputs = append(outputs, o.Exporter+"/"+o.Signal)
	}
	if !slices.Equal(outputs, []string{"debug/logs", "otlp/backend/traces", "otlp/backend/logs"}) {
		t.Fatalf("outputs = %v", outputs)
	}
	for _, o := range rep.Result.Outputs {
		data := string(o.Data)
		if o.Counts.Items != 1 || strings.Contains(data, "hunter2") || strings.Contains(data, `"jane"`) ||
			!strings.Contains(data, `"test"`) {
			t.Errorf("%s/%s not processed as configured: %s", o.Exporter, o.Signal, data)
		}
	}

	// Log records have no stable ID: the filter's drop is a removal and the
	// attribute scrub is a confidence-qualified change.
	logStage := func(component string) *analysis.Diff {
		for _, s := range rep.Stages {
			if s.Component == component && s.Pipeline == "logs" && len(s.Inputs) == 1 {
				return s.Inputs[0].Diff
			}
		}
		t.Fatalf("no logs stage %s", component)
		return nil
	}
	if d := logStage("filter/health"); d.Identity != analysis.IdentityContent || len(d.Removed) != 1 || d.Removed[0].Name != "healthcheck ok" {
		t.Errorf("filter/health logs diff = %+v", d)
	}
	scrub := logStage("attributes/scrub")
	if len(scrub.Changes) == 0 {
		t.Fatalf("attributes/scrub logs diff = %+v", scrub)
	}
	// attributes/scrub changes its records in place: the same records.
	for _, c := range scrub.Changes {
		if c.Confidence != analysis.ConfidenceObject {
			t.Errorf("scrub change %+v is not an object match", c)
		}
	}

	// log_statements are replayed and verified against the observed output.
	if len(rep.OTTL) != 1 || rep.OTTL[0].Processor != "transform/logs" || rep.OTTL[0].Signal != "logs" ||
		rep.OTTL[0].Verification != ottldebug.VerificationReplayVerified || rep.OTTL[0].Groups[0].Statements[0].Matched != 1 {
		t.Errorf("ottl = %+v", rep.OTTL)
	}

	p := rep.Provenance
	if p == nil || p.CollectorVersion != release.CollectorVersion || len(p.Modules) != len(release.Modules) ||
		len(p.Config.SHA256) != 64 || p.Scenario == nil || p.Fixture != nil ||
		!slices.Equal(p.Environment, []string{"DEPLOY_ENV"}) || p.Settings != (Settings{Timeout: "30s", Compare: true}) {
		t.Fatalf("provenance = %+v", p)
	}
}

var logRouting = filepath.Join("..", "..", "..", "samples", "log-routing")

// TestExecuteFollowsRecordsThroughARoutingConnector: the routing connector
// runs for real, matches the reference Collector, reports each delivery as a
// route and lets every injected record be followed to its exporters.
func TestExecuteFollowsRecordsThroughARoutingConnector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{
		Config:   filepath.Join(logRouting, "config.yaml"),
		Scenario: filepath.Join(logRouting, "scenario.yaml"),
		Compare:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("comparison: %v", rep.Comparison.Differences)
	}

	// Every stage event but the injection is caused by an earlier event.
	for _, e := range rep.Result.Events {
		if (e.Kind == instrument.KindIngress || e.Kind == instrument.KindExport) && (e.FromSeq == 0 || e.FromSeq >= e.Seq) {
			t.Errorf("event %d %s %s from_seq = %d", e.Seq, e.Kind, e.Component, e.FromSeq)
		}
	}

	var routes []string
	for _, s := range rep.Stages {
		if s.Component != "routing" {
			continue
		}
		if s.Kind != analysis.KindConnector || len(s.Inputs) != 1 {
			t.Fatalf("routing stage = %+v", s)
		}
		for _, r := range s.Inputs[0].Routes {
			routes = append(routes, fmt.Sprintf("%s:%d", r.Pipeline, r.Items))
		}
	}
	slices.Sort(routes)
	if want := []string{"logs/archive:3", "logs/audit:1", "logs/pager:1"}; !slices.Equal(routes, want) {
		t.Errorf("routes = %v, want %v", routes, want)
	}

	got := map[string][]string{}
	for _, r := range rep.Lineage {
		for _, d := range r.Deliveries {
			got[r.Name] = append(got[r.Name], d.Exporter)
		}
		for _, s := range r.Stops {
			got[r.Name] = append(got[r.Name], string(s.End)+" at "+s.Component)
		}
		slices.Sort(got[r.Name])
	}
	want := map[string][]string{
		"card declined":    {"otlp/archive", "otlp/pager"},
		"payment accepted": {"otlp/archive"},
		"cache miss":       {"dropped at filter/debug"},
		"user signed in":   {"otlp/audit"},
	}
	for name, w := range want {
		if !slices.Equal(got[name], w) {
			t.Errorf("%s ends = %v, want %v", name, got[name], w)
		}
	}
}

var logMetrics = filepath.Join("..", "..", "..", "samples", "log-metrics")

// TestExecuteAttributesRecordsToCountedDataPoints: the count connector turns
// logs into data points; the report names the records each point counts,
// ends counted records at the connector and follows each point to its
// exporter as a lineage of its own.
func TestExecuteAttributesRecordsToCountedDataPoints(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{
		Config:   filepath.Join(logMetrics, "config.yaml"),
		Scenario: filepath.Join(logMetrics, "scenario.yaml"),
		Compare:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("comparison: %v", rep.Comparison.Differences)
	}
	if len(rep.Aggregations) != 1 {
		t.Fatalf("aggregations = %+v, want one", rep.Aggregations)
	}
	agg := rep.Aggregations[0]
	if agg.Connector != "count" || agg.Verification != analysis.AggregationVerified {
		t.Fatalf("aggregation %s is %s: %v", agg.Connector, agg.Verification, agg.Notes)
	}
	points := map[string]int{}
	for _, p := range agg.Points {
		points[p.Metric+" "+p.Attributes["service.name"]+" "+p.Attributes["http.route"]] = len(p.Records)
	}
	want := map[string]int{
		"app.log.errors payments /pay":    1,
		"app.log.errors payments unknown": 1,
		"app.log.errors checkout /cart":   1,
		"app.log.records payments ":       3,
		"app.log.records checkout ":       2,
	}
	if !maps.Equal(points, want) {
		t.Errorf("points = %v, want %v", points, want)
	}

	byID := map[string]analysis.RecordLineage{}
	for _, r := range rep.Lineage {
		byID[r.ID] = r
	}
	var derived int
	for _, r := range rep.Lineage {
		if r.Origin != analysis.OriginAggregated {
			continue
		}
		derived++
		if r.Connector != "count" || len(r.Sources) == 0 {
			t.Errorf("%s: connector %q, sources %v", r.Name, r.Connector, r.Sources)
		}
		if len(r.Deliveries) != 1 || r.Deliveries[0].Exporter != "otlp/metrics" {
			t.Errorf("%s: deliveries %+v, want otlp/metrics", r.Name, r.Deliveries)
		}
		// Every source names this point among the points it went into.
		for _, src := range r.Sources {
			s, ok := byID[src]
			if !ok {
				t.Errorf("%s: unknown source %s", r.Name, src)
				continue
			}
			found := false
			for _, h := range s.Hops {
				found = found || (h.End == analysis.EndAggregated && slices.Contains(h.Into, r.ID))
			}
			if !found {
				t.Errorf("%s: source %s does not end aggregated into it", r.Name, s.Name)
			}
		}
	}
	if derived != len(want) {
		t.Errorf("%d derived lineages, want %d", derived, len(want))
	}
}

// TestExecuteAcceptsInlineConfigAndScenario covers the web client's inputs:
// configuration and scenario as text with inline telemetry, a chosen run ID
// and an event observer.
func TestExecuteAcceptsInlineConfigAndScenario(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "multisignal", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenarioYAML := "version: 1\nname: inline\nenvironment: {DEPLOY_ENV: test}\ninputs:\n  - data: '" +
		strings.ReplaceAll(strings.TrimSpace(string(fixture)), "\n", " ") + "'\n"
	var observed int
	rep, err := Execute(ctx, Request{
		ConfigYAML:   string(cfg),
		ScenarioYAML: scenarioYAML,
		RunID:        "run-inline",
		Observer:     func(instrument.Event) { observed++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Result.RunID != "run-inline" || observed == 0 || observed != len(rep.Result.Events) {
		t.Fatalf("run %s, observed %d of %d events", rep.Result.RunID, observed, len(rep.Result.Events))
	}
	p := rep.Provenance
	if p.Config.Path != "" || len(p.Config.SHA256) != 64 || p.Scenario == nil || p.Scenario.Path != "" || rep.Scenario != "inline" {
		t.Fatalf("provenance = %+v", p)
	}
}

// TestExecuteRunsInlineFixtureLikeAFixtureFile: fixture text is the quick
// mode of the web client and must behave exactly as the same file does.
func TestExecuteRunsInlineFixtureLikeAFixtureFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path := filepath.Join("..", "..", "..", "testdata", "multisignal", "logs.json")
	fixture, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	run := func(req Request) *Report {
		t.Helper()
		req.Config, req.Receiver, req.Normalize = multisignalConfig, "otlp", true
		rep, err := Execute(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	file, inline := run(Request{Fixture: path}), run(Request{FixtureJSON: string(fixture)})
	if inline.Provenance.Fixture == nil || inline.Provenance.Fixture.Path != "" ||
		inline.Provenance.Fixture.SHA256 != file.Provenance.Fixture.SHA256 {
		t.Fatalf("fixture provenance = %+v, want the file's hash %+v", inline.Provenance.Fixture, file.Provenance.Fixture)
	}
	if len(inline.Result.Outputs) == 0 || len(inline.Result.Outputs) != len(file.Result.Outputs) ||
		inline.Result.Outputs[0].Counts != file.Result.Outputs[0].Counts {
		t.Fatalf("inline outputs %+v, file outputs %+v", inline.Result.Outputs, file.Result.Outputs)
	}
}

func TestExecuteRejectsAmbiguousRequests(t *testing.T) {
	tests := []Request{
		{},
		{Config: "c.yaml"},
		{Config: "c.yaml", ConfigYAML: "receivers: {}", Fixture: "f.json"},
		{Config: "c.yaml", Fixture: "f.json", Scenario: "s.yaml"},
		{Config: "c.yaml", Fixture: "f.json", FixtureJSON: "{}"},
		{Config: "c.yaml", FixtureJSON: "{}", ScenarioYAML: "version: 1"},
		{Config: "c.yaml", Scenario: "s.yaml", ScenarioYAML: "version: 1"},
		{Config: "c.yaml", Scenario: "s.yaml", Receiver: "otlp"},
		{ConfigYAML: "receivers: {}", ScenarioYAML: "version: 2"},
		{Config: "c.yaml", Scenario: "s.yaml", ScenarioDir: "."},
	}
	for _, req := range tests {
		if _, err := Execute(context.Background(), req); err == nil {
			t.Errorf("%+v: no error", req)
		}
	}
}

// TestExecuteNormalizedReportsAreIdentical checks that repeated executions
// of a deterministic scenario encode to identical normalized reports, even
// though the Collector stops independent pipelines, and flushes their
// batches into shared exporters, in varying order.
func TestExecuteNormalizedReportsAreIdentical(t *testing.T) {
	multisignalDir := filepath.Join("..", "..", "..", "testdata", "multisignal")
	tests := []struct {
		name string
		req  Request
	}{
		{"scenario", Request{Config: multisignalConfig, Scenario: filepath.Join(multisignalDir, "scenario.yaml")}},
		{"shared exporter", Request{Config: filepath.Join(multisignalDir, "fanin.yaml"), Fixture: filepath.Join(multisignalDir, "traces.json")}},
		{"routing connector", Request{Config: filepath.Join(logRouting, "config.yaml"), Scenario: filepath.Join(logRouting, "scenario.yaml")}},
		{"count connector", Request{Config: filepath.Join(logMetrics, "config.yaml"), Scenario: filepath.Join(logMetrics, "scenario.yaml")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			req := tt.req
			req.Compare, req.Normalize = true, true
			var first []byte
			for i := range 4 {
				rep, err := Execute(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				if !rep.Comparison.Equal {
					t.Fatalf("comparison: %v", rep.Comparison.Differences)
				}
				var buf bytes.Buffer
				if err := Encode(&buf, rep); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					first = buf.Bytes()
				} else if !bytes.Equal(buf.Bytes(), first) {
					t.Fatalf("run %d report differs from the first", i+1)
				}
			}
		})
	}
}

// TestInspectToleratesInvalidScenarioText: editor text may be mid-edit; the
// configuration is still validated and the scenario problem is a warning.
func TestInspectToleratesInvalidScenarioText(t *testing.T) {
	ins, err := Inspect(context.Background(), InspectRequest{Config: multisignalConfig, ScenarioYAML: "version: 1\ninputs: [\n"})
	if err != nil {
		t.Fatal(err)
	}
	// ${env:DEPLOY_ENV:-dev} has a default, so the config stays valid.
	if !ins.Valid || len(ins.Diagnostics) != 1 || ins.Diagnostics[0].Code != CodeScenarioIgnored {
		t.Fatalf("inspection = %+v", ins)
	}
	if _, err := Inspect(context.Background(), InspectRequest{Config: multisignalConfig, Scenario: "missing.yaml"}); err == nil {
		t.Fatal("missing scenario file accepted")
	}
}

// TestExecuteExplainsTailSamplingDecisions: every trace the sample's
// tail_sampling processor decided is reported with the deciding policy,
// what each policy found in it, and the late spans that followed the
// decision; the replay reproduces every recorded decision.
func TestExecuteExplainsTailSamplingDecisions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := filepath.Join("..", "..", "..", "samples", "trace-sampling")
	rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Sampling) != 1 {
		t.Fatalf("sampling = %+v, want one processor", rep.Sampling)
	}
	var got []string
	for _, tr := range rep.Sampling[0].Traces {
		if tr.Verification != analysis.ReplayVerified {
			t.Errorf("%s: %s", tr.Name, tr.Verification)
		}
		line := fmt.Sprintf("%s %s %s late=%d %s |", tr.Name, tr.Decision, tr.Policy, tr.LateSpans, tr.Late)
		for _, p := range tr.Policies {
			line += fmt.Sprintf(" %s=%s (%s)", p.Policy, p.Result, p.Evidence)
		}
		got = append(got, line)
	}
	want := []string{
		"POST /orders sampled errors late=1 forwarded | errors=sampled (span statuses: ERROR) slow=not_sampled (lasted 400ms)",
		"GET /search sampled slow late=0  | errors=not_sampled (span statuses: UNSET) slow=sampled (lasted 850ms)",
		"GET /orders/{id} not_sampled  late=1 dropped | errors=not_sampled (span statuses: UNSET) slow=not_sampled (lasted 40ms)",
		"GET /health not_sampled  late=0  | errors=not_sampled (span statuses: UNSET) slow=not_sampled (lasted 2ms)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("traces:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExecuteExplainsRoutingDecisions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := filepath.Join("..", "..", "..", "samples", "log-routing")
	rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Routing) != 1 {
		t.Fatalf("routing = %+v, want one input", rep.Routing)
	}
	r := rep.Routing[0]
	if r.Verification != analysis.RoutingVerified {
		t.Fatalf("verification = %s, notes %v", r.Verification, r.Notes)
	}
	var got []string
	for _, rec := range r.Records {
		got = append(got, fmt.Sprintf("%s rules=%v default=%t", rec.Ref, rec.Rules, rec.Default))
	}
	want := []string{
		"resource[0].scope[0].log_record[0] rules=[0] default=true",
		"resource[0].scope[0].log_record[1] rules=[] default=true",
		"resource[0].scope[0].log_record[2] rules=[] default=true",
		"resource[1].scope[0].log_record[0] rules=[1] default=false",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
