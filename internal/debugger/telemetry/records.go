package telemetry

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"
)

// Record is one item of a payload (span, log record or metric data point)
// flattened into typed fields, so items of every signal can be compared,
// fingerprinted and diffed field by field.
//
// Field names follow OTLP JSON in snake case ("time_unix_nano", "body",
// "attributes"). A data point also carries its metric's fields under
// "metric.*". Values keep their pdata types: an Int 1 and a Double 1 differ.
type Record struct {
	// Resource and Scope index the record's resource and scope in the
	// payload; Metric indexes the metric within the scope (-1 for spans and
	// logs); Index is the position within the parent list.
	Resource, Scope, Metric, Index int
	Fields                         pcommon.Map
}

// Ref names the record's position, for example
// "resource[0].scope[1].log_record[2]".
func (r Record) Ref(signal pipeline.Signal) string {
	base := fmt.Sprintf("resource[%d].scope[%d]", r.Resource, r.Scope)
	switch signal {
	case pipeline.SignalTraces:
		return fmt.Sprintf("%s.span[%d]", base, r.Index)
	case pipeline.SignalMetrics:
		return fmt.Sprintf("%s.metric[%d].data_point[%d]", base, r.Metric, r.Index)
	}
	return fmt.Sprintf("%s.log_record[%d]", base, r.Index)
}

// Str returns a string field, or "" when absent.
func (r Record) Str(field string) string {
	v, ok := r.Fields.Get(field)
	if !ok {
		return ""
	}
	return v.AsString()
}

// Records flattens every item of the payload, in payload order.
func Records(p Payload) []Record {
	var out []Record
	switch p.Signal {
	case pipeline.SignalTraces:
		for i, rs := range p.Traces.ResourceSpans().All() {
			for j, ss := range rs.ScopeSpans().All() {
				for k, s := range ss.Spans().All() {
					out = append(out, Record{Resource: i, Scope: j, Metric: -1, Index: k, Fields: spanFields(s)})
				}
			}
		}
	case pipeline.SignalLogs:
		for i, rl := range p.Logs.ResourceLogs().All() {
			for j, sl := range rl.ScopeLogs().All() {
				for k, lr := range sl.LogRecords().All() {
					out = append(out, Record{Resource: i, Scope: j, Metric: -1, Index: k, Fields: LogRecordFields(lr)})
				}
			}
		}
	case pipeline.SignalMetrics:
		for i, rm := range p.Metrics.ResourceMetrics().All() {
			for j, sm := range rm.ScopeMetrics().All() {
				for m, metric := range sm.Metrics().All() {
					for k, fields := range PointFields(metric) {
						out = append(out, Record{Resource: i, Scope: j, Metric: m, Index: k, Fields: fields})
					}
				}
			}
		}
	}
	return out
}

// ResourceAt returns the i-th resource and its schema URL.
func (p Payload) ResourceAt(i int) (pcommon.Resource, string) {
	switch p.Signal {
	case pipeline.SignalTraces:
		rs := p.Traces.ResourceSpans().At(i)
		return rs.Resource(), rs.SchemaUrl()
	case pipeline.SignalMetrics:
		rm := p.Metrics.ResourceMetrics().At(i)
		return rm.Resource(), rm.SchemaUrl()
	}
	rl := p.Logs.ResourceLogs().At(i)
	return rl.Resource(), rl.SchemaUrl()
}

// ScopeAt returns scope j of resource i and its schema URL.
func (p Payload) ScopeAt(i, j int) (pcommon.InstrumentationScope, string) {
	switch p.Signal {
	case pipeline.SignalTraces:
		ss := p.Traces.ResourceSpans().At(i).ScopeSpans().At(j)
		return ss.Scope(), ss.SchemaUrl()
	case pipeline.SignalMetrics:
		sm := p.Metrics.ResourceMetrics().At(i).ScopeMetrics().At(j)
		return sm.Scope(), sm.SchemaUrl()
	}
	sl := p.Logs.ResourceLogs().At(i).ScopeLogs().At(j)
	return sl.Scope(), sl.SchemaUrl()
}

