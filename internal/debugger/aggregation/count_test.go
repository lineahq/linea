package aggregation

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/countconnector"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func load(t *testing.T, path string) telemetry.Payload {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	p, err := telemetry.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// countConfig resolves a count connector block through the real Collector
// config pipeline, with the connector joining a pipeline of signal.
func countConfig(t *testing.T, signal, block string) component.Config {
	t.Helper()
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"connectors:\n  count:\n" + block +
		"exporters: {nop: {}}\n" +
		"service: {pipelines: {" + signal + ": {receivers: [otlp], exporters: [count]}, metrics/out: {receivers: [count], exporters: [nop]}}}\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(context.Background(), path, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Config.Connectors[component.MustNewID("count")]
}

// runConnector executes the real count connector on in and returns its
// output, or nil when it returned an error.
func runConnector(t *testing.T, cfg component.Config, in telemetry.Payload) *telemetry.Payload {
	t.Helper()
	ctx, f := context.Background(), countconnector.NewFactory()
	set := connectortest.NewNopSettings(f.Type())
	sink := &consumertest.MetricsSink{}
	var err error
	switch in.Signal {
	case pipeline.SignalLogs:
		c, cerr := f.CreateLogsToMetrics(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		err = c.ConsumeLogs(ctx, in.Clone().Logs)
	case pipeline.SignalTraces:
		c, cerr := f.CreateTracesToMetrics(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		err = c.ConsumeTraces(ctx, in.Clone().Traces)
	case pipeline.SignalMetrics:
		c, cerr := f.CreateMetricsToMetrics(ctx, set, cfg, sink)
		if cerr != nil {
			t.Fatal(cerr)
		}
		err = c.ConsumeMetrics(ctx, in.Clone().Metrics)
	}
	if err != nil {
		return nil
	}
	out := telemetry.FromMetrics(sink.AllMetrics()[0])
	return &out
}

// TestCountReplayMatchesTheConnector replays configurations on fixtures,
// checks each against the real connector's output and that every counted
// point names exactly as many records as it counts.
func TestCountReplayMatchesTheConnector(t *testing.T) {
	cases := []struct {
		name, signal, fixture, block string
		want                         analysis.AggregationVerification
		uncounted                    int
	}{
		{"default logs", "logs", "testdata/multisignal/logs.json", "", analysis.AggregationVerified, 0},
		{"default traces with events", "traces", "testdata/traces/traces.json", "", analysis.AggregationVerified, 0},
		{"default metrics", "metrics", "samples/metrics-rename/input.json", "", analysis.AggregationVerified, 0},
		{"conditions and attributes", "logs", "samples/log-metrics/input.json",
			"    logs:\n      errors:\n        conditions: ['log.severity_number >= SEVERITY_NUMBER_ERROR']\n" +
				"        attributes: [{key: service.name}, {key: http.route, default_value: unknown}]\n" +
				"      routed:\n        attributes: [{key: http.route}]\n",
			analysis.AggregationVerified, 2},
		{"span attribute", "traces", "testdata/traces/traces.json",
			"    spans:\n      by.service:\n        attributes: [{key: service.name}]\n",
			analysis.AggregationVerified, 0},
		{"data point condition", "metrics", "samples/metrics-rename/input.json",
			"    datapoints:\n      big:\n        conditions: ['datapoint.value_double > 1.0 or datapoint.value_int > 1']\n",
			analysis.AggregationVerified, -1},
		{"condition error", "logs", "samples/log-metrics/input.json",
			"    logs:\n      parsed:\n        conditions: ['ParseJSON(log.body)[\"ok\"] == true']\n",
			analysis.AggregationVerified, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := countConfig(t, tc.signal, tc.block)
			in := load(t, tc.fixture)
			out := runConnector(t, cfg, in)
			outSeq := 0
			if out != nil {
				outSeq = 3
			}
			agg, err := Count(context.Background(), "count", cfg, 2, in, outSeq, out)
			if err != nil {
				t.Fatal(err)
			}
			if agg.Verification != tc.want {
				t.Fatalf("verification = %s, want %s; notes %v", agg.Verification, tc.want, agg.Notes)
			}
			if tc.uncounted >= 0 && len(agg.Uncounted) != tc.uncounted {
				t.Fatalf("uncounted = %v, want %d", agg.Uncounted, tc.uncounted)
			}
			if out == nil {
				if len(agg.Points) != 0 || len(agg.Notes) == 0 {
					t.Fatalf("an emitted-nothing aggregation has points %v, notes %v", agg.Points, agg.Notes)
				}
				return
			}
			if len(agg.Points) != out.Counts().Items {
				t.Fatalf("%d points, the output has %d", len(agg.Points), out.Counts().Items)
			}
			for _, p := range agg.Points {
				if int64(len(p.Records)) != p.Value {
					t.Fatalf("%s %v counts %d but names %v", p.Metric, p.Attributes, p.Value, p.Records)
				}
			}
		})
	}
}

// TestCountAttributesRecordsToPoints checks the attribution itself.
func TestCountAttributesRecordsToPoints(t *testing.T) {
	cfg := countConfig(t, "logs", "    logs:\n      errors:\n        conditions: ['log.severity_number >= SEVERITY_NUMBER_ERROR']\n"+
		"        attributes: [{key: service.name}, {key: http.route, default_value: unknown}]\n")
	in := load(t, "samples/log-metrics/input.json")
	agg, err := Count(context.Background(), "count", cfg, 2, in, 3, runConnector(t, cfg, in))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, p := range agg.Points {
		got[p.Attributes["service.name"]+" "+p.Attributes["http.route"]] = p.Records
	}
	want := map[string][]string{
		"payments /pay":    {"resource[0].scope[0].log_record[0]"},
		"payments unknown": {"resource[0].scope[0].log_record[1]"},
		"checkout /cart":   {"resource[1].scope[0].log_record[1]"},
	}
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s: records %v, want %v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("points %v, want %v", got, want)
	}
	wantUncounted := []string{"resource[0].scope[0].log_record[2]", "resource[0].scope[0].log_record[3]", "resource[1].scope[0].log_record[0]"}
	if !slices.Equal(agg.Uncounted, wantUncounted) {
		t.Errorf("uncounted %v, want %v", agg.Uncounted, wantUncounted)
	}
}

// TestCountReportsADifferingOutput checks that an output the replay does
// not reproduce is never attributed.
func TestCountReportsADifferingOutput(t *testing.T) {
	cfg := countConfig(t, "logs", "")
	in := load(t, "samples/log-metrics/input.json")
	out := runConnector(t, cfg, in)
	dp := out.Metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Sum().DataPoints().At(0)
	dp.SetIntValue(dp.IntValue() + 1)
	agg, err := Count(context.Background(), "count", cfg, 2, in, 3, out)
	if err != nil {
		t.Fatal(err)
	}
	if agg.Verification != analysis.AggregationDiffers || len(agg.Notes) == 0 {
		t.Fatalf("verification = %s, notes %v; want replay_differs with a note", agg.Verification, agg.Notes)
	}
	for _, p := range agg.Points {
		if len(p.Records) > 0 {
			t.Fatalf("unverified point %s attributed to %v", p.Metric, p.Records)
		}
	}
}
