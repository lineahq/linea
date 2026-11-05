// Package aggregation explains aggregating connectors: which input records
// each emitted data point aggregates. Like ottldebug, it replays the
// connector's configuration on a recorded stage input with the upstream
// OTTL APIs and checks the replay against the connector's actual output.
// The output is the observation; the attribution is reported as verified
// only when the replay reproduces every emitted data point exactly.
package aggregation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/countconnector"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/ottlfuncs"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// The explanation is an optional feature: the generated registry
// imports this package only when the manifest has the count connector.
func init() { features.ProvideCount(Count) }

// Count explains one input of a count connector (contrib v0.162.0,
// connector.go and counter.go). The replay mirrors the connector: per
// resource, every configured metric is evaluated on every record, span
// event, metric or data point; a record is counted when every configured
// attribute resolves (record, then scope, then resource attributes, then
// the default value) and the conditions match. Each resource with counts
// becomes one output resource with one scope. Any condition error makes the
// connector return an error and emit nothing.
//
// output is the payload delivered for the input (nil when there was none)
// and outputSeq its event.
func Count(ctx context.Context, connector string, cfg component.Config, inputSeq int, input telemetry.Payload, outputSeq int, output *telemetry.Payload) (*analysis.Aggregation, error) {
	c, ok := cfg.(*countconnector.Config)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected configuration %T", connector, cfg)
	}
	agg := &analysis.Aggregation{Connector: connector, Input: inputSeq, Output: outputSeq, Points: []analysis.AggregatedPoint{}}
	if output != nil {
		agg.Points = observed(*output)
	}

	t := &tally{}
	var replayErr error
	var err error
	switch input.Signal {
	case pipeline.SignalLogs:
		replayErr, err = countLogs(ctx, c, input, t)
	case pipeline.SignalTraces:
		replayErr, err = countTraces(ctx, c, input, t)
	case pipeline.SignalMetrics:
		replayErr, err = countMetrics(ctx, c, input, t)
	default:
		err = fmt.Errorf("signal %s is not counted", input.Signal)
	}
	if err != nil {
		agg.Verification = analysis.AggregationUnsupported
		agg.Notes = append(agg.Notes, err.Error())
		return agg, nil
	}

	switch {
	case replayErr != nil && output == nil:
		agg.Verification = analysis.AggregationVerified
		agg.Notes = append(agg.Notes, "the connector returned an error and emitted nothing: "+replayErr.Error())
	case replayErr != nil:
		agg.Verification = analysis.AggregationDiffers
		agg.Notes = append(agg.Notes, "the replay failed but the connector emitted: "+replayErr.Error())
	case output == nil:
		agg.Verification = analysis.AggregationDiffers
		agg.Notes = append(agg.Notes, "the replay counted, but no output was observed")
	default:
		if diff := t.verify(*output, agg.Points); diff != "" {
			agg.Verification = analysis.AggregationDiffers
			agg.Notes = append(agg.Notes, diff)
			return agg, nil
		}
		agg.Verification = analysis.AggregationVerified
		agg.Uncounted = uncounted(input, t.counted)
	}
	return agg, nil
}

// observed lists the data points of a count connector output.
func observed(p telemetry.Payload) []analysis.AggregatedPoint {
	var out []analysis.AggregatedPoint
	for i, rm := range p.Metrics.ResourceMetrics().All() {
		for j, sm := range rm.ScopeMetrics().All() {
			for m, metric := range sm.Metrics().All() {
				if metric.Type() != pmetric.MetricTypeSum {
					continue
				}
				for k, dp := range metric.Sum().DataPoints().All() {
					out = append(out, analysis.AggregatedPoint{
						Ref:        fmt.Sprintf("resource[%d].scope[%d].metric[%d].data_point[%d]", i, j, m, k),
						Metric:     metric.Name(),
						Attributes: render(dp.Attributes()),
						Value:      dp.IntValue(),
					})
				}
			}
		}
	}
	return out
}

func render(m pcommon.Map) map[string]string {
	if m.Len() == 0 {
		return nil
	}
	out := make(map[string]string, m.Len())
	for k, v := range m.All() {
		out[k] = v.AsString()
	}
	return out
}

// tally is the replay's counts: one entry per input resource.
type tally struct {
	resources []map[string]*count
	counted   map[string]bool
}

type count struct {
	metric  string
	attrs   pcommon.Map
	records []string
}

func key(metric string, attrs pcommon.Map) string {
	return metric + "\x00" + telemetry.Fingerprint(attrs)
}

