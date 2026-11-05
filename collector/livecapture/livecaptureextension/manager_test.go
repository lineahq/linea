package livecaptureextension

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

type testHost map[component.ID]component.Component

func (h testHost) GetExtensions() map[component.ID]component.Component { return h }

type recordingDelivery struct {
	mu        sync.Mutex
	trace     []ptrace.Traces
	log       []plog.Logs
	metric    []pmetric.Metrics
	err       error
	ensureErr error
	panics    bool
}

func (d *recordingDelivery) ensure([]Signal) error { return d.ensureErr }

func (d *recordingDelivery) traces(_ context.Context, data ptrace.Traces) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.panics {
		panic("private payload must not enter status")
	}
	if d.err != nil {
		return d.err
	}
	d.trace = append(d.trace, data)
	return nil
}
func (d *recordingDelivery) logs(_ context.Context, data plog.Logs) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.log = append(d.log, data)
	return nil
}
func (d *recordingDelivery) metrics(_ context.Context, data pmetric.Metrics) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.metric = append(d.metric, data)
	return nil
}
func (d *recordingDelivery) shutdown(context.Context) error { return nil }

func testManager(t testing.TB) (*Manager, *recordingDelivery) {
	t.Helper()
	cfg := defaultConfig().(*Config)
	cfg.Enabled = true
	cfg.Environment = "test"
	cfg.Pod = "pod-a"
	cfg.Cluster = "cluster-a"
	cfg.LocalControl.Enabled = true
	cfg.LocalControl.Endpoint = "127.0.0.1:0"
	cfg.Delivery.ClientConfig.Endpoint = "127.0.0.1:9"
	cfg.Delivery.ClientConfig.TLS.Insecure = true
	m, err := newManager(extension.Settings{}, *cfg)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingDelivery{}
	m.delivery = sink
	for _, signal := range []Signal{Traces, Logs, Metrics} {
		if err := m.Register(CapturePoint{Point: Point{Pipeline: string(signal), Point: "ingress"}, Signal: signal, Sensitivity: "redacted"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return m, sink
}

func traceRequest(id string) Request {
	return Request{SessionID: id, Principal: "test-engineer", Signals: []Signal{Traces}, CapturePoints: []Point{{Pipeline: "traces", Point: "ingress"}}}
}
func start(t testing.TB, m *Manager, r Request) *session {
	t.Helper()
	if _, err := m.StartSession(context.Background(), r, Access{}); err != nil {
		t.Fatal(err)
	}
	return m.active.Load()
}
func wait(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func testTraces() ptrace.Traces {
	data := ptrace.NewTraces()
	rs := data.ResourceSpans().AppendEmpty()
	rs.SetSchemaUrl("resource-schema")
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	rs.Resource().Attributes().PutStr("db.query.text", "secret SQL")
	ss := rs.ScopeSpans().AppendEmpty()
	ss.SetSchemaUrl("scope-schema")
	ss.Scope().SetName("test-instrumentation")
	ss.Scope().SetVersion("1")
	ss.Scope().Attributes().PutStr("db.query.text", "scope secret")
	for _, code := range []ptrace.StatusCode{ptrace.StatusCodeError, ptrace.StatusCodeOk} {
		span := ss.Spans().AppendEmpty()
		span.SetName("POST /checkout")
		span.SetTraceID(pcommon.TraceID{1, 2, 3})
		span.SetSpanID(pcommon.SpanID{1, 2})
		span.Status().SetCode(code)
		span.Attributes().PutStr("http.request.header.authorization", "Bearer private")
		span.Attributes().PutInt("number", 12)
		span.Events().AppendEmpty().Attributes().PutStr("db.query.text", "event SQL")
		span.Links().AppendEmpty().Attributes().PutStr("db.query.text", "link SQL")
	}
	return data
}

func TestTracesFilterCopyAndRedaction(t *testing.T) {
	m, sink := testManager(t)
	r := traceRequest("trace-copy")
	r.Filter.Resource = []string{`attributes["service.name"] == "checkout"`}
	r.Filter.Traces.Span = []string{`status.code == STATUS_CODE_ERROR`, `IsMatch(name, "^POST")`}
	s := start(t, m, r)
	data := testTraces()
	before, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(data)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], data)
	after, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(data)
	if !bytes.Equal(before, after) {
		t.Fatal("capture mutated original")
	}
	if len(sink.trace) != 1 || sink.trace[0].SpanCount() != 1 {
		t.Fatalf("selected=%d", len(sink.trace))
	}
	copy := sink.trace[0]
	rs := copy.ResourceSpans().At(0)
	ss := rs.ScopeSpans().At(0)
	span := ss.Spans().At(0)
	if rs.SchemaUrl() != "resource-schema" || ss.SchemaUrl() != "scope-schema" || ss.Scope().Name() != "test-instrumentation" {
		t.Fatal("resource/scope context lost")
	}
	for _, attributes := range []pcommon.Map{rs.Resource().Attributes(), ss.Scope().Attributes(), span.Attributes(), span.Events().At(0).Attributes(), span.Links().At(0).Attributes()} {
		if _, found := attributes.Get("db.query.text"); found {
			t.Fatal("denied attribute survived")
		}
		if _, found := attributes.Get("http.request.header.authorization"); found {
			t.Fatal("authorization survived")
		}
	}
	if value, _ := rs.Resource().Attributes().Get("capture.session.id"); value.Str() != r.SessionID {
		t.Fatal("missing capture metadata")
	}
	data.ResourceSpans().At(0).Resource().Attributes().PutStr("service.name", "downstream")
	data.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).SetName("mutated downstream")
	if span.Name() != "POST /checkout" {
		t.Fatal("copy still aliases original")
	}
	if value, _ := rs.Resource().Attributes().Get("service.name"); value.Str() != "checkout" {
		t.Fatal("copied resource aliases original")
	}
	if s.stats().EnqueuedBytes != int64((&ptrace.ProtoMarshaler{}).TracesSize(copy)) {
		t.Fatal("byte accounting differs from encoded copy")
	}
}

func TestRejectUnsafeAndInvalidFilters(t *testing.T) {
	for _, expression := range []string{`set(attributes["x"], "y")`, `delete_key(attributes, "secret")`, `IsMatch(name, attributes["pattern"])`, `IsMatch(name, "[")`, `name ==`, stringsRepeat("x", 257)} {
		t.Run(expression, func(t *testing.T) {
			m, _ := testManager(t)
			r := traceRequest("bad-filter")
			r.Filter.Traces.Span = []string{expression}
			if _, err := m.StartSession(context.Background(), r, Access{}); err == nil {
				t.Fatal("unsafe filter accepted")
			}
			if m.active.Load() != nil {
				t.Fatal("invalid session activated")
			}
		})
	}
	m, _ := testManager(t)
	r := traceRequest("many")
	for range 17 {
		r.Filter.Traces.Span = append(r.Filter.Traces.Span, `name != ""`)
	}
	if _, err := m.StartSession(context.Background(), r, Access{}); err == nil {
		t.Fatal("17 conditions accepted")
	}
}
func stringsRepeat(s string, n int) string {
	var b bytes.Buffer
	for range n {
		b.WriteString(s)
	}
	return b.String()
}

func TestRuntimeFilterErrorIsNoMatch(t *testing.T) {
	m, sink := testManager(t)
	r := traceRequest("type-error")
	r.Filter.Traces.Span = []string{`attributes["number"]["nested"] == "x"`}
	s := start(t, m, r)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	if len(sink.trace) != 0 || s.stats().FilterErrors == 0 {
		t.Fatalf("filter errors not counted: %+v", s.stats())
	}
}

func TestLogsFilteringAndStructuredRedaction(t *testing.T) {
	m, sink := testManager(t)
	m.redactor.config.MaxLogBodyLength = 32
	r := Request{SessionID: "logs", Principal: "engineer", Signals: []Signal{Logs}, CapturePoints: []Point{{Pipeline: "logs", Point: "ingress"}}}
	r.Filter.Logs.Log = []string{`severity_number >= SEVERITY_NUMBER_WARN`}
	start(t, m, r)
	data := plog.NewLogs()
	rs := data.ResourceLogs().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeLogs().AppendEmpty()
	scope.Scope().SetName("logger")
	for _, level := range []plog.SeverityNumber{plog.SeverityNumberInfo, plog.SeverityNumberWarn} {
		log := scope.LogRecords().AppendEmpty()
		log.SetSeverityNumber(level)
		body := log.Body().SetEmptyMap()
		body.PutStr("db.query.text", "private")
		body.PutStr("message", stringsRepeat("é", 100))
		log.Attributes().PutStr("http.request.header.authorization", "private")
	}
	before, _ := (&plog.ProtoMarshaler{}).MarshalLogs(data)
	m.CaptureLogs(context.Background(), r.CapturePoints[0], data)
	after, _ := (&plog.ProtoMarshaler{}).MarshalLogs(data)
	if !bytes.Equal(before, after) {
		t.Fatal("log original changed")
	}
	if len(sink.log) != 1 {
		t.Fatalf("logs=%d", len(sink.log))
	}
	log := sink.log[0].ResourceLogs().At(0).ScopeLogs().At(0).LogRecords().At(0)
	if len(log.Body().AsString()) > 32 {
		t.Fatal("structured body cap ignored")
	}
}

func metricWithPoints(kind pmetric.MetricType) pmetric.Metrics {
	data := pmetric.NewMetrics()
	rs := data.ResourceMetrics().AppendEmpty()
	rs.SetSchemaUrl("r-schema")
	ss := rs.ScopeMetrics().AppendEmpty()
	ss.SetSchemaUrl("s-schema")
	ss.Scope().SetName("meter")
	m := ss.Metrics().AppendEmpty()
	m.SetName("requests")
	m.SetUnit("1")
	m.SetDescription("request count")
	m.Metadata().PutStr("aggregation", "test")
	switch kind {
	case pmetric.MetricTypeGauge:
		m.SetEmptyGauge()
	case pmetric.MetricTypeSum:
		s := m.SetEmptySum()
		s.SetIsMonotonic(true)
		s.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	case pmetric.MetricTypeHistogram:
		m.SetEmptyHistogram().SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	case pmetric.MetricTypeExponentialHistogram:
		m.SetEmptyExponentialHistogram().SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	case pmetric.MetricTypeSummary:
		m.SetEmptySummary()
	}
	for _, status := range []int64{200, 500} {
		var point any
		switch kind {
		case pmetric.MetricTypeGauge:
			point = m.Gauge().DataPoints().AppendEmpty()
		case pmetric.MetricTypeSum:
			point = m.Sum().DataPoints().AppendEmpty()
		case pmetric.MetricTypeHistogram:
			p := m.Histogram().DataPoints().AppendEmpty()
			p.SetCount(1)
			p.BucketCounts().FromRaw([]uint64{1})
			point = p
		case pmetric.MetricTypeExponentialHistogram:
			p := m.ExponentialHistogram().DataPoints().AppendEmpty()
			p.SetCount(1)
			p.Positive().BucketCounts().FromRaw([]uint64{1})
			point = p
		case pmetric.MetricTypeSummary:
			p := m.Summary().DataPoints().AppendEmpty()
			p.SetCount(1)
			point = p
		}
		datapointAttributes(point).PutInt("http.response.status_code", status)
		datapointAttributes(point).PutStr("db.query.text", "private")
	}
	return data
}

func TestMetricDatapointSelectionPreservesAllTypes(t *testing.T) {
	for _, kind := range []pmetric.MetricType{pmetric.MetricTypeGauge, pmetric.MetricTypeSum, pmetric.MetricTypeHistogram, pmetric.MetricTypeExponentialHistogram, pmetric.MetricTypeSummary} {
		t.Run(kind.String(), func(t *testing.T) {
			m, sink := testManager(t)
			r := Request{SessionID: "metric", Principal: "engineer", Signals: []Signal{Metrics}, CapturePoints: []Point{{Pipeline: "metrics", Point: "ingress"}}}
			r.Filter.Metrics.Metric = []string{`name == "requests"`}
			r.Filter.Metrics.Datapoint = []string{`attributes["http.response.status_code"] >= 500`}
			start(t, m, r)
			data := metricWithPoints(kind)
			before, _ := (&pmetric.ProtoMarshaler{}).MarshalMetrics(data)
			m.CaptureMetrics(context.Background(), r.CapturePoints[0], data)
			after, _ := (&pmetric.ProtoMarshaler{}).MarshalMetrics(data)
			if !bytes.Equal(before, after) {
				t.Fatal("metric original changed")
			}
			if len(sink.metric) != 1 || sink.metric[0].DataPointCount() != 1 {
				t.Fatal("matching datapoint selection failed")
			}
			copy := sink.metric[0].ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0)
			if copy.Type() != kind || copy.Name() != "requests" || copy.Unit() != "1" || copy.Description() != "request count" {
				t.Fatal("metric descriptor changed")
			}
			if value, _ := copy.Metadata().Get("aggregation"); value.Str() != "test" {
				t.Fatal("metric metadata missing")
			}
			if kind == pmetric.MetricTypeSum && (!copy.Sum().IsMonotonic() || copy.Sum().AggregationTemporality() != pmetric.AggregationTemporalityDelta) {
				t.Fatal("sum semantics changed")
			}
			forEachDatapoint(copy, func(point any) {
				attributes := datapointAttributes(point)
				if value, _ := attributes.Get("http.response.status_code"); value.Int() != 500 {
					t.Error("wrong datapoint")
				}
				if _, found := attributes.Get("db.query.text"); found {
					t.Error("metric secret survived")
				}
			})
			encoded, err := (&pmetric.ProtoMarshaler{}).MarshalMetrics(sink.metric[0])
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, err := (&pmetric.ProtoUnmarshaler{}).UnmarshalMetrics(encoded)
			if err != nil || roundtrip.DataPointCount() != 1 {
				t.Fatal("invalid OTLP metric copy")
			}
		})
	}
}

