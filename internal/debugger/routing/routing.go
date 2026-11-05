// Package routing explains routing connectors: which rule of the routing
// table sent each input record where. Like aggregation, it replays the
// connector on a recorded stage input, here with the real connector, and
// checks the replay against what the connector actually delivered. The
// recorded deliveries are the observation; the attribution is reported as
// verified only when the replay reproduces them exactly.
//
// To tell the rules apart the replay rewrites the table so that every rule
// delivers to a pipeline of its own, and the default pipelines to another.
// Matching, move and copy semantics are the connector's own.
package routing

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/routingconnector"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Delivery is what the connector delivered into one pipeline for the input.
type Delivery = features.Delivery

// The explanation is an optional feature: the generated registry
// imports this package only when the manifest has the routing connector.
func init() { features.ProvideRouting(Explain) }

const defaultSink = "default"

// Explain explains one input of a routing connector (contrib v0.162.0,
// logs.go, metrics.go, traces.go and router.go). inputError is the error
// the connector returned for the input, if any; deliveries are what it
// delivered.
func Explain(ctx context.Context, name string, cfg component.Config, inputSeq int, input telemetry.Payload, inputError string, deliveries []Delivery) (*analysis.Routing, error) {
	c, ok := cfg.(*routingconnector.Config)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected configuration %T", name, cfg)
	}
	out := &analysis.Routing{Connector: name, Input: inputSeq, Rules: rules(c)}
	for _, p := range c.DefaultPipelines {
		out.DefaultPipelines = append(out.DefaultPipelines, p.String())
	}
	if why := unsupported(c); why != "" {
		out.Verification = analysis.RoutingUnsupported
		out.Notes = append(out.Notes, why)
		return out, nil
	}

	sinks, replayErr, err := replay(ctx, c, input)
	if err != nil {
		out.Verification = analysis.RoutingUnsupported
		out.Notes = append(out.Notes, err.Error())
		return out, nil
	}
	if replayErr != nil {
		if inputError != "" {
			out.Verification = analysis.RoutingVerified
			out.Notes = append(out.Notes, "the connector returned an error for this input: "+replayErr.Error())
		} else {
			out.Verification = analysis.RoutingDiffers
			out.Notes = append(out.Notes, "the replay failed but the connector delivered: "+replayErr.Error())
		}
		return out, nil
	}
	if inputError != "" {
		out.Verification = analysis.RoutingDiffers
		out.Notes = append(out.Notes, "the connector returned an error for this input, but the replay did not: "+inputError)
		return out, nil
	}
	if diff := verify(c, sinks, deliveries); diff != "" {
		out.Verification = analysis.RoutingDiffers
		out.Notes = append(out.Notes, diff)
		return out, nil
	}
	out.Verification = analysis.RoutingVerified
	out.Records = attribute(input, len(c.Table), sinks)
	return out, nil
}

func rules(c *routingconnector.Config) []analysis.RoutingRule {
	out := make([]analysis.RoutingRule, 0, len(c.Table))
	for i, item := range c.Table {
		r := analysis.RoutingRule{Index: i, Context: item.Context, Condition: item.Condition, Action: string(item.Action)}
		if r.Condition == "" {
			r.Condition = item.Statement
		}
		if r.Action == "" {
			r.Action = string(routingconnector.Move)
		}
		for _, p := range item.Pipelines {
			r.Pipelines = append(r.Pipelines, p.String())
		}
		out = append(out, r)
	}
	return out
}

// unsupported names a rule whose condition reads what a run does not
// record: the client metadata of the request that delivered the data.
func unsupported(c *routingconnector.Config) string {
	for i, item := range c.Table {
		text := item.Condition + item.Statement
		if item.Context == "request" || item.Context == "otelcol" || strings.Contains(text, "otelcol.") {
			return fmt.Sprintf("rule %d reads client request metadata, which a run does not record, so its decisions cannot be replayed", i)
		}
	}
	return ""
}

// sinkName is the pipeline the replay gives rule i.
func sinkName(i int) string { return fmt.Sprintf("rule%d", i) }

// rewritten is the configuration with every rule, and the default, delivering
// to a pipeline of their own.
func rewritten(c *routingconnector.Config, signal pipeline.Signal) *routingconnector.Config {
	cp := *c
	cp.Table = slices.Clone(c.Table)
	for i := range cp.Table {
		cp.Table[i].Pipelines = []pipeline.ID{pipeline.NewIDWithName(signal, sinkName(i))}
	}
	cp.DefaultPipelines = nil
	if len(c.DefaultPipelines) > 0 {
		cp.DefaultPipelines = []pipeline.ID{pipeline.NewIDWithName(signal, defaultSink)}
	}
	return &cp
}

