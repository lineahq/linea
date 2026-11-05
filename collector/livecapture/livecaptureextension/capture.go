package livecaptureextension

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottldatapoint"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlmetric"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlresource"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/metric"
)

// envelopeTag over-estimates a nested message's field tag and length prefix,
// so staged sizes never undercount the encoded copy.
const envelopeTag = 1 + 5

// fieldSize is the encoded size of an embedded message of n bytes.
func fieldSize(n int) int64 {
	size := int64(2)
	for v := uint64(n) >> 7; v != 0; v >>= 7 {
		size++
	}
	return size + int64(n)
}

// capture is the state of one capture call. A call builds one redacted copy of
// all records it selects and admits it once, so the session gate and the
// exporter queue see a batch instead of every record.
type capture struct {
	manager   *Manager
	session   *session
	signal    Signal
	point     Point
	options   metric.MeasurementOption
	redact    *redactor // Nil for a raw point.
	started   time.Time
	now       int64
	sizes     []int64 // Estimated encoded size of each staged record, in copy order.
	sizeArray [64]int64
	staged    int64 // Sum of sizes.
	pending   int64 // Envelope bytes of groups no staged record paid for yet.
	budget    bool  // Some record was refused for the session budget.
	refused   dropReason
	// Flushed to the session and telemetry once per call.
	selected, filterErrors int64
	drops                  [dropReasonCount]int64
}

func (m *Manager) begin(signal Signal, point Point) *capture {
	s := m.active.Load()
	if s == nil {
		return nil
	}
	selected, ok := s.points[captureKey{Point: point, signal: signal}]
	if !ok {
		return nil
	}
	started := time.Now()
	c := &capture{manager: m, session: s, signal: signal, point: point, options: selected.options, redact: selected.redactor, started: started, now: started.UnixNano(), refused: -1}
	c.sizes = c.sizeArray[:0]
	return c
}

func (c *capture) finish() {
	if recover() != nil {
		c.session.requestStop("failed", "capture_panic")
	}
	r, t := c.session.run, &c.manager.telemetry
	if c.selected > 0 {
		r.selected.Add(c.selected)
		t.selected.Add(context.Background(), c.selected, c.options)
	}
	if c.filterErrors > 0 {
		r.filterErrors.Add(c.filterErrors)
		t.errors.Add(context.Background(), c.filterErrors, t.signalLabels[c.signal])
	}
	for reason, n := range c.drops {
		if n > 0 {
			r.drops[reason].Add(n)
			t.dropped.Add(context.Background(), n, t.dropLabels[c.signal][reason])
		}
	}
	t.duration.Record(context.Background(), time.Since(c.started).Seconds(), c.options)
}

func (c *capture) stopped() bool { return c.session.stopped.Load() }

func (c *capture) matched(ok bool, err error) bool {
	if err != nil {
		c.filterErrors++
	}
	return ok && err == nil
}

func (c *capture) resourceMatches(ctx context.Context, resource pcommon.Resource, schema ottl.SchemaURLItem) bool {
	if len(c.session.filter.resource) == 0 {
		return true
	}
	record := ottlresource.NewTransformContext(resource, schema)
	defer record.Close()
	return c.matched(c.session.filter.resource.match(ctx, record))
}

// sampled decides on the original record. hash runs only in hash mode.
func (c *capture) sampled(hash func() uint64) bool {
	sampling := c.session.request.Sampling
	if sampling == nil || sampling.Rate >= 1 {
		return true
	}
	var fraction float64
	if c.signal != Traces && sampling.Mode == "random" {
		fraction = rand.Float64()
	} else {
		fraction = float64(hash()>>11) / float64(uint64(1)<<53)
	}
	if fraction >= sampling.Rate {
		c.drops[dropSampling]++
		return false
	}
	return true
}

