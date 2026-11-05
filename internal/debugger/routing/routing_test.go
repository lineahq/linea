package routing

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/routingconnector"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer"
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

// routingConfig resolves a configuration file or text through the real
// Collector config pipeline and returns its routing connector.
func routingConfig(t *testing.T, path, text string) *routingconnector.Config {
	t.Helper()
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		path = filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		path = filepath.Join("..", "..", "..", path)
	}
	resolved, err := config.Resolve(context.Background(), path, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Config.Connectors[component.MustNewID("routing")].(*routingconnector.Config)
}

// deliver runs the real routing connector, with its own table, on in and
// returns what each pipeline received: the observation the replay is checked
// against.
func deliver(t *testing.T, c *routingconnector.Config, in telemetry.Payload) []Delivery {
	t.Helper()
	ctx, f := context.Background(), routingconnector.NewFactory()
	set := connectortest.NewNopSettings(f.Type())
	ids := map[pipeline.ID]bool{}
	for _, p := range c.DefaultPipelines {
		ids[p] = true
	}
	for _, item := range c.Table {
		for _, p := range item.Pipelines {
			ids[p] = true
		}
	}
	var out []Delivery
	in = in.Clone()
	switch in.Signal {
	case pipeline.SignalLogs:
		sinks, m := map[pipeline.ID]*consumertest.LogsSink{}, map[pipeline.ID]consumer.Logs{}
		for id := range ids {
			sinks[id] = new(consumertest.LogsSink)
			m[id] = sinks[id]
		}
		conn, err := f.CreateLogsToLogs(ctx, set, c, connector.NewLogsRouter(m))
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.ConsumeLogs(ctx, in.Logs); err != nil {
			t.Fatal(err)
		}
		for id, s := range sinks {
			for _, l := range s.AllLogs() {
				out = append(out, Delivery{Pipeline: id.String(), Payload: telemetry.FromLogs(l)})
			}
		}
	case pipeline.SignalTraces:
		sinks, m := map[pipeline.ID]*consumertest.TracesSink{}, map[pipeline.ID]consumer.Traces{}
		for id := range ids {
			sinks[id] = new(consumertest.TracesSink)
			m[id] = sinks[id]
		}
		conn, err := f.CreateTracesToTraces(ctx, set, c, connector.NewTracesRouter(m))
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.ConsumeTraces(ctx, in.Traces); err != nil {
			t.Fatal(err)
		}
		for id, s := range sinks {
			for _, tr := range s.AllTraces() {
				out = append(out, Delivery{Pipeline: id.String(), Payload: telemetry.FromTraces(tr)})
			}
		}
	}
	return out
}

func TestRoutingExplainsTheRuleThatRoutedEachRecord(t *testing.T) {
	c := routingConfig(t, "samples/log-routing/config.yaml", "")
	in := load(t, "samples/log-routing/input.json")
	got, err := Explain(context.Background(), "routing", c, 4, in, "", deliver(t, c, in))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.RoutingVerified {
		t.Fatalf("verification = %s, notes %v", got.Verification, got.Notes)
	}
	if len(got.Rules) != 2 || got.Rules[0].Action != "copy" || got.Rules[1].Action != "move" ||
		got.Rules[0].Condition != "log.severity_number >= SEVERITY_NUMBER_ERROR" || !slices.Equal(got.Rules[0].Pipelines, []string{"logs/pager"}) {
		t.Fatalf("rules = %+v", got.Rules)
	}
	if !slices.Equal(got.DefaultPipelines, []string{"logs/archive"}) {
		t.Fatalf("default pipelines = %v", got.DefaultPipelines)
	}
	// The error is copied to the pager and continues to the default; the
	// audit record moves; the rest go to the default.
	want := []analysis.RoutedRecord{
		{Ref: "resource[0].scope[0].log_record[0]", Rules: []int{0}, Default: true},
		{Ref: "resource[0].scope[0].log_record[1]", Default: true},
		{Ref: "resource[0].scope[0].log_record[2]", Default: true},
		{Ref: "resource[1].scope[0].log_record[0]", Rules: []int{1}},
	}
	if len(got.Records) != len(want) {
		t.Fatalf("records = %+v", got.Records)
	}
	for i, w := range want {
		g := got.Records[i]
		if g.Ref != w.Ref || !slices.Equal(g.Rules, w.Rules) || g.Default != w.Default {
			t.Errorf("record %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestRoutingDoesNotAttributeWhenTheReplayDiffers(t *testing.T) {
	c := routingConfig(t, "samples/log-routing/config.yaml", "")
	in := load(t, "samples/log-routing/input.json")
	observed := deliver(t, c, in)
	observed = slices.DeleteFunc(observed, func(d Delivery) bool { return d.Pipeline == "logs/pager" })
	got, err := Explain(context.Background(), "routing", c, 4, in, "", observed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.RoutingDiffers || got.Records != nil || len(got.Notes) != 1 {
		t.Fatalf("got %s records %v notes %v", got.Verification, got.Records, got.Notes)
	}
}

func TestRoutingExplainsSpansByTheirOwnContext(t *testing.T) {
	c := routingConfig(t, "", `receivers: {otlp: {protocols: {grpc: {}}}}
connectors:
  routing:
    default_pipelines: [traces/main]
    table:
      - context: span
        condition: name == "GET /health"
        pipelines: [traces/health]
exporters: {nop: {}}
service:
  pipelines:
    traces/in: {receivers: [otlp], exporters: [routing]}
    traces/main: {receivers: [routing], exporters: [nop]}
    traces/health: {receivers: [routing], exporters: [nop]}
`)
	in := load(t, "samples/trace-sampling/input.json")
	got, err := Explain(context.Background(), "routing", c, 4, in, "", deliver(t, c, in))
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.RoutingVerified {
		t.Fatalf("verification = %s, notes %v", got.Verification, got.Notes)
	}
	matched, def := 0, 0
	for _, r := range got.Records {
		if len(r.Rules) > 0 {
			matched++
			if r.Default {
				t.Errorf("a moved span also went to the default: %+v", r)
			}
		}
		if r.Default {
			def++
		}
	}
	if matched == 0 || def == 0 || matched+def != len(got.Records) {
		t.Fatalf("matched %d, default %d of %d: %+v", matched, def, len(got.Records), got.Records)
	}
}

func TestRoutingCannotReplayRulesThatReadClientMetadata(t *testing.T) {
	c := routingConfig(t, "", `receivers: {otlp: {protocols: {grpc: {}}}}
connectors:
  routing:
    default_pipelines: [logs/main]
    table:
      - context: request
        condition: request["tenant"] == "a"
        pipelines: [logs/a]
exporters: {nop: {}}
service:
  pipelines:
    logs/in: {receivers: [otlp], exporters: [routing]}
    logs/main: {receivers: [routing], exporters: [nop]}
    logs/a: {receivers: [routing], exporters: [nop]}
`)
	got, err := Explain(context.Background(), "routing", c, 4, load(t, "samples/log-routing/input.json"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Verification != analysis.RoutingUnsupported || got.Records != nil || len(got.Notes) != 1 {
		t.Fatalf("got %s records %v notes %v", got.Verification, got.Records, got.Notes)
	}
}
