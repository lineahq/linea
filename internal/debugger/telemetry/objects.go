package telemetry

import (
	"reflect"
	"sync"
	"unsafe"

	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"
)

// poolingGate is the pdata feature gate that returns removed records'
// objects to a pool for reuse, so an object no longer identifies one record.
const poolingGate = "pdata.useProtoPooling"

// Objects returns the pdata object holding each record of p, in Records
// order, or false when record objects cannot identify records: the pdata
// wrapper layout is not the one this package was verified against, or
// object pooling is enabled.
//
// A processor that changes records in place passes on the same objects it
// received, whatever it changed, so the same object before and after a stage
// is the same record. A record a stage copies is a new object. The pointers
// keep their objects reachable: as long as the caller holds them, no other
// record can take a held object's address.
//
// This reads the unexported object pointer of the pdata record wrappers
// (verified against pdata v1.68.0; see docs/internals/record-objects.md).
func Objects(p Payload) ([]unsafe.Pointer, bool) {
	if !layoutVerified() || pooling() {
		return nil, false
	}
	var out []unsafe.Pointer
	switch p.Signal {
	case pipeline.SignalTraces:
		for _, rs := range p.Traces.ResourceSpans().All() {
			for _, ss := range rs.ScopeSpans().All() {
				for _, s := range ss.Spans().All() {
					out = append(out, object(s))
				}
			}
		}
	case pipeline.SignalLogs:
		for _, rl := range p.Logs.ResourceLogs().All() {
			for _, sl := range rl.ScopeLogs().All() {
				for _, lr := range sl.LogRecords().All() {
					out = append(out, object(lr))
				}
			}
		}
	case pipeline.SignalMetrics:
		for _, rm := range p.Metrics.ResourceMetrics().All() {
			for _, sm := range rm.ScopeMetrics().All() {
				for _, m := range sm.Metrics().All() {
					out = append(out, pointObjects(m)...)
				}
			}
		}
	default:
		return nil, false
	}
	return out, true
}

// pointObjects lists a metric's data point objects in PointFields order.
func pointObjects(m pmetric.Metric) []unsafe.Pointer {
	var out []unsafe.Pointer
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for _, p := range m.Gauge().DataPoints().All() {
			out = append(out, object(p))
		}
	case pmetric.MetricTypeSum:
		for _, p := range m.Sum().DataPoints().All() {
			out = append(out, object(p))
		}
	case pmetric.MetricTypeHistogram:
		for _, p := range m.Histogram().DataPoints().All() {
			out = append(out, object(p))
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, p := range m.ExponentialHistogram().DataPoints().All() {
			out = append(out, object(p))
		}
	case pmetric.MetricTypeSummary:
		for _, p := range m.Summary().DataPoints().All() {
			out = append(out, object(p))
		}
	}
	return out
}

// object reads the wrapper's first field, the pointer to its object; only
// called once layoutVerified holds.
func object[T any](wrapper T) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&wrapper))
}

// layoutVerified reports whether every record wrapper starts with the
// pointer to its object and every record slice holds pointers, so moving a
// record within or between slices keeps its object.
var layoutVerified = sync.OnceValue(func() bool {
	pairs := []struct{ record, slice any }{
		{plog.LogRecord{}, plog.LogRecordSlice{}},
		{ptrace.Span{}, ptrace.SpanSlice{}},
		{pmetric.NumberDataPoint{}, pmetric.NumberDataPointSlice{}},
		{pmetric.HistogramDataPoint{}, pmetric.HistogramDataPointSlice{}},
		{pmetric.ExponentialHistogramDataPoint{}, pmetric.ExponentialHistogramDataPointSlice{}},
		{pmetric.SummaryDataPoint{}, pmetric.SummaryDataPointSlice{}},
	}
	for _, p := range pairs {
		r, s := reflect.TypeOf(p.record), reflect.TypeOf(p.slice)
		if r.NumField() == 0 || s.NumField() == 0 {
			return false
		}
		orig := r.Field(0)
		if orig.Name != "orig" || orig.Offset != 0 || orig.Type.Kind() != reflect.Pointer {
			return false
		}
		elems := s.Field(0)
		if elems.Name != "orig" || elems.Type.Kind() != reflect.Pointer || elems.Type.Elem().Kind() != reflect.Slice ||
			elems.Type.Elem().Elem() != orig.Type {
			return false
		}
	}
	return true
})

// pooling reports whether pdata object pooling is enabled.
func pooling() bool {
	g := poolingFeature()
	return g != nil && g.IsEnabled()
}

// poolingFeature finds the pooling gate once; pdata registers it on import.
var poolingFeature = sync.OnceValue(func() *featuregate.Gate {
	var found *featuregate.Gate
	featuregate.GlobalRegistry().VisitAll(func(g *featuregate.Gate) {
		if g.ID() == poolingGate {
			found = g
		}
	})
	return found
})
