package telemetry

import (
	"fmt"
	"iter"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"
)

// CaptureRecords yields positions without copying values into comparison maps.
// Ingress can enforce its budget before copying each individual record.
func CaptureRecords(p Payload) iter.Seq[Record] {
	return func(yield func(Record) bool) {
		switch p.Signal {
		case pipeline.SignalTraces:
			for i, resource := range p.Traces.ResourceSpans().All() {
				for j, scope := range resource.ScopeSpans().All() {
					for k := 0; k < scope.Spans().Len(); k++ {
						if !yield(Record{Resource: i, Scope: j, Metric: -1, Index: k}) {
							return
						}
					}
				}
			}
		case pipeline.SignalLogs:
			for i, resource := range p.Logs.ResourceLogs().All() {
				for j, scope := range resource.ScopeLogs().All() {
					for k := 0; k < scope.LogRecords().Len(); k++ {
						if !yield(Record{Resource: i, Scope: j, Metric: -1, Index: k}) {
							return
						}
					}
				}
			}
		case pipeline.SignalMetrics:
			for i, resource := range p.Metrics.ResourceMetrics().All() {
				for j, scope := range resource.ScopeMetrics().All() {
					for m, metric := range scope.Metrics().All() {
						n := 0
						switch metric.Type() {
						case pmetric.MetricTypeGauge:
							n = metric.Gauge().DataPoints().Len()
						case pmetric.MetricTypeSum:
							n = metric.Sum().DataPoints().Len()
						case pmetric.MetricTypeHistogram:
							n = metric.Histogram().DataPoints().Len()
						case pmetric.MetricTypeExponentialHistogram:
							n = metric.ExponentialHistogram().DataPoints().Len()
						case pmetric.MetricTypeSummary:
							n = metric.Summary().DataPoints().Len()
						}
						for k := 0; k < n; k++ {
							if !yield(Record{Resource: i, Scope: j, Metric: m, Index: k}) {
								return
							}
						}
					}
				}
			}
		}
	}
}

// DecodeProto decodes an OTLP protobuf request of a signal.
func DecodeProto(signal string, raw []byte) (Payload, error) {
	switch signal {
	case "traces":
		v, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(raw)
		return FromTraces(v), err
	case "logs":
		v, err := (&plog.ProtoUnmarshaler{}).UnmarshalLogs(raw)
		return FromLogs(v), err
	case "metrics":
		v, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(raw)
		return FromMetrics(v), err
	}
	return Payload{}, fmt.Errorf("unsupported capture signal")
}

// Proto encodes the payload as an OTLP protobuf request.
func (p Payload) Proto() ([]byte, error) {
	switch p.Signal {
	case pipeline.SignalTraces:
		return (&ptrace.ProtoMarshaler{}).MarshalTraces(p.Traces)
	case pipeline.SignalLogs:
		return (&plog.ProtoMarshaler{}).MarshalLogs(p.Logs)
	case pipeline.SignalMetrics:
		return (&pmetric.ProtoMarshaler{}).MarshalMetrics(p.Metrics)
	}
	return nil, fmt.Errorf("unsupported capture signal")
}

// RecordPayload copies exactly one recorded item with complete resource/scope,
// schema URLs and metric metadata. Values, int64s, exemplars and timestamps are
// copied through pdata, never round-tripped through interface{} or floats.
func RecordPayload(p Payload, r Record) Payload {
	resource, schema := p.ResourceAt(r.Resource)
	switch p.Signal {
	case pipeline.SignalTraces:
		out := ptrace.NewTraces()
		dst := out.ResourceSpans().AppendEmpty()
		resource.CopyTo(dst.Resource())
		dst.SetSchemaUrl(schema)
		src := p.Traces.ResourceSpans().At(r.Resource).ScopeSpans().At(r.Scope)
		scope := dst.ScopeSpans().AppendEmpty()
		src.Scope().CopyTo(scope.Scope())
		scope.SetSchemaUrl(src.SchemaUrl())
		src.Spans().At(r.Index).CopyTo(scope.Spans().AppendEmpty())
		return FromTraces(out)
	case pipeline.SignalLogs:
		out := plog.NewLogs()
		dst := out.ResourceLogs().AppendEmpty()
		resource.CopyTo(dst.Resource())
		dst.SetSchemaUrl(schema)
		src := p.Logs.ResourceLogs().At(r.Resource).ScopeLogs().At(r.Scope)
		scope := dst.ScopeLogs().AppendEmpty()
		src.Scope().CopyTo(scope.Scope())
		scope.SetSchemaUrl(src.SchemaUrl())
		src.LogRecords().At(r.Index).CopyTo(scope.LogRecords().AppendEmpty())
		return FromLogs(out)
	default:
		out := pmetric.NewMetrics()
		dst := out.ResourceMetrics().AppendEmpty()
		resource.CopyTo(dst.Resource())
		dst.SetSchemaUrl(schema)
		src := p.Metrics.ResourceMetrics().At(r.Resource).ScopeMetrics().At(r.Scope)
		scope := dst.ScopeMetrics().AppendEmpty()
		src.Scope().CopyTo(scope.Scope())
		scope.SetSchemaUrl(src.SchemaUrl())
		metric := scope.Metrics().AppendEmpty()
		original := src.Metrics().At(r.Metric)
		metric.SetName(original.Name())
		metric.SetDescription(original.Description())
		metric.SetUnit(original.Unit())
		original.Metadata().CopyTo(metric.Metadata())
		switch original.Type() {
		case pmetric.MetricTypeGauge:
			original.Gauge().DataPoints().At(r.Index).CopyTo(metric.SetEmptyGauge().DataPoints().AppendEmpty())
		case pmetric.MetricTypeSum:
			dst := metric.SetEmptySum()
			dst.SetAggregationTemporality(original.Sum().AggregationTemporality())
			dst.SetIsMonotonic(original.Sum().IsMonotonic())
			original.Sum().DataPoints().At(r.Index).CopyTo(dst.DataPoints().AppendEmpty())
		case pmetric.MetricTypeHistogram:
			dst := metric.SetEmptyHistogram()
			dst.SetAggregationTemporality(original.Histogram().AggregationTemporality())
			original.Histogram().DataPoints().At(r.Index).CopyTo(dst.DataPoints().AppendEmpty())
		case pmetric.MetricTypeExponentialHistogram:
			dst := metric.SetEmptyExponentialHistogram()
			dst.SetAggregationTemporality(original.ExponentialHistogram().AggregationTemporality())
			original.ExponentialHistogram().DataPoints().At(r.Index).CopyTo(dst.DataPoints().AppendEmpty())
		case pmetric.MetricTypeSummary:
			original.Summary().DataPoints().At(r.Index).CopyTo(metric.SetEmptySummary().DataPoints().AppendEmpty())
		}
		return FromMetrics(out)
	}
}

// CaptureTagKeys returns the resource attributes the Collector extension
// adds to captured records.
func CaptureTagKeys() [5]string {
	return [5]string{"capture.session.id", "capture.point", "capture.pipeline", "capture.collector.pod", "capture.cluster"}
}

// CaptureTag returns a string capture tag of a resource.
func CaptureTag(attributes pcommon.Map, key string) (string, bool) {
	v, ok := attributes.Get(key)
	if !ok || v.Type() != pcommon.ValueTypeStr {
		return "", false
	}
	return v.Str(), true
}
