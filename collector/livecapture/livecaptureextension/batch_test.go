package livecaptureextension

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// tracesWith builds resources × scopes × spans with distinct span names.
func tracesWith(resources, scopes, spans int) ptrace.Traces {
	data := ptrace.NewTraces()
	for r := range resources {
		rs := data.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", fmt.Sprintf("service-%d", r))
		for s := range scopes {
			ss := rs.ScopeSpans().AppendEmpty()
			ss.Scope().SetName(fmt.Sprintf("scope-%d", s))
			for i := range spans {
				span := ss.Spans().AppendEmpty()
				span.SetName(fmt.Sprintf("span-%d-%d-%d", r, s, i))
				span.SetTraceID(pcommon.TraceID{byte(r), byte(s), byte(i)})
			}
		}
	}
	return data
}

func spanNames(data ptrace.Traces) []string {
	var names []string
	for _, rs := range data.ResourceSpans().All() {
		for _, ss := range rs.ScopeSpans().All() {
			for _, span := range ss.Spans().All() {
				names = append(names, span.Name())
			}
		}
	}
	return names
}

// One capture call is one export: a single copy grouped like the original,
// truncated to the admissible prefix, with exact byte accounting.
func TestBatchAdmissionKeepsGroupedPrefix(t *testing.T) {
	m, sink := testManager(t)
	r := traceRequest("batch")
	r.Limits.RecordsPerSecond = 5
	s := start(t, m, r)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], tracesWith(2, 2, 2))
	if len(sink.trace) != 1 {
		t.Fatalf("exports=%d, want one per capture call", len(sink.trace))
	}
	copy := sink.trace[0]
	if got := strings.Join(spanNames(copy), ","); got != "span-0-0-0,span-0-0-1,span-0-1-0,span-0-1-1,span-1-0-0" {
		t.Fatalf("kept %s", got)
	}
	if copy.ResourceSpans().Len() != 2 || copy.ResourceSpans().At(1).ScopeSpans().Len() != 1 {
		t.Fatal("copy not grouped by resource and scope, or empty groups kept")
	}
	for _, rs := range copy.ResourceSpans().All() {
		if value, _ := rs.Resource().Attributes().Get("capture.session.id"); value.Str() != r.SessionID {
			t.Fatal("resource group not tagged")
		}
	}
	stats := s.stats()
	if stats.Selected != 8 || stats.EnqueuedRecords != 5 || stats.Dropped["rate_records"] != 3 {
		t.Fatalf("stats %+v", stats)
	}
	if stats.EnqueuedBytes != int64((&ptrace.ProtoMarshaler{}).TracesSize(copy)) {
		t.Fatal("byte accounting differs from the encoded copy")
	}
}

// Size estimates bound admission from above, so the byte budget holds
// exactly however records are batched.
func TestByteBudgetIsNeverExceeded(t *testing.T) {
	m, sink := testManager(t)
	r := traceRequest("bytes")
	r.Limits.TotalBytes = 1500
	s := start(t, m, r)
	for i := 0; i < 50 && !s.stopped.Load(); i++ {
		m.CaptureTraces(context.Background(), r.CapturePoints[0], tracesWith(1+i%2, 1+i%3, 1+i%4))
	}
	<-s.done
	var sent int64
	for _, data := range sink.trace {
		sent += int64((&ptrace.ProtoMarshaler{}).TracesSize(data))
	}
	stats := s.stats()
	if sent != stats.EnqueuedBytes || sent > r.Limits.TotalBytes || stats.Dropped["budget"] == 0 || s.termination.Load().state != "limit_reached" {
		t.Fatalf("sent %d bytes, stats %+v", sent, stats)
	}
}

func TestMetricBatchPrunesAcrossMetrics(t *testing.T) {
	m, sink := testManager(t)
	r := Request{SessionID: "metric-batch", Principal: "engineer", Signals: []Signal{Metrics}, CapturePoints: []Point{{Pipeline: "metrics", Point: "ingress"}}}
	r.Limits.RecordsPerSecond = 3
	s := start(t, m, r)
	data := metricWithPoints(pmetric.MetricTypeSum)
	metricWithPoints(pmetric.MetricTypeHistogram).ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).CopyTo(data.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().AppendEmpty())
	m.CaptureMetrics(context.Background(), r.CapturePoints[0], data)
	if len(sink.metric) != 1 {
		t.Fatalf("exports=%d", len(sink.metric))
	}
	metrics := sink.metric[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	if metrics.Len() != 2 || metrics.At(0).Sum().DataPoints().Len() != 2 || metrics.At(1).Histogram().DataPoints().Len() != 1 {
		t.Fatal("datapoint prefix not kept per metric")
	}
	if s.stats().Dropped["rate_records"] != 1 || s.stats().EnqueuedBytes != int64((&pmetric.ProtoMarshaler{}).MetricsSize(sink.metric[0])) {
		t.Fatalf("stats %+v", s.stats())
	}
}