func TestSessionIdempotencyExpiryAndNoResurrection(t *testing.T) {
	m, _ := testManager(t)
	r := traceRequest("ttl")
	r.ExpiresAt = time.Now().Add(70 * time.Millisecond)
	s := start(t, m, r)
	first := s.request.ExpiresAt
	if _, err := m.StartSession(context.Background(), r, Access{}); err != nil {
		t.Fatal(err)
	}
	if m.active.Load() != s || !s.request.ExpiresAt.Equal(first) {
		t.Fatal("repeated full state extended session")
	}
	wait(t, func() bool { return m.active.Load() == nil })
	<-s.done
	if statuses := m.Statuses(); len(statuses) != 1 || statuses[0].State != "expired" {
		t.Fatalf("statuses=%+v", statuses)
	}
	r.ExpiresAt = time.Now().Add(time.Minute)
	if _, err := m.StartSession(context.Background(), r, Access{}); err == nil {
		t.Fatal("terminal session resurrected")
	}
}

func TestLimitsQueueFailuresPanicAndKillSwitch(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		m, sink := testManager(t)
		r := traceRequest("rate")
		r.Limits.RecordsPerSecond = 1
		s := start(t, m, r)
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
		if len(sink.trace) != 1 || s.stats().Dropped["rate_records"] != 1 {
			t.Fatal(s.stats())
		}
	})
	t.Run("record size", func(t *testing.T) {
		m, sink := testManager(t)
		r := traceRequest("size")
		r.Limits.RecordBytes = 1
		s := start(t, m, r)
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
		if len(sink.trace) != 0 || s.stats().Dropped["record_size"] != 2 {
			t.Fatal(s.stats())
		}
	})
	t.Run("total budget", func(t *testing.T) {
		m, sink := testManager(t)
		r := traceRequest("total")
		r.Limits.TotalRecords = 1
		s := start(t, m, r)
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
		<-s.done
		if len(sink.trace) != 1 || m.Statuses()[0].State != "limit_reached" {
			t.Fatal(m.Statuses())
		}
	})
	t.Run("queue full", func(t *testing.T) {
		m, sink := testManager(t)
		sink.err = exporterQueueFull
		r := traceRequest("full")
		s := start(t, m, r)
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
		if s.stats().Dropped["queue_full"] != 2 || s.stats().EnqueuedBytes != 0 {
			t.Fatal(s.stats())
		}
	})
	t.Run("panic", func(t *testing.T) {
		m, sink := testManager(t)
		sink.panics = true
		r := traceRequest("panic")
		s := start(t, m, r)
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
		<-s.done
		if m.Statuses()[0].State != "failed" || m.Statuses()[0].Reason != "capture_panic" {
			t.Fatal(m.Statuses())
		}
	})
	t.Run("kill switch", func(t *testing.T) {
		m, _ := testManager(t)
		s := start(t, m, traceRequest("disabled"))
		m.SetEnabled(false)
		<-s.done
		if m.Statuses()[0].State != "disabled" {
			t.Fatal(m.Statuses())
		}
		if _, err := m.StartSession(context.Background(), traceRequest("new"), Access{}); err == nil {
			t.Fatal("disabled session accepted")
		}
	})
	t.Run("clamp", func(t *testing.T) {
		m, _ := testManager(t)
		r := traceRequest("clamp")
		r.Limits.TotalRecords = 1 << 60
		r.ExpiresAt = time.Now().Add(time.Hour)
		s := start(t, m, r)
		if s.request.Limits.TotalRecords != m.config.Limits.MaxTotalRecords || time.Until(s.request.ExpiresAt) > m.config.Limits.MaxSessionDuration {
			t.Fatal("hard caps exceeded")
		}
	})
}

