package platform

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// The spans of a build attempt on the instance its builder is connected
// to: linea.build from the claim to the outcome, linked to the request that
// asked for the build, with linea.build.execute (one linea.builder.<phase>
// child per phase the builder timed) and linea.build.publish around
// storing the binary and the outcome. Like the run spans they carry IDs, a
// bool, the state and a fixed failure reason: never the runtime's name, the
// builder's, a module, a URL, a digest, the log or an error message.

// Span and attribute names are a contract once published.
const (
	spanBuild        = "linea.build"
	spanBuildExecute = "linea.build.execute"
	spanBuildPublish = "linea.build.publish"
	// spanBuilderPhase prefixes the span of each phase a builder reports.
	spanBuilderPhase = "linea.builder."

	attrBuildID       = "linea.build.id"
	attrBuildAttempt  = "linea.build.attempt"
	attrBuildVerified = "linea.build.verified"
	attrBuildState    = "linea.build.state"
	attrBuildFailure  = "linea.build.failure"
	attrBuilderPhases = "linea.builder.phases"

	// buildReleased is the state of an attempt this instance left to
	// expire, so another attempt runs the build.
	buildReleased = "released"
)

// Why a build failed, as its span says: builds have no platform error
// codes, so this is a span vocabulary only.
const (
	// buildFailureBuilder: the builder reported an error.
	buildFailureBuilder = "builder"
	// buildFailureTimeout: the build exceeded its time.
	buildFailureTimeout = "timeout"
	// buildFailureRejected: the server refused the builder's result.
	buildFailureRejected = "rejected"
	// buildFailureInputs: preparing the build's inputs failed.
	buildFailureInputs = "inputs"
	// buildFailureStorage: writing the binary or the outcome failed; only a
	// publish span's status, since the attempt is then released.
	buildFailureStorage = "storage"
)

var buildPhaseSpans = phaseSpans{prefix: spanBuilderPhase, count: attrBuilderPhases, known: worker.KnownBuildPhase}

// buildSpanOutcome is how an attempt ended, for its span.
type buildSpanOutcome struct {
	state, failure string
}

// startBuildSpan starts the root span of build attempt r, linked to the
// span of the request that asked for the build.
func (s *Service) startBuildSpan(r artifact.BuildRecord, spec serverBuildSpec) trace.Span {
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String(attrBuildID, r.ID),
			attribute.Int(attrBuildAttempt, r.Attempt),
			attribute.Bool(attrBuildVerified, spec.Verified),
		),
	}
	if submitted := spec.Submitted.spanContext(); submitted.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: submitted}))
	}
	// A root span: the build outlives the request that asked for it.
	_, span := s.tracer.Start(context.Background(), spanBuild, opts...)
	return span
}

// endBuildSpan ends a build span with its attempt's outcome; a failed
// build has error status.
func endBuildSpan(span trace.Span, out buildSpanOutcome) {
	span.SetAttributes(attribute.String(attrBuildState, out.state))
	if out.state == string(artifact.BuildFailed) {
		span.SetAttributes(attribute.String(attrBuildFailure, out.failure))
		span.SetStatus(codes.Error, out.failure)
	}
	span.End()
}

// startBuildExecuteSpan starts the span of a build's exchange with its
// builder under the build span in ctx, and returns its start.
func (s *Service) startBuildExecuteSpan(ctx context.Context) (trace.Span, time.Time) {
	start := time.Now()
	_, span := s.tracer.Start(ctx, spanBuildExecute, trace.WithSpanKind(trace.SpanKindInternal), trace.WithTimestamp(start))
	return span, start
}

// startBuildPublishSpan starts the span of storing a build's binary and
// outcome under the build span in ctx.
func (s *Service) startBuildPublishSpan(ctx context.Context) trace.Span {
	_, span := s.tracer.Start(ctx, spanBuildPublish, trace.WithSpanKind(trace.SpanKindInternal))
	return span
}