// refusal pre-checks whether a record of size bytes can still be admitted
// after the records already staged, without taking the gate. Counters only
// grow within a rate window and a run, so a refusal here is final; admit
// re-checks everything under the gate.
func (c *capture) refusal(size int64) dropReason {
	r, limits := c.session.run, &c.session.request.Limits
	count := int64(len(c.sizes))
	if r.enqueued.Load()+count >= limits.TotalRecords || size > limits.TotalBytes-r.bytes.Load()-c.staged {
		c.budget = true
		return dropBudget
	}
	var windowRecords, windowBytes int64
	if c.now-r.windowStart.Load() < int64(time.Second) {
		windowRecords, windowBytes = r.windowRecords.Load(), r.windowBytes.Load()
	}
	if windowRecords+count >= limits.RecordsPerSecond {
		return dropRateRecords
	}
	if size > limits.BytesPerSecond-windowBytes-c.staged {
		return dropRateBytes
	}
	return -1
}

// precheck runs before a selected record is copied. It refuses every record
// once one was refused: admission keeps a prefix of the staged records.
func (c *capture) precheck() bool {
	if c.refused < 0 {
		c.refused = c.refusal(1)
	}
	if c.refused >= 0 {
		c.drops[c.refused]++
		return false
	}
	return true
}

// stage accounts a redacted record of n encoded bytes. It returns false and
// counts the drop when the record cannot be admitted.
func (c *capture) stage(n int) bool {
	size := fieldSize(n)
	if size > c.session.request.Limits.RecordBytes {
		c.drops[dropRecordSize]++
		return false
	}
	size += c.pending
	if c.refused = c.refusal(size); c.refused >= 0 {
		c.drops[c.refused]++
		return false
	}
	c.sizes = append(c.sizes, size)
	c.staged += size
	c.pending = 0
	return true
}

// admit takes the gate once, keeps the longest admissible prefix of the
// staged records, and hands it to the exporter queue. prune removes all but
// the first n records and empty groups; size returns the exact encoded size.
func (c *capture) admit(prune func(int), size func() int64, send func() error) {
	if len(c.sizes) == 0 && !c.budget {
		return
	}
	r, limits := c.session.run, &c.session.request.Limits
	r.gate.Lock()
	defer r.gate.Unlock()
	if r.stopped.Load() {
		c.drops[dropStopping] += int64(len(c.sizes))
		return
	}
	now := time.Now().UnixNano()
	if now >= r.deadline.Load() {
		r.requestStop("expired", "ttl")
		c.drops[dropStopping] += int64(len(c.sizes))
		return
	}
	if now-r.windowStart.Load() >= int64(time.Second) {
		r.windowStart.Store(now)
		r.windowBytes.Store(0)
		r.windowRecords.Store(0)
	}
	enqueued, bytes := r.enqueued.Load(), r.bytes.Load()
	windowRecords, windowBytes := r.windowRecords.Load(), r.windowBytes.Load()
	keep, kept := 0, int64(0)
	reason := dropReason(-1)
	for _, size := range c.sizes {
		switch {
		case enqueued+int64(keep) >= limits.TotalRecords || size > limits.TotalBytes-bytes-kept:
			reason = dropBudget
			c.budget = true
		case windowRecords+int64(keep) >= limits.RecordsPerSecond:
			reason = dropRateRecords
		case size > limits.BytesPerSecond-windowBytes-kept:
			reason = dropRateBytes
		}
		if reason >= 0 {
			break
		}
		keep++
		kept += size
	}
	if refused := len(c.sizes) - keep; refused > 0 {
		c.drops[reason] += int64(refused)
	}
	if keep > 0 {
		prune(keep)
		// Estimates bound admission; counters record the exact copy.
		actual := size()
		if err := send(); err != nil {
			if errors.Is(err, exporterQueueFull) {
				c.drops[dropQueueFull] += int64(keep)
			} else {
				c.drops[dropDelivery] += int64(keep)
			}
		} else {
			r.windowBytes.Add(actual)
			r.windowRecords.Add(int64(keep))
			bytes = r.bytes.Add(actual)
			enqueued = r.enqueued.Add(int64(keep))
			c.manager.telemetry.bytes.Add(context.Background(), actual, c.manager.telemetry.signalLabels[c.signal])
		}
	}
	if c.budget || bytes >= limits.TotalBytes || enqueued >= limits.TotalRecords {
		r.requestStop("limit_reached", "budget")
	}
}

func (c *capture) tag(resource pcommon.Resource) {
	attributes := resource.Attributes()
	attributes.PutStr("capture.session.id", c.session.request.SessionID)
	attributes.PutStr("capture.point", c.point.Point)
	attributes.PutStr("capture.pipeline", c.point.Pipeline)
	attributes.PutStr("capture.collector.pod", c.manager.config.Pod)
	attributes.PutStr("capture.cluster", c.manager.config.Cluster)
}