// replay runs the real connector on the input and returns what each sink
// received. replayErr is the error the connector returned; err means the
// connector could not be built.
func replay(ctx context.Context, c *routingconnector.Config, input telemetry.Payload) (sinks map[string]telemetry.Payload, replayErr, err error) {
	signal := input.Signal
	cfg := rewritten(c, signal)
	names := []string{defaultSink}
	for i := range c.Table {
		names = append(names, sinkName(i))
	}
	set := connector.Settings{ID: component.MustNewID("routing"), TelemetrySettings: componenttest.NewNopTelemetrySettings(), BuildInfo: component.NewDefaultBuildInfo()}
	f := routingconnector.NewFactory()
	in := input.Clone()
	sinks = map[string]telemetry.Payload{}

	switch signal {
	case pipeline.SignalLogs:
		s := map[string]*consumertest.LogsSink{}
		m := map[pipeline.ID]consumer.Logs{}
		for _, n := range names {
			s[n] = new(consumertest.LogsSink)
			m[pipeline.NewIDWithName(signal, n)] = s[n]
		}
		conn, err := f.CreateLogsToLogs(ctx, set, cfg, connector.NewLogsRouter(m))
		if err != nil {
			return nil, nil, err
		}
		replayErr = conn.ConsumeLogs(ctx, in.Logs)
		for n, sink := range s {
			var ps []telemetry.Payload
			for _, l := range sink.AllLogs() {
				ps = append(ps, telemetry.FromLogs(l))
			}
			sinks[n] = concat(signal, ps)
		}
	case pipeline.SignalTraces:
		s := map[string]*consumertest.TracesSink{}
		m := map[pipeline.ID]consumer.Traces{}
		for _, n := range names {
			s[n] = new(consumertest.TracesSink)
			m[pipeline.NewIDWithName(signal, n)] = s[n]
		}
		conn, err := f.CreateTracesToTraces(ctx, set, cfg, connector.NewTracesRouter(m))
		if err != nil {
			return nil, nil, err
		}
		replayErr = conn.ConsumeTraces(ctx, in.Traces)
		for n, sink := range s {
			var ps []telemetry.Payload
			for _, t := range sink.AllTraces() {
				ps = append(ps, telemetry.FromTraces(t))
			}
			sinks[n] = concat(signal, ps)
		}
	case pipeline.SignalMetrics:
		s := map[string]*consumertest.MetricsSink{}
		m := map[pipeline.ID]consumer.Metrics{}
		for _, n := range names {
			s[n] = new(consumertest.MetricsSink)
			m[pipeline.NewIDWithName(signal, n)] = s[n]
		}
		conn, err := f.CreateMetricsToMetrics(ctx, set, cfg, connector.NewMetricsRouter(m))
		if err != nil {
			return nil, nil, err
		}
		replayErr = conn.ConsumeMetrics(ctx, in.Metrics)
		for n, sink := range s {
			var ps []telemetry.Payload
			for _, md := range sink.AllMetrics() {
				ps = append(ps, telemetry.FromMetrics(md))
			}
			sinks[n] = concat(signal, ps)
		}
	default:
		return nil, nil, fmt.Errorf("signal %s is not routed", signal)
	}
	return sinks, replayErr, nil
}

func concat(signal pipeline.Signal, ps []telemetry.Payload) telemetry.Payload {
	if p, ok := telemetry.Concat(ps); ok {
		return p
	}
	return telemetry.Empty(signal)
}

// replayed returns, per configured pipeline, the fingerprints of the records
// the replay delivered into it: the rule sinks whose rule names the pipeline,
// and the default sink for the default pipelines.
func replayed(c *routingconnector.Config, sinks map[string]telemetry.Payload) map[string][]string {
	out := map[string][]string{}
	add := func(p pipeline.ID, sink string) {
		out[p.String()] = append(out[p.String()], telemetry.ContextFingerprints(sinks[sink])...)
	}
	for i, item := range c.Table {
		for _, p := range item.Pipelines {
			add(p, sinkName(i))
		}
	}
	for _, p := range c.DefaultPipelines {
		add(p, defaultSink)
	}
	return out
}

// verify compares the replay with the recorded deliveries pipeline by
// pipeline: the same records, in any order or grouping. It returns "" when
// they agree.
func verify(c *routingconnector.Config, sinks map[string]telemetry.Payload, deliveries []Delivery) string {
	want := replayed(c, sinks)
	got := map[string][]string{}
	for _, d := range deliveries {
		got[d.Pipeline] = append(got[d.Pipeline], telemetry.ContextFingerprints(d.Payload)...)
	}
	seen := map[string]bool{}
	var names []string
	for n := range want {
		seen[n] = true
		names = append(names, n)
	}
	for n := range got {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		w, g := slices.Clone(want[n]), slices.Clone(got[n])
		slices.Sort(w)
		slices.Sort(g)
		if !slices.Equal(w, g) {
			return fmt.Sprintf("the replay delivered %d record(s) into %s but the run delivered %d, so no record is attributed to a rule", len(w), n, len(g))
		}
	}
	return ""
}

// attribute assigns each sink's records to the input records with the same
// content, resource and scope. Routing decides by content, so identical
// records are interchangeable and are assigned in order.
func attribute(input telemetry.Payload, rules int, sinks map[string]telemetry.Payload) []analysis.RoutedRecord {
	recs := telemetry.Records(input)
	byPrint := map[string][]int{}
	for i, p := range telemetry.ContextFingerprints(input) {
		byPrint[p] = append(byPrint[p], i)
	}
	out := make([]analysis.RoutedRecord, len(recs))
	for i, r := range recs {
		out[i].Ref = r.Ref(input.Signal)
	}
	assign := func(sink string, do func(*analysis.RoutedRecord)) {
		used := map[string]int{}
		for _, p := range telemetry.ContextFingerprints(sinks[sink]) {
			idx := byPrint[p]
			if used[p] >= len(idx) {
				continue
			}
			do(&out[idx[used[p]]])
			used[p]++
		}
	}
	// Rules in table order, so each record lists its rules in order.
	for i := range rules {
		assign(sinkName(i), func(r *analysis.RoutedRecord) { r.Rules = append(r.Rules, i) })
	}
	assign(defaultSink, func(r *analysis.RoutedRecord) { r.Default = true })
	return out
}
