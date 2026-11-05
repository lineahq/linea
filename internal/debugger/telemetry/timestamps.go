package telemetry

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"
)

// eachTimestamp calls f with every set record timestamp of p and a setter
// for it: span start, end and event times, log record time and observed
// time, data point start time and time, and exemplar times. Unset (zero)
// timestamps are skipped.
func (p Payload) eachTimestamp(f func(t pcommon.Timestamp, set func(pcommon.Timestamp))) {
	do := func(t pcommon.Timestamp, set func(pcommon.Timestamp)) {
		if t != 0 {
			f(t, set)
		}
	}
	switch p.Signal {
	case pipeline.SignalTraces:
		for _, rs := range p.Traces.ResourceSpans().All() {
			for _, ss := range rs.ScopeSpans().All() {
				for _, s := range ss.Spans().All() {
					do(s.StartTimestamp(), s.SetStartTimestamp)
					do(s.EndTimestamp(), s.SetEndTimestamp)
					for _, e := range s.Events().All() {
						do(e.Timestamp(), e.SetTimestamp)
					}
				}
			}
		}
	case pipeline.SignalLogs:
		for _, rl := range p.Logs.ResourceLogs().All() {
			for _, sl := range rl.ScopeLogs().All() {
				for _, l := range sl.LogRecords().All() {
					do(l.Timestamp(), l.SetTimestamp)
					do(l.ObservedTimestamp(), l.SetObservedTimestamp)
				}
			}
		}
	case pipeline.SignalMetrics:
		for _, rm := range p.Metrics.ResourceMetrics().All() {
			for _, sm := range rm.ScopeMetrics().All() {
				for _, m := range sm.Metrics().All() {
					eachPointTimestamp(m, do)
				}
			}
		}
	}
}

func eachPointTimestamp(m pmetric.Metric, do func(pcommon.Timestamp, func(pcommon.Timestamp))) {
	exemplars := func(ex pmetric.ExemplarSlice) {
		for _, e := range ex.All() {
			do(e.Timestamp(), e.SetTimestamp)
		}
	}
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		for _, dp := range m.Gauge().DataPoints().All() {
			do(dp.StartTimestamp(), dp.SetStartTimestamp)
			do(dp.Timestamp(), dp.SetTimestamp)
			exemplars(dp.Exemplars())
		}
	case pmetric.MetricTypeSum:
		for _, dp := range m.Sum().DataPoints().All() {
			do(dp.StartTimestamp(), dp.SetStartTimestamp)
			do(dp.Timestamp(), dp.SetTimestamp)
			exemplars(dp.Exemplars())
		}
	case pmetric.MetricTypeHistogram:
		for _, dp := range m.Histogram().DataPoints().All() {
			do(dp.StartTimestamp(), dp.SetStartTimestamp)
			do(dp.Timestamp(), dp.SetTimestamp)
			exemplars(dp.Exemplars())
		}
	case pmetric.MetricTypeExponentialHistogram:
		for _, dp := range m.ExponentialHistogram().DataPoints().All() {
			do(dp.StartTimestamp(), dp.SetStartTimestamp)
			do(dp.Timestamp(), dp.SetTimestamp)
			exemplars(dp.Exemplars())
		}
	case pmetric.MetricTypeSummary:
		for _, dp := range m.Summary().DataPoints().All() {
			do(dp.StartTimestamp(), dp.SetStartTimestamp)
			do(dp.Timestamp(), dp.SetTimestamp)
		}
	}
}

// Latest returns the latest record timestamp of the payloads, or the zero
// time when none is set.
func Latest(payloads ...Payload) time.Time {
	var latest pcommon.Timestamp
	for _, p := range payloads {
		p.eachTimestamp(func(t pcommon.Timestamp, _ func(pcommon.Timestamp)) { latest = max(latest, t) })
	}
	if latest == 0 {
		return time.Time{}
	}
	return latest.AsTime()
}

// Shift adds d to every set record timestamp of p, in place. Unset
// timestamps stay unset; a result before the Unix epoch is clamped to 1ns
// so that it stays set.
func (p Payload) Shift(d time.Duration) {
	if d == 0 {
		return
	}
	p.eachTimestamp(func(t pcommon.Timestamp, set func(pcommon.Timestamp)) {
		v := int64(t) + int64(d)
		if v <= 0 {
			v = 1
		}
		set(pcommon.Timestamp(v))
	})
}
