package platform

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
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

	attrRunID       = "linea.run.id"
	attrRunMode     = "linea.run.mode"
	attrRunAttempt  = "linea.run.attempt"
	attrRuntimeID   = "linea.runtime.id"
	attrRunState    = "linea.run.state"
	attrErrorCode   = "linea.error.code"
	attrRunEvidence = "linea.run.evidence"

	// runReleased is the state of an attempt this instance handed back to
	// the queue, for another instance or a later attempt.
	runReleased = "released"
)

// newServerTraces sets the tracer from provider, a no-op one when nil.
func (s *Service) newServerTraces(provider trace.TracerProvider) {
	if provider == nil {
		provider = noop.NewTracerProvider()
	}
	s.tracer = provider.Tracer(meterName)
}

// startRunSpan starts the span of the attempt job holds. A run submitted on
// this instance links to the submitting request's span.
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
	if job.submitted.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: job.submitted}))
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
