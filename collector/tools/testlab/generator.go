package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
)

type generatorState struct {
	Enabled   bool   `json:"enabled"`
	Rate      int    `json:"rate"`
	Traces    uint64 `json:"traces"`
	Logs      uint64 `json:"logs"`
	Metrics   uint64 `json:"metrics"`
	Errors    uint64 `json:"errors"`
	LastError string `json:"last_error,omitempty"`
}
type generator struct {
	mu    sync.Mutex
	state generatorState
	conn  *grpc.ClientConn
}

func (g *generator) snapshot() generatorState { g.mu.Lock(); defer g.mu.Unlock(); return g.state }
func (g *generator) configure(enabled bool, rate int) {
	g.mu.Lock()
	g.state.Enabled = enabled
	g.state.Rate = rate
	g.mu.Unlock()
}
func (g *generator) run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var sequence uint64
	var next time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			state := g.snapshot()
			if !state.Enabled {
				next = now
				continue
			}
			if now.Before(next) {
				continue
			}
			next = now.Add(time.Second / time.Duration(state.Rate))
			sequence++
			g.send(ctx, sequence)
		}
	}
}
func synthetic(sequence uint64) (ptrace.Traces, plog.Logs, pmetric.Metrics) {
	services := []string{"checkout", "inventory", "billing"}
	service := services[(sequence-1)%3]
	now := time.Now()
	bad := sequence%5 == 0
	code := int64(200)
	if bad {
		code = 500
	}
	traceID := pcommon.TraceID{}
	spanID := pcommon.SpanID{}
	binary.BigEndian.PutUint64(traceID[:8], uint64(now.UnixNano()))
	binary.BigEndian.PutUint64(traceID[8:], sequence)
	binary.BigEndian.PutUint64(spanID[:], sequence)
	resource := func(r pcommon.Resource) {
		r.Attributes().PutStr("service.name", service)
		r.Attributes().PutStr("deployment.environment.name", "testlab")
		r.Attributes().PutStr("http.request.header.authorization", "Bearer SYNTHETIC-ONLY-SECRET")
		r.Attributes().PutStr("db.query.text", "SELECT synthetic_private_value FROM test_fixture")
	}
	attributes := func(a pcommon.Map) {
		a.PutInt("test.sequence", int64(sequence))
		a.PutInt("http.response.status_code", code)
		a.PutStr("http.request.method", "POST")
		a.PutStr("url.path", "/api/"+service)
	}
	traces := ptrace.NewTraces()
	tr := traces.ResourceSpans().AppendEmpty()
	resource(tr.Resource())
	ts := tr.ScopeSpans().AppendEmpty()
	ts.Scope().SetName("livecapture.testlab")
	ts.Scope().SetVersion("1")
	span := ts.Spans().AppendEmpty()
	span.SetTraceID(traceID)
	span.SetSpanID(spanID)
	span.SetName("POST /api/" + service)
	span.SetKind(ptrace.SpanKindServer)
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(-25 * time.Millisecond)))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now))
	attributes(span.Attributes())
	span.Status().SetCode(ptrace.StatusCodeOk)
	if bad {
		span.Status().SetCode(ptrace.StatusCodeError)
		span.Status().SetMessage("synthetic failure")
	}
	logs := plog.NewLogs()
	lr := logs.ResourceLogs().AppendEmpty()
	resource(lr.Resource())
	ls := lr.ScopeLogs().AppendEmpty()
	ls.Scope().SetName("livecapture.testlab")
	log := ls.LogRecords().AppendEmpty()
	log.SetTimestamp(pcommon.NewTimestampFromTime(now))
	log.SetObservedTimestamp(log.Timestamp())
	log.SetTraceID(traceID)
	log.SetSpanID(spanID)
	log.SetSeverityNumber(plog.SeverityNumberInfo)
	log.SetSeverityText("INFO")
	if bad {
		log.SetSeverityNumber(plog.SeverityNumberError)
		log.SetSeverityText("ERROR")
	}
	log.Body().SetStr(fmt.Sprintf("Synthetic %s request #%d returned %d", service, sequence, code))
	attributes(log.Attributes())
	metrics := pmetric.NewMetrics()
	mr := metrics.ResourceMetrics().AppendEmpty()
	resource(mr.Resource())
	ms := mr.ScopeMetrics().AppendEmpty()
	ms.Scope().SetName("livecapture.testlab")
	metric := ms.Metrics().AppendEmpty()
	metric.SetName("test.request.duration")
	metric.SetDescription("Synthetic request duration")
	metric.SetUnit("ms")
	point := metric.SetEmptyGauge().DataPoints().AppendEmpty()
	point.SetTimestamp(pcommon.NewTimestampFromTime(now))
	point.SetDoubleValue(float64(20 + sequence%80))
	attributes(point.Attributes())
	return traces, logs, metrics
}
func (g *generator) send(ctx context.Context, sequence uint64) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	traces, logs, metrics := synthetic(sequence)
	tr, te := ptraceotlp.NewGRPCClient(g.conn).Export(ctx, ptraceotlp.NewExportRequestFromTraces(traces))
	lr, le := plogotlp.NewGRPCClient(g.conn).Export(ctx, plogotlp.NewExportRequestFromLogs(logs))
	mr, me := pmetricotlp.NewGRPCClient(g.conn).Export(ctx, pmetricotlp.NewExportRequestFromMetrics(metrics))
	if te == nil && tr.PartialSuccess().RejectedSpans() > 0 {
		te = fmt.Errorf("the Collector rejected synthetic spans")
	}
	if le == nil && lr.PartialSuccess().RejectedLogRecords() > 0 {
		le = fmt.Errorf("the Collector rejected synthetic logs")
	}
	if me == nil && mr.PartialSuccess().RejectedDataPoints() > 0 {
		me = fmt.Errorf("the Collector rejected synthetic metrics")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if te == nil {
		g.state.Traces++
	}
	if le == nil {
		g.state.Logs++
	}
	if me == nil {
		g.state.Metrics++
	}
	if te == nil && le == nil && me == nil {
		g.state.LastError = ""
	}
	for _, err := range []error{te, le, me} {
		if err != nil {
			g.state.Errors++
			g.state.LastError = err.Error()
		}
	}
}