func (t *tally) resource() { t.resources = append(t.resources, map[string]*count{}) }

func (t *tally) add(metric string, attrs pcommon.Map, ref string) {
	r := t.resources[len(t.resources)-1]
	k := key(metric, attrs)
	if r[k] == nil {
		r[k] = &count{metric: metric, attrs: attrs}
	}
	r[k].records = append(r[k].records, ref)
	if t.counted == nil {
		t.counted = map[string]bool{}
	}
	t.counted[ref] = true
}

// verify compares the replayed counts with the observed points and, when
// they agree, attributes the records to the points. It returns a
// description of the first difference.
func (t *tally) verify(output telemetry.Payload, points []analysis.AggregatedPoint) string {
	var want []map[string]*count
	for _, r := range t.resources {
		if len(r) > 0 {
			want = append(want, r)
		}
	}
	rms := output.Metrics.ResourceMetrics()
	if rms.Len() != len(want) {
		return fmt.Sprintf("the replay counted in %d resource(s), the connector emitted %d", len(want), rms.Len())
	}
	byRef := map[string]int{}
	for i, p := range points {
		byRef[p.Ref] = i
	}
	seen := map[string]bool{}
	for i, rm := range rms.All() {
		for j, sm := range rm.ScopeMetrics().All() {
			for m, metric := range sm.Metrics().All() {
				if metric.Type() != pmetric.MetricTypeSum {
					return fmt.Sprintf("metric %q is not a sum", metric.Name())
				}
				for k, dp := range metric.Sum().DataPoints().All() {
					c := want[i][key(metric.Name(), dp.Attributes())]
					if c == nil {
						return fmt.Sprintf("the replay did not count %s%s", metric.Name(), label(dp.Attributes()))
					}
					if int64(len(c.records)) != dp.IntValue() {
						return fmt.Sprintf("%s%s: the replay counted %d, the connector %d", metric.Name(), label(dp.Attributes()), len(c.records), dp.IntValue())
					}
					ref := fmt.Sprintf("resource[%d].scope[%d].metric[%d].data_point[%d]", i, j, m, k)
					points[byRef[ref]].Records = c.records
					seen[fmt.Sprintf("%d\x00%s", i, key(metric.Name(), dp.Attributes()))] = true
				}
			}
		}
	}
	for i, r := range want {
		for k, c := range r {
			if !seen[fmt.Sprintf("%d\x00%s", i, k)] {
				return fmt.Sprintf("the replay counted %s%s, the connector emitted no such point", c.metric, label(c.attrs))
			}
		}
	}
	return ""
}

func label(m pcommon.Map) string {
	if m.Len() == 0 {
		return ""
	}
	kv := make([]string, 0, m.Len())
	for k, v := range m.All() {
		kv = append(kv, k+"="+v.AsString())
	}
	slices.Sort(kv)
	return "{" + strings.Join(kv, ", ") + "}"
}

// uncounted lists the input records no point counted.
func uncounted(input telemetry.Payload, counted map[string]bool) []string {
	var out []string
	for _, r := range telemetry.Records(input) {
		ref := r.Ref(input.Signal)
		hit := false
		for c := range counted {
			if analysis.Contains(c, ref) {
				hit = true
				break
			}
		}
		if !hit {
			out = append(out, ref)
		}
	}
	return out
}

// def is one configured count metric.
type def[K any] struct {
	name  string
	attrs []countconnector.AttributeConfig
	cond  *ottl.ConditionSequence[K]
}

var settings = componenttest.NewNopTelemetrySettings()

// defs compiles the metrics of one section, sorted by name. The
// connector iterates them in map order; the order changes nothing counted.
func defs[K any](section string, infos map[string]countconnector.MetricInfo, parser ottl.Parser[K], context string,
	sequence func([]*ottl.Condition[K]) ottl.ConditionSequence[K],
) ([]def[K], error) {
	var out []def[K]
	for name, info := range infos {
		d := def[K]{name: name, attrs: info.Attributes}
		if len(info.Conditions) > 0 {
			pc, err := ottl.NewParserCollection(settings, ottl.WithParserCollectionContext(context, &parser,
				ottl.WithConditionConverter(func(_ *ottl.ParserCollection[*ottl.ConditionSequence[K]], _ ottl.ConditionsGetter, parsed []*ottl.Condition[K]) (*ottl.ConditionSequence[K], error) {
					cs := sequence(parsed)
					return &cs, nil
				})))
			if err != nil {
				return nil, err
			}
			if d.cond, err = pc.ParseConditionsWithContext(context, ottl.NewConditionsGetter(info.Conditions), true); err != nil {
				return nil, fmt.Errorf("%s metric %q: the replay cannot parse its conditions: %w", section, name, err)
			}
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b def[K]) int { return cmp.Compare(a.name, b.name) })
	return out, nil
}

