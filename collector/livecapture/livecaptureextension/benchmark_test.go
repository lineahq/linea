package livecaptureextension

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func selectedCopy(data ptrace.Traces) ptrace.Traces {
	copy := ptrace.NewTraces()
	sourceResource := data.ResourceSpans().At(0)
	resource := copy.ResourceSpans().AppendEmpty()
	sourceResource.Resource().CopyTo(resource.Resource())
	resource.SetSchemaUrl(sourceResource.SchemaUrl())
	sourceScope := sourceResource.ScopeSpans().At(0)
	scope := resource.ScopeSpans().AppendEmpty()
	sourceScope.Scope().CopyTo(scope.Scope())
	scope.SetSchemaUrl(sourceScope.SchemaUrl())
	sourceScope.Spans().At(0).CopyTo(scope.Spans().AppendEmpty())
	return copy
}

type discardDelivery struct{}

func (discardDelivery) ensure([]Signal) error                          { return nil }
func (discardDelivery) traces(context.Context, ptrace.Traces) error    { return nil }
func (discardDelivery) logs(context.Context, plog.Logs) error          { return nil }
func (discardDelivery) metrics(context.Context, pmetric.Metrics) error { return nil }
func (discardDelivery) shutdown(context.Context) error                 { return nil }

func BenchmarkSelection(b *testing.B) {
	for _, percent := range []int{0, 1, 10, 100} {
		b.Run(fmt.Sprintf("percent_%d", percent), func(b *testing.B) {
			m, _ := testManager(b)
			m.config.Limits.MaxBytesPerSecond = 1 << 60
			m.config.Limits.MaxRecordsPerSecond = 1 << 60
			m.config.Limits.MaxTotalBytes = 1 << 60
			m.config.Limits.MaxTotalRecords = 1 << 60
			m.delivery = discardDelivery{}
			r := traceRequest("selection-benchmark")
			r.Filter.Traces.Span = []string{`attributes["selected"] == true`}
			start(b, m, r)
			data := ptrace.NewTraces()
			resource := data.ResourceSpans().AppendEmpty()
			resource.Resource().Attributes().PutStr("service.name", "benchmark")
			scope := resource.ScopeSpans().AppendEmpty()
			scope.Scope().SetName("benchmark")
			for i := range 100 {
				span := scope.Spans().AppendEmpty()
				span.SetName("synthetic-span")
				span.SetTraceID(pcommon.TraceID{byte(i)})
				span.SetSpanID(pcommon.SpanID{byte(i)})
				span.Attributes().PutBool("selected", i < percent)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
			}
		})
	}
}

// This measures a full stock byte queue under outage with distinct ~4 KiB
// records. Retained heap includes pdata, queue bookkeeping, and gRPC overhead;
// it is not a universal multiplier for other telemetry shapes.
func BenchmarkQueueOutage(b *testing.B) {
	m, _ := testManager(b)
	m.host = testHost{}
	m.config.Limits.MaxBytesPerSecond = 256 << 20
	m.config.Limits.MaxRecordsPerSecond = 100000
	m.delivery = newOTLPDelivery(m)
	r := traceRequest("outage-benchmark")
	s := start(b, m, r)
	data := selectedCopy(testTraces())
	span := data.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	payload := strings.Repeat("x", 4000)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range 5000 {
		span.Attributes().PutStr("payload", fmt.Sprintf("%06d-%s", i, payload))
		m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
		if s.stats().Dropped["queue_full"] > 0 {
			break
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	queueBytes := m.settings.MeterProvider.(queueMeterProvider).queues.sum(context.Background())
	if queueBytes <= 0 || queueBytes > int64(m.config.Limits.MaxQueueBytes) {
		b.Fatalf("invalid queue occupancy %d", queueBytes)
	}
	heapBytes := max(int64(after.HeapAlloc)-int64(before.HeapAlloc), 0)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
	}
	b.StopTimer()
	b.ReportMetric(float64(queueBytes), "queue_bytes")
	b.ReportMetric(float64(heapBytes), "retained_heap_bytes")
}

func BenchmarkSelectedCopy(b *testing.B) {
	data := testTraces()
	b.ReportAllocs()
	for range b.N {
		_ = selectedCopy(data)
	}
}

func BenchmarkQueueShapes(b *testing.B) {
	for _, payloadSize := range []int{128, 4000} {
		for _, signalCount := range []int{1, 3} {
			b.Run(fmt.Sprintf("payload_%d_signals_%d", payloadSize, signalCount), func(b *testing.B) {
				m, _ := testManager(b)
				m.host = testHost{}
				m.config.Limits.MaxBytesPerSecond = 1 << 60
				m.config.Limits.MaxRecordsPerSecond = 1 << 60
				m.config.Limits.MaxTotalBytes = 1 << 60
				m.config.Limits.MaxTotalRecords = 1 << 60
				m.delivery = newOTLPDelivery(m)
				r := traceRequest("queue-shape")
				if signalCount == 3 {
					r.Signals = []Signal{Traces, Logs, Metrics}
					r.CapturePoints = []Point{{Pipeline: "traces", Point: "ingress"}, {Pipeline: "logs", Point: "ingress"}, {Pipeline: "metrics", Point: "ingress"}}
				}
				s := start(b, m, r)
				data := selectedCopy(testTraces())
				span := data.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
				payload := strings.Repeat("x", payloadSize)
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for i := range 100000 {
					span.Attributes().PutStr("payload", fmt.Sprintf("%06d-%s", i, payload))
					m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
					if s.stats().Dropped["queue_full"] > 0 {
						break
					}
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				queueBytes := m.settings.MeterProvider.(queueMeterProvider).queues.sum(context.Background())
				if queueBytes <= 0 || queueBytes > int64(m.config.Limits.MaxQueueBytes)/3 {
					b.Fatalf("invalid partition occupancy %d", queueBytes)
				}
				heapBytes := max(int64(after.HeapAlloc)-int64(before.HeapAlloc), 0)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
				}
				b.StopTimer()
				b.ReportMetric(float64(queueBytes), "queue_bytes")
				b.ReportMetric(float64(heapBytes), "retained_heap_bytes")
			})
		}
	}
}
func BenchmarkSelectedProtoRoundtrip(b *testing.B) {
	data := testTraces()
	marshaler := ptrace.ProtoMarshaler{}
	unmarshaler := ptrace.ProtoUnmarshaler{}
	b.ReportAllocs()
	for range b.N {
		encoded, err := marshaler.MarshalTraces(selectedCopy(data))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := unmarshaler.UnmarshalTraces(encoded); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkActiveNoMatch(b *testing.B) {
	m, _ := testManager(b)
	r := traceRequest("benchmark")
	r.Filter.Traces.Span = []string{`name == "never"`}
	start(b, m, r)
	data := testTraces()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
	}
}