// Updating a session swaps its configuration in place: capture never sees a
// gap and nothing is dropped as stopping.
func TestUpdateNeverPausesCapture(t *testing.T) {
	m, _ := testManager(t)
	r := traceRequest("hot-update")
	s := start(t, m, r)
	var done atomic.Bool
	var gaps atomic.Int64
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for !done.Load() {
				if m.active.Load() == nil {
					gaps.Add(1)
				}
				m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
			}
		}()
	}
	for i := range 20 {
		updated := r
		updated.Filter.Traces.Span = []string{fmt.Sprintf(`attributes["number"] >= %d`, i%3)}
		if _, err := m.StartSession(context.Background(), updated, Access{}); err != nil {
			t.Fatal(err)
		}
	}
	done.Store(true)
	workers.Wait()
	if gaps.Load() != 0 || s.stats().Dropped["stopping"] != 0 || s.stopped.Load() {
		t.Fatalf("update paused capture: gaps=%d stats=%+v", gaps.Load(), s.stats())
	}
}

func TestUpdateCanBringDeadlineForward(t *testing.T) {
	m, _ := testManager(t)
	r := traceRequest("shorten")
	r.ExpiresAt = time.Now().Add(time.Minute)
	s := start(t, m, r)
	r.ExpiresAt = time.Now().Add(50 * time.Millisecond)
	if _, err := m.StartSession(context.Background(), r, Access{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("earlier deadline from an update was ignored")
	}
	if s.termination.Load().state != "expired" {
		t.Fatal(s.termination.Load())
	}
}

// Log sampling hashes the original record, so pods with different identity
// and capture points keep the same records.
func TestLogSamplingDecidesOnOriginal(t *testing.T) {
	a, sinkA := testManager(t)
	b, sinkB := testManager(t)
	b.config.Pod = "pod-b"
	b.redactor.config.MaxStringLength = 4
	r := Request{SessionID: "log-sampling", Principal: "engineer", Signals: []Signal{Logs}, CapturePoints: []Point{{Pipeline: "logs", Point: "ingress"}}, Sampling: &Sampling{Rate: .25}}
	start(t, a, r)
	start(t, b, r)
	data := plog.NewLogs()
	logs := data.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	for i := range 2000 {
		log := logs.AppendEmpty()
		log.SetTimestamp(pcommon.Timestamp(1_700_000_000_000_000_000 + i))
		log.Body().SetStr("request handled")
		log.Attributes().PutInt("sequence", int64(i))
	}
	a.CaptureLogs(context.Background(), r.CapturePoints[0], data)
	b.CaptureLogs(context.Background(), r.CapturePoints[0], data)
	timestamps := func(sink *recordingDelivery) []pcommon.Timestamp {
		var result []pcommon.Timestamp
		for _, log := range sink.log[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().All() {
			result = append(result, log.Timestamp())
		}
		return result
	}
	keptA, keptB := timestamps(sinkA), timestamps(sinkB)
	if len(keptA) < 400 || len(keptA) > 600 {
		t.Fatalf("kept %d of 2000 at rate 0.25", len(keptA))
	}
	if fmt.Sprint(keptA) != fmt.Sprint(keptB) {
		t.Fatal("log sampling depends on the pod or redaction")
	}
}

// Invalid UTF-8 used to make truncation quadratic.
func TestTruncateIsBoundedOnInvalidUTF8(t *testing.T) {
	started := time.Now()
	for _, input := range []string{strings.Repeat("\xff", 1<<20), strings.Repeat("é", 1<<19), "ab\xe6\x97", "日本語"} {
		for _, maximum := range []int{0, 1, 2, 3, 4, 7, 8, 1 << 10} {
			got := truncate(input, maximum)
			if len(got) > maximum || !strings.HasPrefix(input, got) {
				t.Fatalf("truncate(%q..., %d) = %q", input[:min(len(input), 8)], maximum, got)
			}
			if utf8.ValidString(input) && !utf8.ValidString(got) {
				t.Fatalf("split a rune at %d", maximum)
			}
			if utf8.ValidString(input) && len(input) > maximum && len(got) < maximum-utf8.UTFMax+1 {
				t.Fatal("cut more than one rune")
			}
		}
	}
	if time.Since(started) > time.Second {
		t.Fatal("truncation is not linear")
	}
}

type blockingDelivery struct {
	recordingDelivery
	release chan struct{}
}

func (d *blockingDelivery) ensure([]Signal) error { <-d.release; return nil }

// Starting the exporter can be slow; the kill switch must not wait for it.
func TestKillSwitchDoesNotWaitForExporterStart(t *testing.T) {
	m, _ := testManager(t)
	slow := &blockingDelivery{release: make(chan struct{})}
	m.delivery = slow
	result := make(chan error, 1)
	go func() {
		_, err := m.StartSession(context.Background(), traceRequest("slow-start"), Access{})
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	disabled := make(chan struct{})
	go func() { m.SetEnabled(false); _ = m.Statuses(); close(disabled) }()
	select {
	case <-disabled:
	case <-time.After(time.Second):
		t.Fatal("kill switch blocked behind exporter start")
	}
	close(slow.release)
	if err := <-result; err == nil || m.active.Load() != nil {
		t.Fatal("session activated after the kill switch")
	}
}
