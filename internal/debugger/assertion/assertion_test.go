package assertion

import (
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// graph is one logs pipeline exporting to a captured otlp exporter and a
// local debug exporter.
var graph = &topology.Graph{
	Pipelines: []topology.Pipeline{{
		ID: "logs", Signal: "logs",
		Receivers:  []string{"receiver/otlp"},
		Processors: []string{"processor/batch"},
		Exporters:  []string{"exporter/otlp", "exporter/debug"},
	}},
	Components: []topology.Component{
		{Ref: "receiver/otlp", Kind: topology.KindReceiver, Mode: topology.ModeSimulated},
		{Ref: "processor/batch", Kind: topology.KindProcessor, Mode: topology.ModeReal},
		{Ref: "exporter/otlp", Kind: topology.KindExporter, Mode: topology.ModeCaptured},
		{Ref: "exporter/debug", Kind: topology.KindExporter, Mode: topology.ModeReal},
	},
}

// result is a run whose otlp exporter received the given log records, each
// described by its attributes.
func result(t *testing.T, status runresult.Status, records ...map[string]any) *runresult.Result {
	t.Helper()
	r := &runresult.Result{Status: status, Outputs: []runresult.Output{}}
	if len(records) == 0 {
		return r
	}
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	lrs := rl.ScopeLogs().AppendEmpty().LogRecords()
	for _, attrs := range records {
		if err := lrs.AppendEmpty().Attributes().FromRaw(attrs); err != nil {
			t.Fatal(err)
		}
	}
	p := telemetry.FromLogs(ld)
	data, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	r.Outputs = append(r.Outputs, runresult.Output{Exporter: "otlp", Signal: "logs", Counts: p.Counts(), Data: data})
	return r
}

func ptr(n int) *int { return &n }

func TestEvaluateOutcomes(t *testing.T) {
	otlp := scenario.Target{Exporter: "otlp", Signal: "logs"}
	ok := map[string]any{"user.id": "u1", "http.status_code": 200}
	tests := []struct {
		name      string
		assertion scenario.Assertion
		result    *runresult.Result
		want      Outcome
		message   string
	}{
		{
			name:      "count holds",
			assertion: scenario.Assertion{Target: otlp, Count: &scenario.Bound{Equals: ptr(2)}},
			result:    result(t, runresult.StatusCompleted, ok, ok),
			want:      Pass,
		},
		{
			name:      "count too low",
			assertion: scenario.Assertion{Target: otlp, Count: &scenario.Bound{Min: ptr(3)}},
			result:    result(t, runresult.StatusCompleted, ok, ok),
			want:      Fail,
			message:   "2 log record(s) reached exporter otlp (logs), expected >= 3; records per stage: logs: in 0 -> batch 0",
		},
		{
			name:      "partial run never passes",
			assertion: scenario.Assertion{Target: otlp, Count: &scenario.Bound{Equals: ptr(2)}},
			result:    result(t, runresult.StatusPartial, ok, ok),
			want:      Inconclusive,
			message:   "status partial",
		},
		{
			name:      "partial run with too many records fails",
			assertion: scenario.Assertion{Target: otlp, Count: &scenario.Bound{Max: ptr(1)}},
			result:    result(t, runresult.StatusPartial, ok, ok),
			want:      Fail,
		},
		{
			name:      "attribute present",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Present: []string{"user.id"}, Absent: []string{"user.password"}}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Pass,
		},
		{
			name:      "attribute leaked",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Absent: []string{"user.password"}}},
			result:    result(t, runresult.StatusCompleted, ok, map[string]any{"user.password": "hunter2"}),
			want:      Fail,
			message:   `"user.password" is not absent on 1 of 2 record(s)`,
		},
		{
			name:      "observed violation fails a partial run",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Absent: []string{"user.password"}}},
			result:    result(t, runresult.StatusPartial, map[string]any{"user.password": "hunter2"}),
			want:      Fail,
		},
		{
			name:      "equals is type strict",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Equals: map[string]any{"http.status_code": "200"}}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Fail,
			message:   `is not "200" (string)`,
		},
		{
			name:      "equals int",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Equals: map[string]any{"http.status_code": 200}, Types: map[string]string{"user.id": "string"}}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Pass,
		},
		{
			name:      "resource attributes",
			assertion: scenario.Assertion{Target: otlp, ResourceAttributes: &scenario.Attributes{Equals: map[string]any{"service.name": "checkout"}}},
			result:    result(t, runresult.StatusCompleted, ok, ok),
			want:      Pass,
		},
		{
			name:      "no records is inconclusive",
			assertion: scenario.Assertion{Target: otlp, Attributes: &scenario.Attributes{Absent: []string{"user.password"}}},
			result:    result(t, runresult.StatusCompleted),
			want:      Inconclusive,
			message:   "no records reached the target",
		},
		{
			name:      "local exporter is recorded",
			assertion: scenario.Assertion{Target: scenario.Target{Exporter: "debug", Signal: "logs"}, Count: &scenario.Bound{Min: ptr(1)}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Fail,
			message:   "0 log record(s) reached exporter debug (logs)",
		},
		{
			name:      "unknown exporter",
			assertion: scenario.Assertion{Target: scenario.Target{Exporter: "kafka", Signal: "logs"}, Count: &scenario.Bound{Min: ptr(1)}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Error,
			message:   "logs exporters: otlp, debug",
		},
		{
			name:      "unknown processor pipeline",
			assertion: scenario.Assertion{Target: scenario.Target{Processor: "batch", Pipeline: "logs/2", Signal: "logs"}, Count: &scenario.Bound{Min: ptr(1)}},
			result:    result(t, runresult.StatusCompleted, ok),
			want:      Error,
			message:   "pipeline logs/2 does not use processor batch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := Evaluate([]scenario.Assertion{tt.assertion}, graph, tt.result)
			if err != nil {
				t.Fatal(err)
			}
			res := rep.Results[0]
			if rep.Outcome != tt.want || res.Outcome != tt.want || res.Name != "assertions[0]" {
				t.Fatalf("outcome %s (report %s), want %s: %+v", res.Outcome, rep.Outcome, tt.want, res.Checks)
			}
			if tt.message == "" {
				return
			}
			for _, c := range res.Checks {
				if strings.Contains(c.Message, tt.message) {
					return
				}
			}
			t.Fatalf("no check message contains %q: %+v", tt.message, res.Checks)
		})
	}
}

