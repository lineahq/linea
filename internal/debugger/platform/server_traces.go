package platform

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/lineahq/linea/internal/debugger/engine"
)

// The server's own traces: one linea.run span per run attempt this instance
// executes, from its claim to its published outcome, with a
// linea.run.publish child around the repository write. Like the metrics
// (server_metrics.go) they describe the server, never the Collector, the
// configuration or the telemetry of a run; spans carry IDs, the mode, the
// state and an error code, never an error message. The composing
// application chooses the SDK through Options.TracerProvider.

// Span and attribute names are a contract once published.
const (
	spanRun        = "linea.run"
	spanRunPublish = "linea.run.publish"
	spanRunExecute = "linea.run.execute"
	// spanPhase prefixes the span of each phase an execution reports.
	spanPhase = "linea.worker."

	attrRunID       = "linea.run.id"
	attrRunMode     = "linea.run.mode"
	attrRunAttempt  = "linea.run.attempt"
	attrRuntimeID   = "linea.runtime.id"
	attrRunState    = "linea.run.state"
	attrErrorCode   = "linea.error.code"
	attrRunEvidence = "linea.run.evidence"
	attrPhases      = "linea.worker.phases"

	// maxPhases bounds the phases one execution may report.
	maxPhases = 16

	// runReleased is the state of an attempt this instance handed back to
	// the queue, for another instance or a later attempt.
	runReleased = "released"
)

// storedSpan is a span context as run metadata stores it: two IDs and the
// sampled flag, nothing from the request it names.
type storedSpan struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
	Sampled bool   `json:"sampled,omitempty"`
}

// storeSpan returns sc as stored, or nil when it is not valid.
func storeSpan(sc trace.SpanContext) *storedSpan {
	if !sc.IsValid() {
		return nil
	}
	return &storedSpan{TraceID: sc.TraceID().String(), SpanID: sc.SpanID().String(), Sampled: sc.IsSampled()}
}

// spanContext returns the stored span as a remote span context, invalid
// when it is absent or malformed: it only drives a link.
func (s *storedSpan) spanContext() trace.SpanContext {
	if s == nil {
		return trace.SpanContext{}
	}
	tid, err1 := trace.TraceIDFromHex(s.TraceID)
	sid, err2 := trace.SpanIDFromHex(s.SpanID)
	if err1 != nil || err2 != nil {
		return trace.SpanContext{}
	}
	var flags trace.TraceFlags
	if s.Sampled {
		flags = trace.FlagsSampled
	}
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: flags, Remote: true})
}

// newServerTraces sets the tracer from provider, a no-op one when nil.
func (s *Service) newServerTraces(provider trace.TracerProvider) {
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	s.tracer = provider.Tracer(meterName)
}

// startRunSpan starts the span of the attempt job holds, linked to the
// span of the request that submitted the run, on whichever instance.
func (s *Service) startRunSpan(job *pendingRun) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String(attrRunID, job.handle.id),
			attribute.String(attrRunMode, string(job.handle.mode)),
			attribute.Int(attrRunAttempt, job.assignment.Attempt),
		),
	}
	if job.assignment.Runtime != "" {
		opts = append(opts, trace.WithAttributes(attribute.String(attrRuntimeID, job.assignment.Runtime)))
	}
	if submitted := job.handle.submitted; submitted.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: submitted}))
	}
	// A root span: the run outlives the request that submitted it.
	return s.tracer.Start(context.Background(), spanRun, opts...)
}

// endRunSpan ends span with the state the attempt left the run in.
func endRunSpan(span trace.Span, job *pendingRun) {
	r := job.handle
	r.mu.Lock()
	state, err := runReleased, error(nil)
	if r.terminalLocked() && r.attempt == job.assignment.Attempt {
		state, err = string(r.state), r.err
	}
	r.mu.Unlock()
	span.SetAttributes(attribute.String(attrRunState, state))
	if state == string(StateError) {
		code := runErrorCode(err)
		span.SetAttributes(attribute.String(attrErrorCode, string(code)))
		span.SetStatus(codes.Error, string(code))
	}
	span.End()
}

// runErrorCode classifies the error a run ended with: storage_error when its
// outcome could not be written, a platform error's own code, and run_failed
// for anything the run itself returned.
func runErrorCode(err error) Code {
	var e *Error
	switch {
	case errors.Is(err, ErrPersistence):
		return CodeStorageError
	case errors.As(err, &e):
		return e.Code
	}
	return CodeRunFailed
}

// startPublishSpan starts the publish span of a run's outcome under ctx.
func (s *Service) startPublishSpan(ctx context.Context) trace.Span {
	if ctx == nil {
		ctx = context.Background()
	}
	_, span := s.tracer.Start(ctx, spanRunPublish, trace.WithSpanKind(trace.SpanKindInternal))
	return span
}

// endPublishSpan ends a publish span: evidence says whether the evidence was
// written, failed whether no write succeeded.
func endPublishSpan(span trace.Span, evidence, failed bool) {
	span.SetAttributes(attribute.Bool(attrRunEvidence, evidence))
	if failed {
		span.SetStatus(codes.Error, string(CodeStorageError))
	}
	span.End()
}

// startExecuteSpan starts the span of a run's execution under the run span
// ctx and, when it records, the recorder of the phases the execution
// reports, counting from the span's start.
func (s *Service) startExecuteSpan(ctx context.Context) (trace.Span, *engine.Phases) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	_, span := s.tracer.Start(ctx, spanRunExecute, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(start))
	if !span.IsRecording() {
		return span, nil
	}
	return span, engine.NewPhases(start)
}

// endExecuteSpan ends span with a child per phase the execution reported.
func (s *Service) endExecuteSpan(span trace.Span, phases *engine.Phases, err error) {
	end := time.Now()
	status := ""
	if err != nil {
		status = string(runErrorCode(err))
	}
	s.endPhasedSpan(span, runPhaseSpans, end.Add(-phases.Since(end)), end, phases.List(), err != nil, status)
}

// phaseSpans names the children of an execute span and the attribute
// counting them, and the phase names kept.
type phaseSpans struct {
	prefix, count string
	known         func(string) bool
}

var runPhaseSpans = phaseSpans{prefix: spanPhase, count: attrPhases, known: engine.KnownPhase}

// endPhasedSpan ends span, which started at start, at end with a child per
// phase, placed by its offset from start. A worker's or builder's phases
// count from its receipt of the request, so each is clamped into the span;
// unknown names, phases beyond maxPhases and negative times are dropped.
// failed sets the span's error status, described by status.
func (s *Service) endPhasedSpan(span trace.Span, ps phaseSpans, start, end time.Time, phases []engine.Phase, failed bool, status string) {
	ctx := trace.ContextWithSpan(context.Background(), span)
	kept := 0
	for _, p := range phases {
		if kept == maxPhases || !ps.known(p.Name) || p.Offset < 0 || p.Duration < 0 {
			continue
		}
		kept++
		from := earlier(start.Add(p.Offset), end)
		to := earlier(from.Add(p.Duration), end)
		_, child := s.tracer.Start(ctx, ps.prefix+p.Name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(from))
		if p.Failed {
			child.SetStatus(codes.Error, "")
		}
		child.End(trace.WithTimestamp(to))
	}
	span.SetAttributes(attribute.Int(ps.count, kept))
	if failed {
		span.SetStatus(codes.Error, status)
	}
	span.End(trace.WithTimestamp(end))
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