func converters[K any]() map[string]ottl.Factory[K] {
	return ottlcompat.Functions(ottlfuncs.StandardConverters[K]())
}

// update evaluates every metric of a section on one item, like
// counter.update.
func update[K any](ctx context.Context, t *tally, defs []def[K], ref string, attrs, scope, resource pcommon.Map, tCtx K) error {
	var errs error
	for _, d := range defs {
		dims := pcommon.NewMap()
		for _, a := range d.attrs {
			if v, ok := dimension(a, attrs, scope, resource); ok {
				v.CopyTo(dims.PutEmpty(a.Key))
			}
		}
		// The connector skips an item missing any configured attribute.
		if dims.Len() != len(d.attrs) {
			continue
		}
		if d.cond != nil {
			match, err := d.cond.Eval(ctx, tCtx)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if !match {
				continue
			}
		}
		t.add(d.name, dims, ref)
	}
	return errs
}

// dimension resolves a configured attribute like pdatautil.GetDimensionValue.
func dimension(a countconnector.AttributeConfig, maps ...pcommon.Map) (pcommon.Value, bool) {
	for _, m := range maps {
		if v, ok := m.Get(a.Key); ok {
			return v, true
		}
	}
	if a.DefaultValue != nil {
		v := pcommon.NewValueEmpty()
		if err := v.FromRaw(a.DefaultValue); err == nil {
			return v, true
		}
	}
	return pcommon.Value{}, false
}

