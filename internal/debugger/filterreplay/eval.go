package filterreplay

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/analysis"
	ottlcompat "github.com/lineahq/linea/internal/debugger/compat/ottl"
)

// node tracks the input position of an item while the evaluation removes
// items from a copy of the payload: resources hold scopes, scopes hold
// records or metrics, spans hold events and metrics hold data points.
type node struct {
	index int
	kids  []node
}

func nodes(n int) []node {
	out := make([]node, n)
	for i := range out {
		out[i].index = i
	}
	return out
}

// removeIf removes the items for which f returns true from s and their
// nodes from pos, in step. f gets the item's node, whose kids it may
// change.
func removeIf[T any](s interface{ RemoveIf(func(T) bool) }, pos *[]node, f func(T, *node) bool) {
	n := 0
	kept := (*pos)[:0:0]
	s.RemoveIf(func(x T) bool {
		p := (*pos)[n]
		n++
		if f(x, &p) {
			return true
		}
		kept = append(kept, p)
		return false
	})
	*pos = kept
}

// evaluation evaluates one input; it counts per condition and records each
// drop.
type evaluation struct {
	ctx        context.Context
	conditions []analysis.FilterCondition
	drops      []analysis.FilterDrop
}

// first evaluates an OR sequence the way ottl.ConditionSequence.Eval does
// (pkg/ottl v0.162.0): the first true condition decides; a failing
// condition is skipped unless the mode is propagate, which stops the
// sequence with the error. It returns the index of the true condition or
// -1.
func first[K interface{ Close() }](e *evaluation, es []entry[K], mode ottl.ErrorMode, tCtx K) (int, error) {
	defer tCtx.Close()
	for _, en := range es {
		row := &e.conditions[en.index]
		row.Evaluated++
		ok, err := en.cond.Eval(e.ctx, tCtx)
		if err != nil {
			row.Errors++
			if row.Error == "" {
				row.Error = err.Error()
			}
			if mode == ottl.PropagateError {
				return -1, fmt.Errorf("failed to eval condition: %v, %w", row.Condition, err)
			}
			continue
		}
		if ok {
			return en.index, nil
		}
	}
	return -1, nil
}

func (e *evaluation) drop(ref string, condition, records int) {
	row := &e.conditions[condition]
	row.Matched++
	row.Records += records
	e.drops = append(e.drops, analysis.FilterDrop{Ref: ref, Condition: condition, Records: records})
}

func resourceRef(r int) string { return fmt.Sprintf("resource[%d]", r) }
func scopeRef(r, s int) string { return fmt.Sprintf("resource[%d].scope[%d]", r, s) }
func itemRef(r, s int, kind string, i int) string {
	return fmt.Sprintf("resource[%d].scope[%d].%s[%d]", r, s, kind, i)
}

// logs applies one group to ld as filterprocessor's LogsConsumer does.
func (e *evaluation) logs(g *group, ld plog.Logs, pos *[]node) error {
	var errs error
	removeIf(ld.ResourceLogs(), pos, func(rl plog.ResourceLogs, rp *node) bool {
		if len(g.resource) > 0 {
			i, err := first(e, g.resource, g.mode, ottlcompat.NewResourceContext(rl.Resource(), rl))
			if err != nil {
				errs = errors.Join(errs, err)
				return false
			}
			if i >= 0 {
				e.drop(resourceRef(rp.index), i, logCount(rl))
				return true
			}
		}
		if len(g.scope) == 0 && len(g.log) == 0 {
			return rl.ScopeLogs().Len() == 0
		}
		removeIf(rl.ScopeLogs(), &rp.kids, func(sl plog.ScopeLogs, sp *node) bool {
			if len(g.scope) > 0 {
				i, err := first(e, g.scope, g.mode, ottlcompat.NewScopeContext(sl.Scope(), rl.Resource(), sl, rl))
				if err != nil {
					errs = errors.Join(errs, err)
					return false
				}
				if i >= 0 {
					e.drop(scopeRef(rp.index, sp.index), i, sl.LogRecords().Len())
					return true
				}
			}
			if len(g.log) > 0 {
				removeIf(sl.LogRecords(), &sp.kids, func(lr plog.LogRecord, lp *node) bool {
					i, err := first(e, g.log, g.mode, ottlcompat.NewLogContext(rl, sl, lr))
					if err != nil {
						errs = errors.Join(errs, err)
						return false
					}
					if i >= 0 {
						e.drop(itemRef(rp.index, sp.index, "log_record", lp.index), i, 1)
						return true
					}
					return false
				})
			}
			return sl.LogRecords().Len() == 0
		})
		return rl.ScopeLogs().Len() == 0
	})
	return errs
}

