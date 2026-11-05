package livecaptureextension

import (
	"context"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// The shapes below follow the Collector Contrib testbed's perf data provider
// (collector/tools/testbed): batches of 100 spans or log records, or 100
// gauges of 7 data points, so these benchmarks profile what the testbed's
// CaptureAll and CaptureFilter10pct cases measure end to end.

func perfTraces() ptrace.Traces {
	data := ptrace.NewTraces()
	spans := data.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
	now := time.Now()
	for i := range 100 {
		span := spans.AppendEmpty()
		span.SetTraceID(pcommon.TraceID{1, byte(i)})
		span.SetSpanID(pcommon.SpanID{byte(i), 1})
		span.SetName("load-generator-span" + strconv.Itoa(i+1000))
		span.SetKind(ptrace.SpanKindClient)
		span.Attributes().PutInt("load_generator.span_seq_num", int64(i))
		span.Attributes().PutInt("load_generator.trace_seq_num", 1)
		span.Attributes().PutBool("linea.selected", i%10 == 0)
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Millisecond)))
	}
	return data
}

func perfLogs() plog.Logs {
	data := plog.NewLogs()
	records := data.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	now := pcommon.NewTimestampFromTime(time.Now())
	for i := range 100 {
		record := records.AppendEmpty()
		record.SetTimestamp(now)
		record.SetSeverityNumber(plog.SeverityNumberInfo3)
		record.SetSeverityText("INFO3")
		record.Body().SetStr("Load Generator Counter #" + strconv.Itoa(i))
		record.Attributes().PutStr("dur_ms", "50")
		record.Attributes().PutStr("flags", "FIXED")
		record.Attributes().PutInt("load_generator.item_seq_num", int64(i))
		record.Attributes().PutBool("linea.selected", i%10 == 0)
	}
	return data
}

func perfMetrics() pmetric.Metrics {
	data := pmetric.NewMetrics()
	metrics := data.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	now := pcommon.NewTimestampFromTime(time.Now())
	n := 0
	for i := range 100 {
		metric := metrics.AppendEmpty()
		metric.SetName("load_generator_" + strconv.Itoa(i))
		metric.SetDescription("Load Generator Counter #" + strconv.Itoa(i))
		metric.SetUnit("1")
		points := metric.SetEmptyGauge().DataPoints()
		for j := range 7 {
			point := points.AppendEmpty()
			point.SetStartTimestamp(now)
			point.SetIntValue(int64(n))
			point.Attributes().PutStr("item_index", "item_"+strconv.Itoa(j))
			point.Attributes().PutStr("batch_index", "batch_"+strconv.Itoa(i))
			point.Attributes().PutBool("linea.selected", n%10 == 0)
			n++
		}
	}
	return data
}

// BenchmarkCaptureShapes captures testbed-shaped batches into a discarding
// delivery. It reports ns per captured-or-evaluated record.
func BenchmarkCaptureShapes(b *testing.B) {
	filters := []struct {
		name   string
		filter string
	}{
		{"all", ""},
		{"filter_10pct", `attributes["linea.selected"] == true`},
		{"filter_none", `attributes["linea.selected"] == "never"`},
	}
	for _, f := range filters {
		b.Run("traces/"+f.name, func(b *testing.B) {
			m := benchManager(b)
			r := traceRequest("shape")
			if f.filter != "" {
				r.Filter.Traces.Span = []string{f.filter}
			}
			start(b, m, r)
			data := perfTraces()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*100), "ns/record")
		})
		b.Run("logs/"+f.name, func(b *testing.B) {
			m := benchManager(b)
			r := Request{SessionID: "shape", Principal: "bench", Signals: []Signal{Logs}, CapturePoints: []Point{{Pipeline: "logs", Point: "ingress"}}}
			if f.filter != "" {
				r.Filter.Logs.Log = []string{f.filter}
			}
			start(b, m, r)
			data := perfLogs()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.CaptureLogs(context.Background(), r.CapturePoints[0], data)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*100), "ns/record")
		})
		b.Run("metrics/"+f.name, func(b *testing.B) {
			m := benchManager(b)
			r := Request{SessionID: "shape", Principal: "bench", Signals: []Signal{Metrics}, CapturePoints: []Point{{Pipeline: "metrics", Point: "ingress"}}}
			if f.filter != "" {
				r.Filter.Metrics.Datapoint = []string{f.filter}
			}
			start(b, m, r)
			data := perfMetrics()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.CaptureMetrics(context.Background(), r.CapturePoints[0], data)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*700), "ns/record")
		})
	}
}

func benchManager(b *testing.B) *Manager {
	m, _ := testManager(b)
	m.config.Limits.MaxBytesPerSecond = 1 << 60
	m.config.Limits.MaxRecordsPerSecond = 1 << 60
	m.config.Limits.MaxTotalBytes = 1 << 60
	m.config.Limits.MaxTotalRecords = 1 << 60
	m.delivery = discardDelivery{}
	return m
}
