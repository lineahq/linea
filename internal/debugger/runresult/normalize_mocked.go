package runresult

import (
	"slices"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// NormalizedInstanceID replaces the collector.instance.id data point
// attribute. The spanmetrics connector adds it with a random UUID per run
// unless the Collector's resource sets one.
const NormalizedInstanceID = "instance-normalized"

const collectorInstanceID = "collector.instance.id"

// NormalizeMocked removes what differs between two runs of the same
// scenario against mocks: components running against a mock order the
// attributes they add arbitrarily (built from Go maps: the filelog
// receiver's stanza entries, the k8sattributes tags), the filelog receiver
// stamps observed times from the clock, and the spanmetrics connector adds
// a random collector.instance.id. It sorts all resource, scope, item and
// data point attributes by key, clears the observed timestamps of log
// records and replaces collector.instance.id on data points, in place.
func NormalizeMocked(p telemetry.Payload) {
	switch p.Signal {
	case pipeline.SignalTraces:
		rss := p.Traces.ResourceSpans()
		for i := range rss.Len() {
			sortAttributes(rss.At(i).Resource().Attributes())
			sss := rss.At(i).ScopeSpans()
			for j := range sss.Len() {
				sortAttributes(sss.At(j).Scope().Attributes())
				spans := sss.At(j).Spans()
				for k := range spans.Len() {
					sortAttributes(spans.At(k).Attributes())
				}
			}
		}
	case pipeline.SignalMetrics:
		rms := p.Metrics.ResourceMetrics()
		for i := range rms.Len() {
			sortAttributes(rms.At(i).Resource().Attributes())
			sms := rms.At(i).ScopeMetrics()
			for j := range sms.Len() {
				sortAttributes(sms.At(j).Scope().Attributes())
				ms := sms.At(j).Metrics()
				for k := range ms.Len() {
					for _, attrs := range pointAttributes(ms.At(k)) {
						if _, ok := attrs.Get(collectorInstanceID); ok {
							attrs.PutStr(collectorInstanceID, NormalizedInstanceID)
						}
						sortAttributes(attrs)
					}
				}
			}
		}
	case pipeline.SignalLogs:
		rls := p.Logs.ResourceLogs()
		for i := range rls.Len() {
			sortAttributes(rls.At(i).Resource().Attributes())
			sls := rls.At(i).ScopeLogs()
			for j := range sls.Len() {
				sortAttributes(sls.At(j).Scope().Attributes())
				lrs := sls.At(j).LogRecords()
				for k := range lrs.Len() {
					lrs.At(k).SetObservedTimestamp(pcommon.Timestamp(0))
					sortAttributes(lrs.At(k).Attributes())
				}
			}
		}
	}
}

// sortAttributes reorders m, and the maps nested in it, by key.
func sortAttributes(m pcommon.Map) {
	orig := pcommon.NewMap()
	m.MoveTo(orig)
	keys := make([]string, 0, orig.Len())
	for k := range orig.All() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, _ := orig.Get(k)
		dst := m.PutEmpty(k)
		v.MoveTo(dst)
		if dst.Type() == pcommon.ValueTypeMap {
			sortAttributes(dst.Map())
		}
	}
}

// pointAttributes returns the attributes of each data point of m.
func pointAttributes(m pmetric.Metric) []pcommon.Map {
	var out []pcommon.Map
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for _, dp := range m.Gauge().DataPoints().All() {
			out = append(out, dp.Attributes())
		}
	case pmetric.MetricTypeSum:
		for _, dp := range m.Sum().DataPoints().All() {
			out = append(out, dp.Attributes())
		}
	case pmetric.MetricTypeHistogram:
		for _, dp := range m.Histogram().DataPoints().All() {
			out = append(out, dp.Attributes())
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, dp := range m.ExponentialHistogram().DataPoints().All() {
			out = append(out, dp.Attributes())
		}
	case pmetric.MetricTypeSummary:
		for _, dp := range m.Summary().DataPoints().All() {
			out = append(out, dp.Attributes())
		}
	}
	return out
}
