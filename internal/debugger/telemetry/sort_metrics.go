package telemetry

import (
	"fmt"
	"strings"
	"unsafe"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"
)

// SortMetrics returns a copy of a metrics payload whose metrics are sorted
// within each scope by name, type and data points, and whose data points
// are sorted within each metric by attributes, timestamps and value.
// Components that build metrics from maps, such as the count connector
// (v0.162.0), emit them in an order that varies between runs. Other
// signals are returned unchanged.
func SortMetrics(p Payload) Payload {
	if p.Signal != pipeline.SignalMetrics {
		return p
	}
	out := p.Clone()
	sortMetrics(out)
	return out
}

// SortMetricsOrder is SortMetrics that also returns, for each record of the
// sorted payload, the index in Records(p) of the record it was. The order is
// nil when it cannot be told (record objects are unavailable, see Objects)
// and for other signals, which are returned unchanged.
func SortMetricsOrder(p Payload) (Payload, []int) {
	if p.Signal != pipeline.SignalMetrics {
		return p, nil
	}
	out := p.Clone()
	before, ok := Objects(out)
	sortMetrics(out)
	if !ok {
		return out, nil
	}
	after, _ := Objects(out)
	index := make(map[unsafe.Pointer]int, len(before))
	for i, o := range before {
		index[o] = i
	}
	order := make([]int, len(after))
	for i, o := range after {
		j, found := index[o]
		if !found || len(after) != len(before) {
			return out, nil
		}
		order[i] = j
	}
	return out, order
}

// sortMetrics sorts a metrics payload in place, moving its records.
func sortMetrics(out Payload) {
	for _, rm := range out.Metrics.ResourceMetrics().All() {
		for _, sm := range rm.ScopeMetrics().All() {
			keys := map[string]string{}
			for _, m := range sm.Metrics().All() {
				keys[metricIdentity(m)] = sortPoints(m)
			}
			sm.Metrics().Sort(func(a, b pmetric.Metric) bool {
				return keys[metricIdentity(a)] < keys[metricIdentity(b)]
			})
		}
	}
}

// metricIdentity distinguishes the metrics of one scope while sorting;
// metrics equal in it compare equal, which the stable sort keeps in order.
func metricIdentity(m pmetric.Metric) string {
	return m.Name() + "\x00" + m.Type().String() + "\x00" + m.Unit() + "\x00" + m.Description()
}

// sortPoints sorts a metric's data points and returns the metric's sort
// key.
func sortPoints(m pmetric.Metric) string {
	var keys []string
	switch m.Type() {
	case pmetric.MetricTypeGauge, pmetric.MetricTypeSum:
		var dps pmetric.NumberDataPointSlice
		if m.Type() == pmetric.MetricTypeSum {
			dps = m.Sum().DataPoints()
		} else {
			dps = m.Gauge().DataPoints()
		}
		dps.Sort(func(a, b pmetric.NumberDataPoint) bool { return numberKey(a) < numberKey(b) })
		for _, p := range dps.All() {
			keys = append(keys, numberKey(p))
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		key := func(p pmetric.HistogramDataPoint) string {
			return pointKey(p.Attributes(), p.StartTimestamp(), p.Timestamp(), fmt.Sprint(p.Count(), p.Sum(), p.BucketCounts().AsRaw()))
		}
		dps.Sort(func(a, b pmetric.HistogramDataPoint) bool { return key(a) < key(b) })
		for _, p := range dps.All() {
			keys = append(keys, key(p))
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		key := func(p pmetric.ExponentialHistogramDataPoint) string {
			return pointKey(p.Attributes(), p.StartTimestamp(), p.Timestamp(), fmt.Sprint(p.Count(), p.Sum(), p.Scale()))
		}
		dps.Sort(func(a, b pmetric.ExponentialHistogramDataPoint) bool { return key(a) < key(b) })
		for _, p := range dps.All() {
			keys = append(keys, key(p))
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		key := func(p pmetric.SummaryDataPoint) string {
			return pointKey(p.Attributes(), p.StartTimestamp(), p.Timestamp(), fmt.Sprint(p.Count(), p.Sum()))
		}
		dps.Sort(func(a, b pmetric.SummaryDataPoint) bool { return key(a) < key(b) })
		for _, p := range dps.All() {
			keys = append(keys, key(p))
		}
	}
	return metricIdentity(m) + "\x00" + strings.Join(keys, "\x01")
}

func numberKey(p pmetric.NumberDataPoint) string {
	v := fmt.Sprint(p.IntValue())
	if p.ValueType() == pmetric.NumberDataPointValueTypeDouble {
		v = fmt.Sprint(p.DoubleValue())
	}
	return pointKey(p.Attributes(), p.StartTimestamp(), p.Timestamp(), v)
}

func pointKey(attrs pcommon.Map, start, ts pcommon.Timestamp, value string) string {
	return Fingerprint(attrs) + fmt.Sprintf("|%020d|%020d|", uint64(start), uint64(ts)) + value
}