// traces applies one group to td as filterprocessor's TracesConsumer does.
func (e *evaluation) traces(g *group, td ptrace.Traces, pos *[]node) error {
	var errs error
	removeIf(td.ResourceSpans(), pos, func(rs ptrace.ResourceSpans, rp *node) bool {
		if len(g.resource) > 0 {
			i, err := first(e, g.resource, g.mode, ottlcompat.NewResourceContext(rs.Resource(), rs))
			if err != nil {
				errs = errors.Join(errs, err)
				return false
			}
			if i >= 0 {
				e.drop(resourceRef(rp.index), i, spanCount(rs))
				return true
			}
		}
		if len(g.scope) == 0 && len(g.span) == 0 && len(g.spanEvent) == 0 {
			return rs.ScopeSpans().Len() == 0
		}
		removeIf(rs.ScopeSpans(), &rp.kids, func(ss ptrace.ScopeSpans, sp *node) bool {
			if len(g.scope) > 0 {
				i, err := first(e, g.scope, g.mode, ottlcompat.NewScopeContext(ss.Scope(), rs.Resource(), ss, rs))
				if err != nil {
					errs = errors.Join(errs, err)
					return false
				}
				if i >= 0 {
					e.drop(scopeRef(rp.index, sp.index), i, ss.Spans().Len())
					return true
				}
			}
			if len(g.span) == 0 && len(g.spanEvent) == 0 {
				return ss.Spans().Len() == 0
			}
			removeIf(ss.Spans(), &sp.kids, func(span ptrace.Span, np *node) bool {
				ref := itemRef(rp.index, sp.index, "span", np.index)
				if len(g.span) > 0 {
					i, err := first(e, g.span, g.mode, ottlcompat.NewSpanContext(rs, ss, span))
					if err != nil {
						errs = errors.Join(errs, err)
						return false
					}
					if i >= 0 {
						e.drop(ref, i, 1)
						return true
					}
				}
				if len(g.spanEvent) > 0 {
					removeIf(span.Events(), &np.kids, func(ev ptrace.SpanEvent, ep *node) bool {
						i, err := first(e, g.spanEvent, g.mode, ottlcompat.NewSpanEventContext(rs, ss, span, ev))
						if err != nil {
							errs = errors.Join(errs, err)
							return false
						}
						if i >= 0 {
							e.drop(fmt.Sprintf("%s.event[%d]", ref, ep.index), i, 0)
							return true
						}
						return false
					})
				}
				return false
			})
			return ss.Spans().Len() == 0
		})
		return rs.ScopeSpans().Len() == 0
	})
	return errs
}