func (m *Manager) CaptureTraces(ctx context.Context, point Point, data ptrace.Traces) {
	c := m.begin(Traces, point)
	if c == nil {
		return
	}
	defer c.finish()
	s, redact := c.session, c.redact
	sizer := &ptrace.ProtoMarshaler{}
	var out ptrace.Traces
	var scratch ptrace.Span
	created := false
records:
	for _, rs := range data.ResourceSpans().All() {
		if !c.resourceMatches(ctx, rs.Resource(), rs) {
			continue
		}
		var resource ptrace.ResourceSpans
		hasResource := false
		for _, ss := range rs.ScopeSpans().All() {
			var scope ptrace.ScopeSpans
			hasScope := false
			for _, span := range ss.Spans().All() {
				if c.stopped() {
					break records
				}
				if len(s.filter.span) > 0 {
					record := ottlspan.NewTransformContext(rs, ss, span)
					ok, err := s.filter.span.match(ctx, record)
					record.Close()
					if !c.matched(ok, err) {
						continue
					}
				}
				c.selected++
				if !c.sampled(func() uint64 { return traceHash(span.TraceID()) }) || !c.precheck() {
					continue
				}
				if !created {
					out, scratch, created = ptrace.NewTraces(), ptrace.NewSpan(), true
				}
				span.CopyTo(scratch)
				redact.span(scratch)
				if !hasResource {
					resource, hasResource = out.ResourceSpans().AppendEmpty(), true
					rs.Resource().CopyTo(resource.Resource())
					resource.SetSchemaUrl(rs.SchemaUrl())
					redact.resource(resource.Resource())
					c.tag(resource.Resource())
					c.pending += envelopeTag + int64(sizer.ResourceSpansSize(resource))
				}
				if !hasScope {
					scope, hasScope = resource.ScopeSpans().AppendEmpty(), true
					ss.Scope().CopyTo(scope.Scope())
					scope.SetSchemaUrl(ss.SchemaUrl())
					redact.scope(scope.Scope())
					c.pending += envelopeTag + int64(sizer.ScopeSpansSize(scope))
				}
				if c.stage(sizer.SpanSize(scratch)) {
					scratch.MoveTo(scope.Spans().AppendEmpty())
				}
			}
		}
	}
	if !created {
		c.admit(nil, nil, nil)
		return
	}
	c.admit(func(n int) {
		out.ResourceSpans().RemoveIf(func(rs ptrace.ResourceSpans) bool {
			rs.ScopeSpans().RemoveIf(func(ss ptrace.ScopeSpans) bool {
				ss.Spans().RemoveIf(func(ptrace.Span) bool { n--; return n < 0 })
				return ss.Spans().Len() == 0
			})
			return rs.ScopeSpans().Len() == 0
		})
	}, func() int64 { return int64(sizer.TracesSize(out)) }, func() error { return m.delivery.traces(context.Background(), out) })
}

