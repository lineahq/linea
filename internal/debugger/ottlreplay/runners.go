package ottlreplay

import (
	"context"
	"fmt"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlexemplar"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlscope"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/analysis"
	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// The runners mirror the iteration of the transform processor v0.162.0
// (internal/common/{traces,logs,metrics}.go): resource, then scope, then
// item, in payload order. Where the processor iterates by index and
// re-reads the length, so do the runners: a metric statement that appends
// metrics (copy_metric, extract_*) also runs on the appended metrics.

// snapshot captures an entity's state before a statement and returns a
// function producing the changes made since.
type snapshot = func() func() []analysis.Change

func resourceEntity(i int) string     { return fmt.Sprintf("resource[%d]", i) }
func scopeEntity(i, j int) string     { return fmt.Sprintf("resource[%d].scope[%d]", i, j) }
func metricEntity(i, j, m int) string { return fmt.Sprintf("%s.metric[%d]", scopeEntity(i, j), m) }

// around snapshots the resource and scope an item belongs to, plus the item
// itself when item is not nil. Statements in item contexts may write to
// their resource and scope too.
func around(i, j int, res pcommon.Resource, scope pcommon.InstrumentationScope, item snapshot) snapshot {
	return func() func() []analysis.Change {
		br, bs := pcommon.NewResource(), pcommon.NewInstrumentationScope()
		res.CopyTo(br)
		scope.CopyTo(bs)
		var rest func() []analysis.Change
		if item != nil {
			rest = item()
		}
		return func() []analysis.Change {
			out := analysis.DiffResource(resourceEntity(i), br, res)
			out = append(out, analysis.DiffScope(scopeEntity(i, j), bs, scope)...)
			if rest != nil {
				out = append(out, rest()...)
			}
			return out
		}
	}
}

// group is a resource or a scope with the item carrying its schema URL.
type group struct {
	resource pcommon.Resource
	scope    pcommon.InstrumentationScope
	url      ottlcompat.SchemaURLItem
}

// resources returns the payload's resources and, for each, its scopes.
// Resource and scope statements cannot add or remove either.
func resources(data telemetry.Payload) ([]group, [][]group) {
	var res []group
	var scopes [][]group
	switch data.Signal {
	case pipeline.SignalTraces:
		for _, rs := range data.Traces.ResourceSpans().All() {
			res = append(res, group{resource: rs.Resource(), url: rs})
			var ss []group
			for _, s := range rs.ScopeSpans().All() {
				ss = append(ss, group{scope: s.Scope(), url: s})
			}
			scopes = append(scopes, ss)
		}
	case pipeline.SignalLogs:
		for _, rl := range data.Logs.ResourceLogs().All() {
			res = append(res, group{resource: rl.Resource(), url: rl})
			var ss []group
			for _, s := range rl.ScopeLogs().All() {
				ss = append(ss, group{scope: s.Scope(), url: s})
			}
			scopes = append(scopes, ss)
		}
	case pipeline.SignalMetrics:
		for _, rm := range data.Metrics.ResourceMetrics().All() {
			res = append(res, group{resource: rm.Resource(), url: rm})
			var ss []group
			for _, s := range rm.ScopeMetrics().All() {
				ss = append(ss, group{scope: s.Scope(), url: s})
			}
			scopes = append(scopes, ss)
		}
	}
	return res, scopes
}

func runResource(ctx context.Context, data telemetry.Payload, seq *sequence[*ottlresource.TransformContext], g *Group) error {
	res, _ := resources(data)
	for i, r := range res {
		entity := resourceEntity(i)
		tCtx := ottlcompat.NewResourceContext(r.resource, r.url)
		err := seq.execute(ctx, tCtx, entity, g, func() func() []analysis.Change {
			before := pcommon.NewResource()
			r.resource.CopyTo(before)
			return func() []analysis.Change { return analysis.DiffResource(entity, before, r.resource) }
		})
		tCtx.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func runScope(ctx context.Context, data telemetry.Payload, seq *sequence[*ottlscope.TransformContext], g *Group) error {
	res, scopes := resources(data)
	for i, r := range res {
		for j, s := range scopes[i] {
			tCtx := ottlcompat.NewScopeContext(s.scope, r.resource, s.url, r.url)
			err := seq.execute(ctx, tCtx, scopeEntity(i, j), g, around(i, j, r.resource, s.scope, nil))
			tCtx.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func runSpan(ctx context.Context, data telemetry.Payload, seq *sequence[*ottlspan.TransformContext], g *Group) error {
	for i, rs := range data.Traces.ResourceSpans().All() {
		for j, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				entity := analysis.SpanEntity(span)
				tCtx := ottlcompat.NewSpanContext(rs, ss, span)
				err := seq.execute(ctx, tCtx, entity, g, around(i, j, rs.Resource(), ss.Scope(), func() func() []analysis.Change {
					before := ptrace.NewSpan()
					span.CopyTo(before)
					return func() []analysis.Change { return analysis.DiffSpan(entity, before, span) }
				}))
				tCtx.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func runLog(ctx context.Context, data telemetry.Payload, seq *sequence[*ottllog.TransformContext], g *Group) error {
	rls := data.Logs.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			records := sl.LogRecords()
			for k := 0; k < records.Len(); k++ {
				lr := records.At(k)
				entity := fmt.Sprintf("%s.log_record[%d]", scopeEntity(i, j), k)
				tCtx := ottlcompat.NewLogContext(rl, sl, lr)
				err := seq.execute(ctx, tCtx, entity, g, around(i, j, rl.Resource(), sl.Scope(), func() func() []analysis.Change {
					before := plog.NewLogRecord()
					lr.CopyTo(before)
					return func() []analysis.Change { return analysis.DiffLogRecord(entity, before, lr) }
				}))
				tCtx.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func runMetric(ctx context.Context, data telemetry.Payload, seq *sequence[*ottlmetric.TransformContext], g *Group) error {
	rms := data.Metrics.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			metrics := sm.Metrics()
			for k := 0; k < metrics.Len(); k++ {
				tCtx := ottlcompat.NewMetricContext(rm, sm, metrics.At(k))
				// The whole metric list is the item: metric functions may
				// append metrics to the scope.
				err := seq.execute(ctx, tCtx, metricEntity(i, j, k), g, around(i, j, rm.Resource(), sm.Scope(), func() func() []analysis.Change {
					before := pmetric.NewMetricSlice()
					metrics.CopyTo(before)
					return func() []analysis.Change { return analysis.DiffMetrics(scopeEntity(i, j), before, metrics) }
				}))
				tCtx.Close()
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// metricItem snapshots one metric, which data point and exemplar statements
// may change beyond their own point (metric.name, for example).
func metricItem(entity string, metric pmetric.Metric) snapshot {
	return func() func() []analysis.Change {
		before := pmetric.NewMetric()
		metric.CopyTo(before)
		return func() []analysis.Change { return analysis.DiffMetric(entity, before, metric) }
	}
}

func runDataPoint(ctx context.Context, data telemetry.Payload, seq *sequence[*ottldatapoint.TransformContext], g *Group) error {
	rms := data.Metrics.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			metrics := sm.Metrics()
			for k := 0; k < metrics.Len(); k++ {
				metric := metrics.At(k)
				entity := metricEntity(i, j, k)
				points := func(n int, at func(p int) any) error {
					for p := 0; p < n; p++ {
						tCtx := ottlcompat.NewDataPointContext(rm, sm, metric, at(p))
						err := seq.execute(ctx, tCtx, fmt.Sprintf("%s.data_point[%d]", entity, p), g,
							around(i, j, rm.Resource(), sm.Scope(), metricItem(entity, metric)))
						tCtx.Close()
						if err != nil {
							return err
						}
					}
					return nil
				}
				var err error
				switch metric.Type() {
				case pmetric.MetricTypeSum:
					dps := metric.Sum().DataPoints()
					err = points(dps.Len(), func(p int) any { return dps.At(p) })
				case pmetric.MetricTypeGauge:
					dps := metric.Gauge().DataPoints()
					err = points(dps.Len(), func(p int) any { return dps.At(p) })
				case pmetric.MetricTypeHistogram:
					dps := metric.Histogram().DataPoints()
					err = points(dps.Len(), func(p int) any { return dps.At(p) })
				case pmetric.MetricTypeExponentialHistogram:
					dps := metric.ExponentialHistogram().DataPoints()
					err = points(dps.Len(), func(p int) any { return dps.At(p) })
				case pmetric.MetricTypeSummary:
					dps := metric.Summary().DataPoints()
					err = points(dps.Len(), func(p int) any { return dps.At(p) })
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// exemplarHolder is a data point type that carries exemplars.
type exemplarHolder interface {
	Exemplars() pmetric.ExemplarSlice
}

func runExemplar(ctx context.Context, data telemetry.Payload, seq *sequence[*ottlexemplar.TransformContext], g *Group) error {
	for i, rm := range data.Metrics.ResourceMetrics().All() {
		for j, sm := range rm.ScopeMetrics().All() {
			for k, metric := range sm.Metrics().All() {
				entity := metricEntity(i, j, k)
				var points []exemplarHolder
				switch metric.Type() {
				case pmetric.MetricTypeSum:
					for _, dp := range metric.Sum().DataPoints().All() {
						points = append(points, dp)
					}
				case pmetric.MetricTypeGauge:
					for _, dp := range metric.Gauge().DataPoints().All() {
						points = append(points, dp)
					}
				case pmetric.MetricTypeHistogram:
					for _, dp := range metric.Histogram().DataPoints().All() {
						points = append(points, dp)
					}
				case pmetric.MetricTypeExponentialHistogram:
					for _, dp := range metric.ExponentialHistogram().DataPoints().All() {
						points = append(points, dp)
					}
				case pmetric.MetricTypeEmpty, pmetric.MetricTypeSummary:
				}
				for p, dp := range points {
					for e, exemplar := range dp.Exemplars().All() {
						tCtx := ottlcompat.NewExemplarContext(rm, sm, metric, dp, exemplar)
						err := seq.execute(ctx, tCtx, fmt.Sprintf("%s.data_point[%d].exemplar[%d]", entity, p, e), g,
							around(i, j, rm.Resource(), sm.Scope(), metricItem(entity, metric)))
						tCtx.Close()
						if err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}