func TestEvaluateLocatesViolations(t *testing.T) {
	leak := map[string]any{"user.password": "hunter2"}
	records := []map[string]any{{}, leak}
	for range maxViolations + 2 {
		records = append(records, leak)
	}
	a := scenario.Assertion{Name: "scrubbed", Target: scenario.Target{Exporter: "otlp", Signal: "logs"},
		Attributes: &scenario.Attributes{Absent: []string{"user.password"}}}
	rep, err := Evaluate([]scenario.Assertion{a}, graph, result(t, runresult.StatusCompleted, records...))
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Results[0].Checks[0]
	if c.Check != "attributes.absent" || c.Key != "user.password" || c.Expected != "absent" ||
		c.Observed != "8 of 9 record(s) violate" || len(c.Violations) != maxViolations {
		t.Fatalf("check = %+v", c)
	}
	if v := c.Violations[0]; v.Location != "exporter otlp resource[0].scope[0].log_record[1]" || v.Observed != `"hunter2" (string)` {
		t.Fatalf("violation = %+v", v)
	}
}

func TestWorstOrdersOutcomes(t *testing.T) {
	order := []Outcome{Pass, Inconclusive, Fail, Error}
	for i, a := range order {
		for j, b := range order {
			want := order[max(i, j)]
			if got := Worst(a, b); got != want {
				t.Errorf("Worst(%s, %s) = %s, want %s", a, b, got, want)
			}
		}
	}
}