func (m *Manager) CaptureLogs(ctx context.Context, point Point, data plog.Logs) {
	c := m.begin(Logs, point)
	if c == nil {
		return
	}
	defer c.finish()
	s, redact := c.session, c.redact
	sizer := &plog.ProtoMarshaler{}
	var out plog.Logs
	var scratch plog.LogRecord
	created := false
records:
	for _, rl := range data.ResourceLogs().All() {
		if !c.resourceMatches(ctx, rl.Resource(), rl) {
			continue
		}
		var resource plog.ResourceLogs
		hasResource := false
		for _, sl := range rl.ScopeLogs().All() {
			var scope plog.ScopeLogs
			hasScope := false
			for _, log := range sl.LogRecords().All() {
				if c.stopped() {
					break records
				}
				if len(s.filter.log) > 0 {
					record := ottllog.NewTransformContext(rl, sl, log)
					ok, err := s.filter.log.match(ctx, record)
					record.Close()
					if !c.matched(ok, err) {
						continue
					}
				}
				c.selected++
				if !c.sampled(func() uint64 { return logHash(log) }) || !c.precheck() {
					continue
				}
				if !created {
					out, scratch, created = plog.NewLogs(), plog.NewLogRecord(), true
				}
				log.CopyTo(scratch)
				redact.log(scratch)
				if !hasResource {
					resource, hasResource = out.ResourceLogs().AppendEmpty(), true
					rl.Resource().CopyTo(resource.Resource())
					resource.SetSchemaUrl(rl.SchemaUrl())
					redact.resource(resource.Resource())
					c.tag(resource.Resource())
					c.pending += envelopeTag + int64(sizer.ResourceLogsSize(resource))
				}
				if !hasScope {
					scope, hasScope = resource.ScopeLogs().AppendEmpty(), true
					sl.Scope().CopyTo(scope.Scope())
					scope.SetSchemaUrl(sl.SchemaUrl())
					redact.scope(scope.Scope())
					c.pending += envelopeTag + int64(sizer.ScopeLogsSize(scope))
				}
				if c.stage(sizer.LogRecordSize(scratch)) {
					scratch.MoveTo(scope.LogRecords().AppendEmpty())
				}
			}
		}
	}
	if !created {
		c.admit(nil, nil, nil)
		return
	}
	c.admit(func(n int) {
		out.ResourceLogs().RemoveIf(func(rl plog.ResourceLogs) bool {
			rl.ScopeLogs().RemoveIf(func(sl plog.ScopeLogs) bool {
				sl.LogRecords().RemoveIf(func(plog.LogRecord) bool { n--; return n < 0 })
				return sl.LogRecords().Len() == 0
			})
			return rl.ScopeLogs().Len() == 0
		})
	}, func() int64 { return int64(sizer.LogsSize(out)) }, func() error { return m.delivery.logs(context.Background(), out) })
}

// metricCapture tracks the copy groups of one metrics capture call. Its
// capture unit is the datapoint.
type metricCapture struct {
	*capture
	ctx         context.Context
	sizer       *pmetric.ProtoMarshaler
	out         pmetric.Metrics
	created     bool
	rm          pmetric.ResourceMetrics
	sm          pmetric.ScopeMetrics
	source      pmetric.Metric
	resource    pmetric.ResourceMetrics
	scope       pmetric.ScopeMetrics
	metric      pmetric.Metric
	hasResource bool
	hasScope    bool
	hasMetric   bool
}

// datapoint is the part of every metric datapoint type that selection reads.
type datapoint interface {
	StartTimestamp() pcommon.Timestamp
	Timestamp() pcommon.Timestamp
	Attributes() pcommon.Map
}

// selects filters and samples one original datapoint of mc.source. The point
// is boxed only for the OTTL datapoint context, so a session without a
// datapoint filter allocates nothing per datapoint here.
func selects[P datapoint](mc *metricCapture, point P) bool {
	s := mc.session
	if len(s.filter.datapoint) > 0 {
		record := ottldatapoint.NewTransformContext(mc.rm, mc.sm, mc.source, any(point))
		ok, err := s.filter.datapoint.match(mc.ctx, record)
		record.Close()
		if !mc.matched(ok, err) {
			return false
		}
	}
	mc.selected++
	return mc.sampled(func() uint64 {
		return pointHash(mc.source, point.StartTimestamp(), point.Timestamp(), point.Attributes())
	}) && mc.precheck()
}

// target creates the copy groups for the current source metric on first use
// and returns the metric shell selected datapoints move into.
func (mc *metricCapture) target() pmetric.Metric {
	redact, sizer := mc.redact, mc.sizer
	if !mc.created {
		mc.out, mc.created = pmetric.NewMetrics(), true
	}
	if !mc.hasResource {
		mc.resource, mc.hasResource = mc.out.ResourceMetrics().AppendEmpty(), true
		mc.rm.Resource().CopyTo(mc.resource.Resource())
		mc.resource.SetSchemaUrl(mc.rm.SchemaUrl())
		redact.resource(mc.resource.Resource())
		mc.tag(mc.resource.Resource())
		mc.pending += envelopeTag + int64(sizer.ResourceMetricsSize(mc.resource))
	}
	if !mc.hasScope {
		mc.scope, mc.hasScope = mc.resource.ScopeMetrics().AppendEmpty(), true
		mc.sm.Scope().CopyTo(mc.scope.Scope())
		mc.scope.SetSchemaUrl(mc.sm.SchemaUrl())
		redact.scope(mc.scope.Scope())
		mc.pending += envelopeTag + int64(sizer.ScopeMetricsSize(mc.scope))
	}
	if !mc.hasMetric {
		mc.metric, mc.hasMetric = mc.scope.Metrics().AppendEmpty(), true
		copyMetricShell(mc.source, mc.metric)
		redact.metric(mc.metric)
		mc.pending += envelopeTag + int64(mc.sizer.MetricSize(mc.metric))
	}
	return mc.metric
}