func countLogs(ctx context.Context, c *countconnector.Config, p telemetry.Payload, t *tally) (replayErr, err error) {
	parser, err := ottllog.NewParser(converters[*ottllog.TransformContext](), settings, ottllog.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	logs, err := defs("logs", c.Logs, parser, ottllog.ContextName, func(cs []*ottl.Condition[*ottllog.TransformContext]) ottl.ConditionSequence[*ottllog.TransformContext] {
		return ottllog.NewConditionSequence(cs, settings, ottllog.WithConditionSequenceErrorMode(ottl.PropagateError))
	})
	if err != nil {
		return nil, err
	}
	for i, rl := range p.Logs.ResourceLogs().All() {
		t.resource()
		for j, sl := range rl.ScopeLogs().All() {
			for k, lr := range sl.LogRecords().All() {
				tCtx := ottlcompat.NewLogContext(rl, sl, lr)
				ref := fmt.Sprintf("resource[%d].scope[%d].log_record[%d]", i, j, k)
				replayErr = errors.Join(replayErr, update(ctx, t, logs, ref, lr.Attributes(), sl.Scope().Attributes(), rl.Resource().Attributes(), tCtx))
				tCtx.Close()
			}
		}
	}
	return replayErr, nil
}

func countTraces(ctx context.Context, c *countconnector.Config, p telemetry.Payload, t *tally) (replayErr, err error) {
	// filterottl.StandardSpanFuncs adds IsRootSpan to the converters.
	isRoot := ottlfuncs.NewIsRootSpanFactory()
	fns := converters[*ottlspan.TransformContext]()
	fns[isRoot.Name()] = isRoot
	sp, err := ottlspan.NewParser(fns, settings, ottlspan.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	spans, err := defs("spans", c.Spans, sp, ottlspan.ContextName, func(cs []*ottl.Condition[*ottlspan.TransformContext]) ottl.ConditionSequence[*ottlspan.TransformContext] {
		return ottlspan.NewConditionSequence(cs, settings, ottlspan.WithConditionSequenceErrorMode(ottl.PropagateError))
	})
	if err != nil {
		return nil, err
	}
	ep, err := ottlspanevent.NewParser(converters[*ottlspanevent.TransformContext](), settings, ottlspanevent.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	events, err := defs("spanevents", c.SpanEvents, ep, ottlspanevent.ContextName, func(cs []*ottl.Condition[*ottlspanevent.TransformContext]) ottl.ConditionSequence[*ottlspanevent.TransformContext] {
		return ottlspanevent.NewConditionSequence(cs, settings, ottlspanevent.WithConditionSequenceErrorMode(ottl.PropagateError))
	})
	if err != nil {
		return nil, err
	}
	for i, rs := range p.Traces.ResourceSpans().All() {
		t.resource()
		res := rs.Resource().Attributes()
		for j, ss := range rs.ScopeSpans().All() {
			scope := ss.Scope().Attributes()
			for k, span := range ss.Spans().All() {
				ref := fmt.Sprintf("resource[%d].scope[%d].span[%d]", i, j, k)
				sCtx := ottlcompat.NewSpanContext(rs, ss, span)
				replayErr = errors.Join(replayErr, update(ctx, t, spans, ref, span.Attributes(), scope, res, sCtx))
				sCtx.Close()
				for e, ev := range span.Events().All() {
					eCtx := ottlcompat.NewSpanEventContext(rs, ss, span, ev)
					replayErr = errors.Join(replayErr, update(ctx, t, events, fmt.Sprintf("%s.event[%d]", ref, e), ev.Attributes(), scope, res, eCtx))
					eCtx.Close()
				}
			}
		}
	}
	return replayErr, nil
}

func countMetrics(ctx context.Context, c *countconnector.Config, p telemetry.Payload, t *tally) (replayErr, err error) {
	// filterottl.StandardMetricFuncs adds HasAttrOnDatapoint and
	// HasAttrKeyOnDatapoint, which are internal to contrib; conditions
	// using them do not parse and are reported as unsupported.
	mp, err := ottlmetric.NewParser(converters[*ottlmetric.TransformContext](), settings, ottlmetric.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	metrics, err := defs("metrics", c.Metrics, mp, ottlmetric.ContextName, func(cs []*ottl.Condition[*ottlmetric.TransformContext]) ottl.ConditionSequence[*ottlmetric.TransformContext] {
		return ottlmetric.NewConditionSequence(cs, settings, ottlmetric.WithConditionSequenceErrorMode(ottl.PropagateError))
	})
	if err != nil {
		return nil, err
	}
	dp, err := ottldatapoint.NewParser(converters[*ottldatapoint.TransformContext](), settings, ottldatapoint.EnablePathContextNames())
	if err != nil {
		return nil, err
	}
	points, err := defs("datapoints", c.DataPoints, dp, ottldatapoint.ContextName, func(cs []*ottl.Condition[*ottldatapoint.TransformContext]) ottl.ConditionSequence[*ottldatapoint.TransformContext] {
		return ottldatapoint.NewConditionSequence(cs, settings, ottldatapoint.WithConditionSequenceErrorMode(ottl.PropagateError))
	})
	if err != nil {
		return nil, err
	}
	for i, rm := range p.Metrics.ResourceMetrics().All() {
		t.resource()
		res := rm.Resource().Attributes()
		for j, sm := range rm.ScopeMetrics().All() {
			scope := sm.Scope().Attributes()
			for m, metric := range sm.Metrics().All() {
				ref := fmt.Sprintf("resource[%d].scope[%d].metric[%d]", i, j, m)
				mCtx := ottlcompat.NewMetricContext(rm, sm, metric)
				replayErr = errors.Join(replayErr, update(ctx, t, metrics, ref, pcommon.NewMap(), scope, res, mCtx))
				mCtx.Close()
				if metric.Type() == pmetric.MetricTypeEmpty {
					replayErr = errors.Join(replayErr, fmt.Errorf("metric %q: invalid metric type: %v", metric.Name(), metric.Type()))
					continue
				}
				for k, point := range dataPoints(metric) {
					dCtx := ottlcompat.NewDataPointContext(rm, sm, metric, point.value)
					replayErr = errors.Join(replayErr, update(ctx, t, points, fmt.Sprintf("%s.data_point[%d]", ref, k), point.attrs, scope, res, dCtx))
					dCtx.Close()
				}
			}
		}
	}
	return replayErr, nil
}

type dataPoint struct {
	value any
	attrs pcommon.Map
}

func dataPoints(m pmetric.Metric) []dataPoint {
	var out []dataPoint
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for _, p := range m.Gauge().DataPoints().All() {
			out = append(out, dataPoint{p, p.Attributes()})
		}
	case pmetric.MetricTypeSum:
		for _, p := range m.Sum().DataPoints().All() {
			out = append(out, dataPoint{p, p.Attributes()})
		}
	case pmetric.MetricTypeHistogram:
		for _, p := range m.Histogram().DataPoints().All() {
			out = append(out, dataPoint{p, p.Attributes()})
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, p := range m.ExponentialHistogram().DataPoints().All() {
			out = append(out, dataPoint{p, p.Attributes()})
		}
	case pmetric.MetricTypeSummary:
		for _, p := range m.Summary().DataPoints().All() {
			out = append(out, dataPoint{p, p.Attributes()})
		}
	}
	return out
}