// ContextFingerprints returns, for every record in payload order, a
// fingerprint of the record together with its resource and scope. Two
// payloads with equal sorted fingerprints deliver the same telemetry,
// possibly in a different order or grouping.
func ContextFingerprints(p Payload) []string {
	recs := Records(p)
	out := make([]string, len(recs))
	for n, r := range recs {
		res, resURL := p.ResourceAt(r.Resource)
		scope, scopeURL := p.ScopeAt(r.Resource, r.Scope)
		var b strings.Builder
		writeMap(&b, res.Attributes())
		b.WriteString(strconv.Quote(resURL))
		b.WriteString(strconv.Quote(scope.Name()))
		b.WriteString(strconv.Quote(scope.Version()))
		writeMap(&b, scope.Attributes())
		b.WriteString(strconv.Quote(scopeURL))
		writeMap(&b, r.Fields)
		out[n] = b.String()
	}
	return out
}

// Fingerprint returns a canonical, type-preserving encoding of a map: equal
// fingerprints mean equal keys, types and values. Map keys are sorted.
func Fingerprint(m pcommon.Map) string {
	var b strings.Builder
	writeMap(&b, m)
	return b.String()
}

func writeMap(b *strings.Builder, m pcommon.Map) {
	keys := make([]string, 0, m.Len())
	for k := range m.All() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	b.WriteByte('{')
	for _, k := range keys {
		v, _ := m.Get(k)
		b.WriteString(strconv.Quote(k))
		b.WriteByte(':')
		writeValue(b, v)
		b.WriteByte(',')
	}
	b.WriteByte('}')
}