func (m *Manager) CaptureMetrics(ctx context.Context, point Point, data pmetric.Metrics) {
	c := m.begin(Metrics, point)
	if c == nil {
		return
	}
	defer c.finish()
	s, redact := c.session, c.redact
	mc := &metricCapture{capture: c, ctx: ctx, sizer: &pmetric.ProtoMarshaler{}}
	sizer := mc.sizer
	var number pmetric.NumberDataPoint
	var histogram pmetric.HistogramDataPoint
	var exponential pmetric.ExponentialHistogramDataPoint
	var summary pmetric.SummaryDataPoint
records:
	for _, rm := range data.ResourceMetrics().All() {
		if !c.resourceMatches(ctx, rm.Resource(), rm) {
			continue
		}
		mc.rm, mc.hasResource = rm, false
		for _, sm := range rm.ScopeMetrics().All() {
			mc.sm, mc.hasScope = sm, false
			for _, source := range sm.Metrics().All() {
				if c.stopped() {
					break records
				}
				if len(s.filter.metric) > 0 {
					record := ottlmetric.NewTransformContext(rm, sm, source)
					ok, err := s.filter.metric.match(ctx, record)
					record.Close()
					if !c.matched(ok, err) {
						continue
					}
				}
				mc.source, mc.hasMetric = source, false
				switch source.Type() {
				case pmetric.MetricTypeGauge, pmetric.MetricTypeSum:
					var points pmetric.NumberDataPointSlice
					if source.Type() == pmetric.MetricTypeSum {
						points = source.Sum().DataPoints()
					} else {
						points = source.Gauge().DataPoints()
					}
					for _, p := range points.All() {
						if c.stopped() {
							break records
						}
						if !selects(mc, p) {
							continue
						}
						if number == (pmetric.NumberDataPoint{}) {
							number = pmetric.NewNumberDataPoint()
						}
						p.CopyTo(number)
						redact.numberDataPoint(number)
						target := mc.target()
						if c.stage(sizer.NumberDataPointSize(number)) {
							if target.Type() == pmetric.MetricTypeSum {
								number.MoveTo(target.Sum().DataPoints().AppendEmpty())
							} else {
								number.MoveTo(target.Gauge().DataPoints().AppendEmpty())
							}
						}
					}
				case pmetric.MetricTypeHistogram:
					for _, p := range source.Histogram().DataPoints().All() {
						if c.stopped() {
							break records
						}
						if !selects(mc, p) {
							continue
						}
						if histogram == (pmetric.HistogramDataPoint{}) {
							histogram = pmetric.NewHistogramDataPoint()
						}
						p.CopyTo(histogram)
						redact.histogramDataPoint(histogram)
						target := mc.target()
						if c.stage(sizer.HistogramDataPointSize(histogram)) {
							histogram.MoveTo(target.Histogram().DataPoints().AppendEmpty())
						}
					}
				case pmetric.MetricTypeExponentialHistogram:
					for _, p := range source.ExponentialHistogram().DataPoints().All() {
						if c.stopped() {
							break records
						}
						if !selects(mc, p) {
							continue
						}
						if exponential == (pmetric.ExponentialHistogramDataPoint{}) {
							exponential = pmetric.NewExponentialHistogramDataPoint()
						}
						p.CopyTo(exponential)
						redact.exponentialHistogramDataPoint(exponential)
						target := mc.target()
						if c.stage(sizer.ExponentialHistogramDataPointSize(exponential)) {
							exponential.MoveTo(target.ExponentialHistogram().DataPoints().AppendEmpty())
						}
					}
				case pmetric.MetricTypeSummary:
					for _, p := range source.Summary().DataPoints().All() {
						if c.stopped() {
							break records
						}
						if !selects(mc, p) {
							continue
						}
						if summary == (pmetric.SummaryDataPoint{}) {
							summary = pmetric.NewSummaryDataPoint()
						}
						p.CopyTo(summary)
						redact.summaryDataPoint(summary)
						target := mc.target()
						if c.stage(sizer.SummaryDataPointSize(summary)) {
							summary.MoveTo(target.Summary().DataPoints().AppendEmpty())
						}
					}
				}
			}
		}
	}
	if !mc.created {
		c.admit(nil, nil, nil)
		return
	}
	out := mc.out
	c.admit(func(n int) {
		out.ResourceMetrics().RemoveIf(func(rm pmetric.ResourceMetrics) bool {
			rm.ScopeMetrics().RemoveIf(func(sm pmetric.ScopeMetrics) bool {
				sm.Metrics().RemoveIf(func(metric pmetric.Metric) bool { return keepDatapoints(metric, &n) == 0 })
				return sm.Metrics().Len() == 0
			})
			return rm.ScopeMetrics().Len() == 0
		})
	}, func() int64 { return int64(sizer.MetricsSize(out)) }, func() error { return m.delivery.metrics(context.Background(), out) })
}

