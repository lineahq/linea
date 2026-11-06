package filterreplay

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor/processortest"
	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// filterConfig unmarshals and validates a filter processor configuration
// as the Collector does.
func filterConfig(t *testing.T, text string) *filterprocessor.Config {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(text), &m); err != nil {
		t.Fatal(err)
	}
	cfg := filterprocessor.NewFactory().CreateDefaultConfig()
	if err := confmap.NewFromStringMap(m).Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := confmap.Validate(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.(*filterprocessor.Config)
}

func payload(t *testing.T, text string) telemetry.Payload {
	t.Helper()
	p, err := telemetry.Decode([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// process runs the real filter processor on in and returns what it passed
// on (nil when nothing) and the error it returned: the observation the
// replay is checked against.
func process(t *testing.T, cfg *filterprocessor.Config, in telemetry.Payload) (*telemetry.Payload, string) {
	t.Helper()
	ctx, f := context.Background(), filterprocessor.NewFactory()
	set := processortest.NewNopSettings(f.Type())
	in = in.Clone()
	var out []telemetry.Payload
	var err error
	switch in.Signal {
	case pipeline.SignalLogs:
		sink := new(consumertest.LogsSink)
		p, cerr := f.CreateLogs(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
			t.Fatal(err)
		}
		err = p.ConsumeLogs(ctx, in.Logs)
		for _, l := range sink.AllLogs() {
			out = append(out, telemetry.FromLogs(l))
		}
	case pipeline.SignalTraces:
		sink := new(consumertest.TracesSink)
		p, cerr := f.CreateTraces(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
			t.Fatal(err)
		}
		err = p.ConsumeTraces(ctx, in.Traces)
		for _, td := range sink.AllTraces() {
			out = append(out, telemetry.FromTraces(td))
		}
	case pipeline.SignalMetrics:
		sink := new(consumertest.MetricsSink)
		p, cerr := f.CreateMetrics(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
			t.Fatal(err)
		}
		err = p.ConsumeMetrics(ctx, in.Metrics)
		for _, md := range sink.AllMetrics() {
			out = append(out, telemetry.FromMetrics(md))
		}
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if c, ok := telemetry.Concat(out); ok {
		return &c, msg
	}
	return nil, msg
}

const logsInput = `{"resourceLogs":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]},
  "scopeLogs":[{"scope":{"name":"s1"},"logRecords":[
   {"body":{"stringValue":"healthcheck ok"}},
   {"body":{"stringValue":"{\"a\":1}"}},
   {"body":{"stringValue":"plain"}}]}]},
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"b"}}]},
  "scopeLogs":[{"scope":{"name":"s2"},"logRecords":[{"body":{"stringValue":"plain b"}}]}]}]}`

const tracesInput = `{"resourceSpans":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
  "scopeSpans":[
   {"scope":{"name":"http"},"spans":[
    {"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","name":"GET /health"},
    {"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060709","name":"GET /orders",
     "events":[{"name":"cache.miss"},{"name":"retry"}]}]},
   {"scope":{"name":"internal"},"spans":[
    {"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"010203040506070a","name":"work"}]}]}]}`

const metricsInput = `{"resourceMetrics":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
  "scopeMetrics":[{"scope":{"name":"m"},"metrics":[
   {"name":"requests","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
    {"asInt":"3","attributes":[{"key":"route","value":{"stringValue":"/health"}}]},
    {"asInt":"5","attributes":[{"key":"route","value":{"stringValue":"/orders"}}]}]}},
   {"name":"debug.queue","gauge":{"dataPoints":[{"asInt":"1"}]}},
   {"name":"health.only","gauge":{"dataPoints":[
    {"asInt":"1","attributes":[{"key":"route","value":{"stringValue":"/health"}}]}]}}]}]}]}`

type drop struct {
	ref       string
	condition int
	records   int
}

func TestExplainAttributesEachDropToItsCondition(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		input    string
		contexts []string
		drops    []drop
		dropped  int
		errors   []int
		propagat bool
	}{
		{
			name: "inferred contexts run resource first",
			config: `
log_conditions:
  - conditions:
      - IsMatch(log.body, "^healthcheck")
      - resource.attributes["service.name"] == "b"`,
			input:    logsInput,
			contexts: []string{"log", "resource"},
			drops:    []drop{{"resource[0].scope[0].log_record[0]", 0, 1}, {"resource[1]", 1, 1}},
			dropped:  2,
		},
		{
			name: "deprecated list skips failing conditions under ignore",
			config: `
logs:
  log_record:
    - ParseJSON(body)["a"] == 1`,
			input:    logsInput,
			contexts: []string{"log"},
			drops:    []drop{{"resource[0].scope[0].log_record[1]", 0, 1}},
			dropped:  1,
			errors:   []int{3},
		},
		{
			name: "propagate loses the whole request",
			config: `
error_mode: propagate
logs:
  log_record:
    - ParseJSON(body)["a"] == 1`,
			input:    logsInput,
			contexts: []string{"log"},
			drops:    []drop{{"resource[0].scope[0].log_record[1]", 0, 1}},
			dropped:  4,
			errors:   []int{3},
			propagat: true,
		},
		{
			name: "later groups see what earlier groups kept",
			config: `
log_conditions:
  - context: log
    conditions:
      - body == "plain"
  - context: scope
    conditions:
      - name == "s1"
      - name == "s2"`,
			input:    logsInput,
			contexts: []string{"log", "scope", "scope"},
			drops:    []drop{{"resource[0].scope[0].log_record[2]", 0, 1}, {"resource[0].scope[0]", 1, 2}, {"resource[1].scope[0]", 2, 1}},
			dropped:  4,
		},
		{
			name: "span events go, their span stays",
			config: `
trace_conditions:
  - conditions:
      - span.name == "GET /health"
      - spanevent.name == "retry"
      - scope.name == "internal"`,
			input:    tracesInput,
			contexts: []string{"span", "spanevent", "scope"},
			drops:    []drop{{"resource[0].scope[0].span[0]", 0, 1}, {"resource[0].scope[0].span[1].event[1]", 1, 0}, {"resource[0].scope[1]", 2, 1}},
			dropped:  2,
		},
		{
			name: "a metric emptied of data points is removed",
			config: `
metric_conditions:
  - conditions:
      - metric.name == "debug.queue"
      - datapoint.attributes["route"] == "/health"`,
			input:    metricsInput,
			contexts: []string{"metric", "datapoint"},
			drops:    []drop{{"resource[0].scope[0].metric[0].data_point[0]", 1, 1}, {"resource[0].scope[0].metric[1]", 0, 1}, {"resource[0].scope[0].metric[2].data_point[0]", 1, 1}},
			dropped:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := filterConfig(t, tt.config)
			in := payload(t, tt.input)
			out, stageErr := process(t, cfg, in)
			outSeq := 0
			if out != nil {
				outSeq = 9
			}
			got, err := Explain(context.Background(), "filter", cfg, 1, in, outSeq, out, stageErr)
			if err != nil {
				t.Fatal(err)
			}
			if got.Verification != analysis.FilterVerified {
				t.Fatalf("verification = %s, notes %v", got.Verification, got.Notes)
			}
			var contexts []string
			for _, c := range got.Conditions {
				contexts = append(contexts, c.Context)
			}
			if !slices.Equal(contexts, tt.contexts) {
				t.Errorf("contexts = %v, want %v", contexts, tt.contexts)
			}
			var drops []drop
			for _, d := range got.Drops {
				drops = append(drops, drop{d.Ref, d.Condition, d.Records})
			}
			if !slices.Equal(drops, tt.drops) {
				t.Errorf("drops = %v, want %v", drops, tt.drops)
			}
			if got.Dropped != tt.dropped {
				t.Errorf("dropped = %d, want %d", got.Dropped, tt.dropped)
			}
			for i, n := range tt.errors {
				if got.Conditions[i].Errors != n {
					t.Errorf("condition %d errors = %d, want %d", i, got.Conditions[i].Errors, n)
				}
			}
			if tt.propagat != (got.Error != "") {
				t.Errorf("error = %q, propagate %v", got.Error, tt.propagat)
			}
		})
	}
}

func TestExplainCountsEvaluationsAndMatches(t *testing.T) {
	cfg := filterConfig(t, `
log_conditions:
  - context: log
    conditions:
      - body == "never"
      - IsMatch(body, "plain")`)
	in := payload(t, logsInput)
	out, stageErr := process(t, cfg, in)
	got, err := Explain(context.Background(), "filter", cfg, 1, in, 2, out, stageErr)
	if err != nil {
		t.Fatal(err)
	}
	type counts struct{ evaluated, matched, records int }
	want := []counts{{4, 0, 0}, {4, 2, 2}}
	for i, c := range got.Conditions {
		if g := (counts{c.Evaluated, c.Matched, c.Records}); g != want[i] {
			t.Errorf("condition %d = %+v, want %+v", i, g, want[i])
		}
	}
	if got.Conditions[1].Source != "log_conditions[0].conditions[1]" {
		t.Errorf("source = %q", got.Conditions[1].Source)
	}
}

func TestExplainReportsDisagreement(t *testing.T) {
	cfg := filterConfig(t, `
log_conditions:
  - conditions:
      - IsMatch(log.body, "^healthcheck")`)
	in := payload(t, logsInput)
	// The recorded output still holds every record: the processor did not
	// do what the evaluation says.
	unchanged := in.Clone()
	got, err := Explain(context.Background(), "filter", cfg, 1, in, 2, &unchanged, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.FilterDiffers || got.Drops != nil {
		t.Fatalf("verification = %s, drops %v", got.Verification, got.Drops)
	}
	if got.Dropped != 0 {
		t.Errorf("dropped = %d, want the observed 0", got.Dropped)
	}
}

func TestExplainLeavesMatchPropertiesUnsupported(t *testing.T) {
	cfg := filterConfig(t, `
logs:
  exclude:
    match_type: strict
    bodies: ["plain"]`)
	in := payload(t, logsInput)
	out, stageErr := process(t, cfg, in)
	got, err := Explain(context.Background(), "filter", cfg, 1, in, 2, out, stageErr)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.FilterUnsupported || !strings.Contains(strings.Join(got.Notes, " "), "match properties") {
		t.Fatalf("verification = %s, notes %v", got.Verification, got.Notes)
	}
	if got.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", got.Dropped)
	}
}