func writeValue(b *strings.Builder, v pcommon.Value) {
	switch v.Type() {
	case pcommon.ValueTypeStr:
		b.WriteString("s" + strconv.Quote(v.Str()))
	case pcommon.ValueTypeInt:
		b.WriteString("i" + strconv.FormatInt(v.Int(), 10))
	case pcommon.ValueTypeDouble:
		// Bits keep NaN payloads and -0 distinct, as the proto encoding does.
		b.WriteString("d" + strconv.FormatUint(math.Float64bits(v.Double()), 16))
	case pcommon.ValueTypeBool:
		b.WriteString("b" + strconv.FormatBool(v.Bool()))
	case pcommon.ValueTypeBytes:
		b.WriteString("x" + fmt.Sprintf("%x", v.Bytes().AsRaw()))
	case pcommon.ValueTypeMap:
		b.WriteByte('m')
		writeMap(b, v.Map())
	case pcommon.ValueTypeSlice:
		b.WriteString("l[")
		for _, e := range v.Slice().All() {
			writeValue(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	default:
		b.WriteString("n")
	}
}

func spanFields(s ptrace.Span) pcommon.Map {
	f := pcommon.NewMap()
	f.PutStr("trace_id", s.TraceID().String())
	f.PutStr("span_id", s.SpanID().String())
	f.PutStr("parent_span_id", s.ParentSpanID().String())
	f.PutStr("trace_state", s.TraceState().AsRaw())
	f.PutInt("flags", int64(s.Flags()))
	f.PutStr("name", s.Name())
	f.PutStr("kind", s.Kind().String())
	f.PutInt("start_time_unix_nano", int64(s.StartTimestamp()))
	f.PutInt("end_time_unix_nano", int64(s.EndTimestamp()))
	s.Attributes().CopyTo(f.PutEmptyMap("attributes"))
	f.PutInt("dropped_attributes_count", int64(s.DroppedAttributesCount()))
	events := f.PutEmptySlice("events")
	for _, e := range s.Events().All() {
		m := events.AppendEmpty().SetEmptyMap()
		m.PutStr("name", e.Name())
		m.PutInt("time_unix_nano", int64(e.Timestamp()))
		e.Attributes().CopyTo(m.PutEmptyMap("attributes"))
		m.PutInt("dropped_attributes_count", int64(e.DroppedAttributesCount()))
	}
	f.PutInt("dropped_events_count", int64(s.DroppedEventsCount()))
	links := f.PutEmptySlice("links")
	for _, l := range s.Links().All() {
		m := links.AppendEmpty().SetEmptyMap()
		m.PutStr("trace_id", l.TraceID().String())
		m.PutStr("span_id", l.SpanID().String())
		m.PutStr("trace_state", l.TraceState().AsRaw())
		m.PutInt("flags", int64(l.Flags()))
		l.Attributes().CopyTo(m.PutEmptyMap("attributes"))
		m.PutInt("dropped_attributes_count", int64(l.DroppedAttributesCount()))
	}
	f.PutInt("dropped_links_count", int64(s.DroppedLinksCount()))
	f.PutStr("status.code", s.Status().Code().String())
	f.PutStr("status.message", s.Status().Message())
	return f
}

// LogRecordFields flattens a log record.
func LogRecordFields(lr plog.LogRecord) pcommon.Map {
	f := pcommon.NewMap()
	f.PutInt("time_unix_nano", int64(lr.Timestamp()))
	f.PutInt("observed_time_unix_nano", int64(lr.ObservedTimestamp()))
	f.PutInt("severity_number", int64(lr.SeverityNumber()))
	f.PutStr("severity_text", lr.SeverityText())
	f.PutStr("event_name", lr.EventName())
	lr.Body().CopyTo(f.PutEmpty("body"))
	lr.Attributes().CopyTo(f.PutEmptyMap("attributes"))
	f.PutInt("dropped_attributes_count", int64(lr.DroppedAttributesCount()))
	f.PutInt("flags", int64(lr.Flags()))
	f.PutStr("trace_id", lr.TraceID().String())
	f.PutStr("span_id", lr.SpanID().String())
	return f
}

// MetricFields flattens a metric's own fields (not its data points) under
// "metric.*".
func MetricFields(m pmetric.Metric) pcommon.Map {
	f := pcommon.NewMap()
	f.PutStr("metric.name", m.Name())
	f.PutStr("metric.type", m.Type().String())
	f.PutStr("metric.unit", m.Unit())
	f.PutStr("metric.description", m.Description())
	m.Metadata().CopyTo(f.PutEmptyMap("metric.metadata"))
	switch m.Type() {
	case pmetric.MetricTypeSum:
		f.PutStr("metric.aggregation_temporality", m.Sum().AggregationTemporality().String())
		f.PutBool("metric.is_monotonic", m.Sum().IsMonotonic())
	case pmetric.MetricTypeHistogram:
		f.PutStr("metric.aggregation_temporality", m.Histogram().AggregationTemporality().String())
	case pmetric.MetricTypeExponentialHistogram:
		f.PutStr("metric.aggregation_temporality", m.ExponentialHistogram().AggregationTemporality().String())
	}
	return f
}

// PointFields returns one field map per data point of the metric, each
// carrying the metric's fields under "metric.*".
func PointFields(m pmetric.Metric) []pcommon.Map {
	common := func(f pcommon.Map, attrs pcommon.Map, start, ts pcommon.Timestamp, flags pmetric.DataPointFlags) {
		attrs.CopyTo(f.PutEmptyMap("attributes"))
		f.PutInt("start_time_unix_nano", int64(start))
		f.PutInt("time_unix_nano", int64(ts))
		f.PutInt("flags", int64(flags))
	}
	optional := func(f pcommon.Map, name string, has bool, v float64) {
		if has {
			f.PutDouble(name, v)
		}
	}
	var out []pcommon.Map
	switch m.Type() {
	case pmetric.MetricTypeGauge, pmetric.MetricTypeSum:
		var points pmetric.NumberDataPointSlice
		if m.Type() == pmetric.MetricTypeSum {
			points = m.Sum().DataPoints()
		} else {
			points = m.Gauge().DataPoints()
		}
		for _, p := range points.All() {
			f := MetricFields(m)
			common(f, p.Attributes(), p.StartTimestamp(), p.Timestamp(), p.Flags())
			putExemplars(f, p.Exemplars())
			putNumber(f, "value", p)
			out = append(out, f)
		}
	case pmetric.MetricTypeHistogram:
		for _, p := range m.Histogram().DataPoints().All() {
			f := MetricFields(m)
			common(f, p.Attributes(), p.StartTimestamp(), p.Timestamp(), p.Flags())
			putExemplars(f, p.Exemplars())
			f.PutInt("count", int64(p.Count()))
			optional(f, "sum", p.HasSum(), p.Sum())
			optional(f, "min", p.HasMin(), p.Min())
			optional(f, "max", p.HasMax(), p.Max())
			putUints(f.PutEmptySlice("bucket_counts"), p.BucketCounts().AsRaw())
			bounds := f.PutEmptySlice("explicit_bounds")
			for _, b := range p.ExplicitBounds().All() {
				bounds.AppendEmpty().SetDouble(b)
			}
			out = append(out, f)
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, p := range m.ExponentialHistogram().DataPoints().All() {
			f := MetricFields(m)
			common(f, p.Attributes(), p.StartTimestamp(), p.Timestamp(), p.Flags())
			putExemplars(f, p.Exemplars())
			f.PutInt("count", int64(p.Count()))
			optional(f, "sum", p.HasSum(), p.Sum())
			optional(f, "min", p.HasMin(), p.Min())
			optional(f, "max", p.HasMax(), p.Max())
			f.PutInt("scale", int64(p.Scale()))
			f.PutInt("zero_count", int64(p.ZeroCount()))
			f.PutDouble("zero_threshold", p.ZeroThreshold())
			for name, b := range map[string]pmetric.ExponentialHistogramDataPointBuckets{"positive": p.Positive(), "negative": p.Negative()} {
				bm := f.PutEmptyMap(name)
				bm.PutInt("offset", int64(b.Offset()))
				putUints(bm.PutEmptySlice("bucket_counts"), b.BucketCounts().AsRaw())
			}
			out = append(out, f)
		}
	case pmetric.MetricTypeSummary:
		for _, p := range m.Summary().DataPoints().All() {
			f := MetricFields(m)
			common(f, p.Attributes(), p.StartTimestamp(), p.Timestamp(), p.Flags())
			f.PutInt("count", int64(p.Count()))
			f.PutDouble("sum", p.Sum())
			qs := f.PutEmptySlice("quantile_values")
			for _, q := range p.QuantileValues().All() {
				qm := qs.AppendEmpty().SetEmptyMap()
				qm.PutDouble("quantile", q.Quantile())
				qm.PutDouble("value", q.Value())
			}
			out = append(out, f)
		}
	}
	return out
}

func putNumber(f pcommon.Map, name string, p interface {
	ValueType() pmetric.NumberDataPointValueType
	IntValue() int64
	DoubleValue() float64
}) {
	switch p.ValueType() {
	case pmetric.NumberDataPointValueTypeInt:
		f.PutInt(name, p.IntValue())
	case pmetric.NumberDataPointValueTypeDouble:
		f.PutDouble(name, p.DoubleValue())
	}
}

func putExemplars(f pcommon.Map, ex pmetric.ExemplarSlice) {
	s := f.PutEmptySlice("exemplars")
	for _, e := range ex.All() {
		m := s.AppendEmpty().SetEmptyMap()
		m.PutInt("time_unix_nano", int64(e.Timestamp()))
		switch e.ValueType() {
		case pmetric.ExemplarValueTypeInt:
			m.PutInt("value", e.IntValue())
		case pmetric.ExemplarValueTypeDouble:
			m.PutDouble("value", e.DoubleValue())
		}
		m.PutStr("trace_id", e.TraceID().String())
		m.PutStr("span_id", e.SpanID().String())
		e.FilteredAttributes().CopyTo(m.PutEmptyMap("filtered_attributes"))
	}
}

func putUints(s pcommon.Slice, vs []uint64) {
	for _, v := range vs {
		s.AppendEmpty().SetInt(int64(v))
	}
}

// SortResources returns a copy of the payload with its resource entries in a
// canonical order: by the fingerprint of the resource and its records. The
// records themselves are unchanged. It removes delivery-order differences
// between payloads that were concatenated from batches.
func SortResources(p Payload) Payload {
	n := p.Counts().Resources
	keys := make([]string, n)
	for i := range n {
		res, url := p.ResourceAt(i)
		keys[i] = Fingerprint(res.Attributes()) + strconv.Quote(url)
	}
	recs := Records(p)
	fps := ContextFingerprints(p)
	for i, r := range recs {
		keys[r.Resource] += "|" + fps[i]
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int { return strings.Compare(keys[a], keys[b]) })

	out := Empty(p.Signal)
	for _, i := range order {
		switch p.Signal {
		case pipeline.SignalTraces:
			p.Traces.ResourceSpans().At(i).CopyTo(out.Traces.ResourceSpans().AppendEmpty())
		case pipeline.SignalMetrics:
			p.Metrics.ResourceMetrics().At(i).CopyTo(out.Metrics.ResourceMetrics().AppendEmpty())
		case pipeline.SignalLogs:
			p.Logs.ResourceLogs().At(i).CopyTo(out.Logs.ResourceLogs().AppendEmpty())
		}
	}
	return out
}