// keepDatapoints keeps the first *n datapoints of metric, lowers *n by the
// number kept, and returns how many remain.
func keepDatapoints(metric pmetric.Metric, n *int) int {
	keep := func() bool { *n--; return *n < 0 }
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		metric.Gauge().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return keep() })
		return metric.Gauge().DataPoints().Len()
	case pmetric.MetricTypeSum:
		metric.Sum().DataPoints().RemoveIf(func(pmetric.NumberDataPoint) bool { return keep() })
		return metric.Sum().DataPoints().Len()
	case pmetric.MetricTypeHistogram:
		metric.Histogram().DataPoints().RemoveIf(func(pmetric.HistogramDataPoint) bool { return keep() })
		return metric.Histogram().DataPoints().Len()
	case pmetric.MetricTypeExponentialHistogram:
		metric.ExponentialHistogram().DataPoints().RemoveIf(func(pmetric.ExponentialHistogramDataPoint) bool { return keep() })
		return metric.ExponentialHistogram().DataPoints().Len()
	case pmetric.MetricTypeSummary:
		metric.Summary().DataPoints().RemoveIf(func(pmetric.SummaryDataPoint) bool { return keep() })
		return metric.Summary().DataPoints().Len()
	}
	return 0
}

// copyMetricShell copies a metric's descriptor and data type without its
// datapoints. It reserves room for the source's datapoints so selected ones
// append without regrowing the slice.
func copyMetricShell(source, target pmetric.Metric) {
	target.SetName(source.Name())
	target.SetDescription(source.Description())
	target.SetUnit(source.Unit())
	source.Metadata().CopyTo(target.Metadata())
	switch source.Type() {
	case pmetric.MetricTypeGauge:
		target.SetEmptyGauge().DataPoints().EnsureCapacity(source.Gauge().DataPoints().Len())
	case pmetric.MetricTypeSum:
		sum := target.SetEmptySum()
		sum.SetAggregationTemporality(source.Sum().AggregationTemporality())
		sum.SetIsMonotonic(source.Sum().IsMonotonic())
		sum.DataPoints().EnsureCapacity(source.Sum().DataPoints().Len())
	case pmetric.MetricTypeHistogram:
		histogram := target.SetEmptyHistogram()
		histogram.SetAggregationTemporality(source.Histogram().AggregationTemporality())
		histogram.DataPoints().EnsureCapacity(source.Histogram().DataPoints().Len())
	case pmetric.MetricTypeExponentialHistogram:
		exponential := target.SetEmptyExponentialHistogram()
		exponential.SetAggregationTemporality(source.ExponentialHistogram().AggregationTemporality())
		exponential.DataPoints().EnsureCapacity(source.ExponentialHistogram().DataPoints().Len())
	case pmetric.MetricTypeSummary:
		target.SetEmptySummary().DataPoints().EnsureCapacity(source.Summary().DataPoints().Len())
	}
}