func TestRawAuthorizationAndUnknownPoints(t *testing.T) {
	m, _ := testManager(t)
	p := CapturePoint{Point: Point{Pipeline: "traces", Point: "raw"}, Signal: Traces, Sensitivity: "raw"}
	if err := m.Register(p); err != nil {
		t.Fatal(err)
	}
	r := traceRequest("raw")
	r.CapturePoints = []Point{p.Point}
	if _, err := m.StartSession(context.Background(), r, Access{}); err == nil {
		t.Fatal("raw without permission accepted")
	}
	if _, err := m.StartSession(context.Background(), r, Access{AllowRaw: true}); err != nil {
		t.Fatal(err)
	}
	r.SessionID = "other"
	r.CapturePoints[0].Point = "missing"
	if _, err := m.StartSession(context.Background(), r, Access{}); err == nil {
		t.Fatal("unknown point accepted")
	}
}

func keptTraceIDs(data []ptrace.Traces) map[pcommon.TraceID]bool {
	kept := make(map[pcommon.TraceID]bool)
	for _, traces := range data {
		for _, rs := range traces.ResourceSpans().All() {
			for _, ss := range rs.ScopeSpans().All() {
				for _, span := range ss.Spans().All() {
					kept[span.TraceID()] = true
				}
			}
		}
	}
	return kept
}