// TestEvaluateChecksWhatEndpointsReceived checks an endpoint target against
// what a simulated endpoint received, with the checks of an exporter, and
// names the endpoints a mistyped target could mean.
func TestEvaluateChecksWhatEndpointsReceived(t *testing.T) {
	g := *graph
	g.Components = append(slices.Clone(graph.Components),
		topology.Component{Ref: "extension/livecapture", Kind: topology.KindExtension, Mode: topology.ModeDependencyMocked},
		topology.Component{Ref: "extension/stubbed", Kind: topology.KindExtension, Mode: topology.ModeDependencyMocked, StandInValues: []string{"${env:TOKEN}"}})
	delivery := func(r *runresult.Result) *runresult.Result {
		r.SimulatedEndpoints = []mock.SimulatedEndpoint{
			{Component: "extension/livecapture", Path: "delivery", Protocol: "grpc"},
			{Component: "extension/livecapture", Path: "control", Protocol: "grpc"},
			{Component: "extension/stubbed", Path: "delivery", Protocol: "grpc"},
		}
		for _, o := range r.Outputs {
			r.EndpointOutputs = append(r.EndpointOutputs,
				runresult.EndpointOutput{Component: "extension/livecapture", Endpoint: "delivery", Signal: o.Signal, Counts: o.Counts, Data: o.Data},
				runresult.EndpointOutput{Component: "extension/stubbed", Endpoint: "delivery", Signal: o.Signal, Counts: o.Counts, Data: o.Data})
		}
		r.Outputs = nil
		return r
	}
	at := func(component, path string) scenario.Target {
		return scenario.Target{Endpoint: &scenario.EndpointTarget{Component: component, Path: path}, Signal: "logs"}
	}
	ok := map[string]any{"session.id": "s1"}
	tests := []struct {
		name      string
		assertion scenario.Assertion
		want      Outcome
		message   string
	}{
		{"count", scenario.Assertion{Target: at("extension/livecapture", "delivery"), Count: &scenario.Bound{Equals: ptr(2)}}, Pass, ""},
		{"attributes", scenario.Assertion{Target: at("extension/livecapture", "delivery"), Attributes: &scenario.Attributes{Present: []string{"session.id"}}}, Pass, ""},
		{"nothing received", scenario.Assertion{Target: at("extension/livecapture", "control"), Count: &scenario.Bound{Min: ptr(1)}}, Fail, "0 log record(s) reached endpoint extension/livecapture control (logs), expected >= 1"},
		{"stand-in values", scenario.Assertion{Target: at("extension/stubbed", "delivery"), Count: &scenario.Bound{Equals: ptr(2)}}, Inconclusive, "extension/stubbed (configured with stand-ins for ${env:TOKEN})"},
		{"unknown path", scenario.Assertion{Target: at("extension/livecapture", "upload"), Count: &scenario.Bound{Min: ptr(1)}}, Error, "its simulated endpoints: delivery, control"},
		{"no endpoints", scenario.Assertion{Target: at("processor/batch", "delivery"), Count: &scenario.Bound{Min: ptr(1)}}, Error, "processor/batch has no simulated endpoint in this run"},
		{"unknown component", scenario.Assertion{Target: at("extension/other", "delivery"), Count: &scenario.Bound{Min: ptr(1)}}, Error, "the configuration has no component extension/other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep, err := Evaluate([]scenario.Assertion{tt.assertion}, &g, delivery(result(t, runresult.StatusCompleted, ok, ok)))
			if err != nil {
				t.Fatal(err)
			}
			res := rep.Results[0]
			if res.Outcome != tt.want {
				t.Fatalf("outcome %s, want %s: %+v", res.Outcome, tt.want, res.Checks)
			}
			if !strings.HasPrefix(res.Target, "endpoint extension/") && !strings.HasPrefix(res.Target, "endpoint processor/") {
				t.Fatalf("target = %q", res.Target)
			}
			if tt.message == "" {
				return
			}
			for _, c := range res.Checks {
				if strings.Contains(c.Message, tt.message) {
					return
				}
			}
			t.Fatalf("no check message contains %q: %+v", tt.message, res.Checks)
		})
	}
}