// metrics applies one group to md as filterprocessor's MetricsConsumer
// does: a metric left without data points is removed.
func (e *evaluation) metrics(g *group, md pmetric.Metrics, pos *[]node) error {
	var errs error
	removeIf(md.ResourceMetrics(), pos, func(rm pmetric.ResourceMetrics, rp *node) bool {
		if len(g.resource) > 0 {
			i, err := first(e, g.resource, g.mode, ottlcompat.NewResourceContext(rm.Resource(), rm))
			if err != nil {
				errs = errors.Join(errs, err)
				return false
			}
			if i >= 0 {
				e.drop(resourceRef(rp.index), i, pointCountResource(rm))
				return true
			}
		}
		if len(g.scope) == 0 && len(g.metric) == 0 && len(g.dataPoint) == 0 {
			return rm.ScopeMetrics().Len() == 0
		}
		removeIf(rm.ScopeMetrics(), &rp.kids, func(sm pmetric.ScopeMetrics, sp *node) bool {
			if len(g.scope) > 0 {
				i, err := first(e, g.scope, g.mode, ottlcompat.NewScopeContext(sm.Scope(), rm.Resource(), sm, rm))
				if err != nil {
					errs = errors.Join(errs, err)
					return false
				}
				if i >= 0 {
					e.drop(scopeRef(rp.index, sp.index), i, pointCountScope(sm))
					return true
				}
			}
			if len(g.metric) == 0 && len(g.dataPoint) == 0 {
				return sm.Metrics().Len() == 0
			}
			removeIf(sm.Metrics(), &sp.kids, func(m pmetric.Metric, mp *node) bool {
				ref := itemRef(rp.index, sp.index, "metric", mp.index)
				if len(g.metric) > 0 {
					i, err := first(e, g.metric, g.mode, ottlcompat.NewMetricContext(rm, sm, m))
					if err != nil {
						errs = errors.Join(errs, err)
						return false
					}
					if i >= 0 {
						e.drop(ref, i, pointCount(m))
						return true
					}
				}
				if len(g.dataPoint) == 0 {
					return false
				}
				var pointErr error
				point := func(dp any, pp *node) bool {
					i, err := first(e, g.dataPoint, g.mode, ottlcompat.NewDataPointContext(rm, sm, m, dp))
					if err != nil {
						pointErr = errors.Join(pointErr, err)
						return false
					}
					if i >= 0 {
						e.drop(fmt.Sprintf("%s.data_point[%d]", ref, pp.index), i, 1)
						return true
					}
					return false
				}
				switch m.Type() {
				case pmetric.MetricTypeSum:
					removeIf(m.Sum().DataPoints(), &mp.kids, func(dp pmetric.NumberDataPoint, pp *node) bool { return point(dp, pp) })
				case pmetric.MetricTypeGauge:
					removeIf(m.Gauge().DataPoints(), &mp.kids, func(dp pmetric.NumberDataPoint, pp *node) bool { return point(dp, pp) })
				case pmetric.MetricTypeHistogram:
					removeIf(m.Histogram().DataPoints(), &mp.kids, func(dp pmetric.HistogramDataPoint, pp *node) bool { return point(dp, pp) })
				case pmetric.MetricTypeExponentialHistogram:
					removeIf(m.ExponentialHistogram().DataPoints(), &mp.kids, func(dp pmetric.ExponentialHistogramDataPoint, pp *node) bool { return point(dp, pp) })
				case pmetric.MetricTypeSummary:
					removeIf(m.Summary().DataPoints(), &mp.kids, func(dp pmetric.SummaryDataPoint, pp *node) bool { return point(dp, pp) })
				default:
					return false
				}
				if pointErr != nil {
					// The processor keeps a metric whose data points failed.
					errs = errors.Join(errs, pointErr)
					return false
				}
				return pointCount(m) == 0
			})
			return sm.Metrics().Len() == 0
		})
		return rm.ScopeMetrics().Len() == 0
	})
	return errs
}

func logCount(rl plog.ResourceLogs) int {
	n := 0
	for _, sl := range rl.ScopeLogs().All() {
		n += sl.LogRecords().Len()
	}
	return n
}

func spanCount(rs ptrace.ResourceSpans) int {
	n := 0
	for _, ss := range rs.ScopeSpans().All() {
		n += ss.Spans().Len()
	}
	return n
}

func pointCount(m pmetric.Metric) int {
	switch m.Type() {
	case pmetric.MetricTypeSum:
		return m.Sum().DataPoints().Len()
	case pmetric.MetricTypeGauge:
		return m.Gauge().DataPoints().Len()
	case pmetric.MetricTypeHistogram:
		return m.Histogram().DataPoints().Len()
	case pmetric.MetricTypeExponentialHistogram:
		return m.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		return m.Summary().DataPoints().Len()
	}
	return 0
}

func pointCountScope(sm pmetric.ScopeMetrics) int {
	n := 0
	for _, m := range sm.Metrics().All() {
		n += pointCount(m)
	}
	return n
}

func pointCountResource(rm pmetric.ResourceMetrics) int {
	n := 0
	for _, sm := range rm.ScopeMetrics().All() {
		n += pointCountScope(sm)
	}
	return n
}