// Traces always sample by trace ID, even in random mode, so every pod keeps
// whole traces.
func TestSamplingDeterminismAcrossPods(t *testing.T) {
	a, sinkA := testManager(t)
	b, sinkB := testManager(t)
	b.config.Pod = "pod-b"
	r := traceRequest("sampling")
	r.Sampling = &Sampling{Rate: .5, Mode: "random"}
	start(t, a, r)
	start(t, b, r)
	data := ptrace.NewTraces()
	spans := data.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans()
	for i := range 1000 {
		span := spans.AppendEmpty()
		span.SetTraceID(pcommon.TraceID([16]byte([]byte(fmt.Sprintf("trace-%010d", i)))))
	}
	a.CaptureTraces(context.Background(), r.CapturePoints[0], data)
	b.CaptureTraces(context.Background(), r.CapturePoints[0], data)
	keptA, keptB := keptTraceIDs(sinkA.trace), keptTraceIDs(sinkB.trace)
	if len(keptA) < 300 || len(keptA) > 700 {
		t.Fatalf("unexpected sampled count %d", len(keptA))
	}
	if !maps.Equal(keptA, keptB) {
		t.Fatal("trace sampling differs between pods")
	}
}

func TestConcurrentCaptureAndReconcile(t *testing.T) {
	m, _ := testManager(t)
	r := traceRequest("concurrent")
	start(t, m, r)
	var workers sync.WaitGroup
	for range 6 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 50 {
				m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
				_ = m.Statuses()
			}
		}()
	}
	for i := range 3 {
		updated := r
		updated.Filter.Traces.Span = []string{fmt.Sprintf(`attributes["number"] >= %d`, i)}
		if _, err := m.StartSession(context.Background(), updated, Access{}); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
	if err := m.Reconcile(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if m.active.Load() != nil {
		t.Fatal("empty desired state did not stop")
	}
}

func TestInternalExporterStartFailureDoesNotActivate(t *testing.T) {
	m, _ := testManager(t)
	sink := m.delivery.(*recordingDelivery)
	sink.ensureErr = errors.New("invalid credentials")
	if _, err := m.StartSession(context.Background(), traceRequest("failed"), Access{}); err == nil {
		t.Fatal("exporter start error ignored")
	}
	if m.active.Load() != nil {
		t.Fatal("failed exporter session activated")
	}
}

func TestEndingSessionWaitsForQueuedCopies(t *testing.T) {
	m, _ := testManager(t)
	var queued atomic.Int64
	queued.Store(1)
	m.queues.mu.Lock()
	m.queues.callbacks = map[uint64]func(context.Context) int64{1: func(context.Context) int64 { return queued.Load() }}
	m.queues.mu.Unlock()
	s := start(t, m, traceRequest("drain"))
	if err := m.StopSession(context.Background(), "drain"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if statuses := m.Statuses(); len(statuses) != 1 || statuses[0].State != "stopping" {
		t.Fatalf("ended before its copies left the queue: %+v", statuses)
	}
	// The draining ID stays reserved; another session may start.
	if _, err := m.StartSession(context.Background(), traceRequest("drain"), Access{}); err == nil {
		t.Fatal("draining session ID was reused")
	}
	start(t, m, traceRequest("next"))
	queued.Store(0)
	<-s.done
	if statuses := m.Statuses(); len(statuses) != 2 || statuses[0].State != "completed" || statuses[1].State != "active" {
		t.Fatalf("%+v", statuses)
	}
}

func TestRawPointCopiesAreNotRedacted(t *testing.T) {
	m, sink := testManager(t)
	m.redactor.config.MaxLogBodyLength = 8
	m.redactor.config.MaxStringLength = 8
	for _, signal := range []Signal{Traces, Logs} {
		if err := m.Register(CapturePoint{Point: Point{Pipeline: string(signal), Point: "raw"}, Signal: signal, Sensitivity: "raw"}); err != nil {
			t.Fatal(err)
		}
	}
	r := Request{SessionID: "raw", Principal: "engineer", Signals: []Signal{Traces, Logs},
		CapturePoints: []Point{{Pipeline: "traces", Point: "raw"}, {Pipeline: "logs", Point: "raw"}}}
	if _, err := m.StartSession(context.Background(), r, Access{AllowRaw: true}); err != nil {
		t.Fatal(err)
	}
	logs := plog.NewLogs()
	rl := logs.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("db.query.text", "resource SQL")
	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName(stringsRepeat("s", 64))
	log := sl.LogRecords().AppendEmpty()
	log.Body().SetStr(stringsRepeat("é", 100))
	log.SetSeverityText(stringsRepeat("w", 64))
	log.Attributes().PutStr("http.request.header.authorization", "Bearer private")
	traces := testTraces()
	m.CaptureLogs(context.Background(), r.CapturePoints[1], logs)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], traces)
	if len(sink.log) != 1 || len(sink.trace) != 1 {
		t.Fatalf("logs=%d traces=%d", len(sink.log), len(sink.trace))
	}
	// The copy equals the original except for the routing tags on the resource.
	untag := func(resource pcommon.Resource) {
		resource.Attributes().RemoveIf(func(key string, _ pcommon.Value) bool { return strings.HasPrefix(key, "capture.") })
	}
	untag(sink.log[0].ResourceLogs().At(0).Resource())
	got, _ := (&plog.ProtoMarshaler{}).MarshalLogs(sink.log[0])
	want, _ := (&plog.ProtoMarshaler{}).MarshalLogs(logs)
	if !bytes.Equal(got, want) {
		t.Fatal("raw log copy was modified")
	}
	untag(sink.trace[0].ResourceSpans().At(0).Resource())
	gotTraces, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(sink.trace[0])
	wantTraces, _ := (&ptrace.ProtoMarshaler{}).MarshalTraces(traces)
	if !bytes.Equal(gotTraces, wantTraces) {
		t.Fatal("raw trace copy was modified")
	}
}