func forEachDatapoint(metric pmetric.Metric, visit func(any)) {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		for _, point := range metric.Gauge().DataPoints().All() {
			visit(point)
		}
	case pmetric.MetricTypeSum:
		for _, point := range metric.Sum().DataPoints().All() {
			visit(point)
		}
	case pmetric.MetricTypeHistogram:
		for _, point := range metric.Histogram().DataPoints().All() {
			visit(point)
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, point := range metric.ExponentialHistogram().DataPoints().All() {
			visit(point)
		}
	case pmetric.MetricTypeSummary:
		for _, point := range metric.Summary().DataPoints().All() {
			visit(point)
		}
	}
}

func datapointAttributes(point any) pcommon.Map {
	switch p := point.(type) {
	case pmetric.NumberDataPoint:
		return p.Attributes()
	case pmetric.HistogramDataPoint:
		return p.Attributes()
	case pmetric.ExponentialHistogramDataPoint:
		return p.Attributes()
	case pmetric.SummaryDataPoint:
		return p.Attributes()
	default:
		panic("unknown metric datapoint type")
	}
}

// fnv64 is FNV-1a. Sampling hashes the original record's identifying content,
// so every pod and capture point keeps the same records.
type fnv64 uint64

const (
	fnvOffset fnv64 = 14695981039346656037
	fnvPrime  fnv64 = 1099511628211
)

func (h fnv64) byte(b byte) fnv64 { return (h ^ fnv64(b)) * fnvPrime }
func (h fnv64) bytes(b []byte) fnv64 {
	for _, x := range b {
		h = h.byte(x)
	}
	return h
}
func (h fnv64) string(s string) fnv64 {
	for i := range len(s) {
		h = h.byte(s[i])
	}
	return h.byte(0xff) // Separates adjacent strings.
}
func (h fnv64) uint64(v uint64) fnv64 {
	for range 8 {
		h = h.byte(byte(v))
		v >>= 8
	}
	return h
}

func (h fnv64) value(v pcommon.Value) fnv64 {
	h = h.byte(byte(v.Type()))
	switch v.Type() {
	case pcommon.ValueTypeStr:
		return h.string(v.Str())
	case pcommon.ValueTypeInt:
		return h.uint64(uint64(v.Int()))
	case pcommon.ValueTypeDouble:
		return h.uint64(math.Float64bits(v.Double()))
	case pcommon.ValueTypeBool:
		if v.Bool() {
			return h.byte(1)
		}
		return h.byte(0)
	case pcommon.ValueTypeBytes:
		return h.uint64(uint64(v.Bytes().Len())).bytes(v.Bytes().AsRaw())
	case pcommon.ValueTypeMap:
		return h.attributes(v.Map())
	case pcommon.ValueTypeSlice:
		h = h.uint64(uint64(v.Slice().Len()))
		for _, item := range v.Slice().All() {
			h = h.value(item)
		}
	}
	return h
}

func (h fnv64) attributes(attributes pcommon.Map) fnv64 {
	h = h.uint64(uint64(attributes.Len()))
	for key, value := range attributes.All() {
		h = h.string(key).value(value)
	}
	return h
}

// sum finalizes the hash with MurmurHash3's fmix64. Sampling compares the
// top bits, which plain FNV-1a mixes poorly for inputs that differ only in
// their last bytes, such as consecutive timestamps.
func (h fnv64) sum() uint64 {
	x := uint64(h)
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// traceHash keeps every span of a trace together.
func traceHash(id pcommon.TraceID) uint64 { return fnvOffset.bytes(id[:]).sum() }

func logHash(log plog.LogRecord) uint64 {
	traceID, spanID := log.TraceID(), log.SpanID()
	h := fnvOffset.uint64(uint64(log.Timestamp())).uint64(uint64(log.ObservedTimestamp())).bytes(traceID[:]).bytes(spanID[:])
	h = h.uint64(uint64(log.SeverityNumber())).string(log.SeverityText()).string(log.EventName())
	return h.value(log.Body()).attributes(log.Attributes()).sum()
}

func pointHash(metric pmetric.Metric, start, timestamp pcommon.Timestamp, attributes pcommon.Map) uint64 {
	return fnvOffset.string(metric.Name()).uint64(uint64(start)).uint64(uint64(timestamp)).attributes(attributes).sum()
}
