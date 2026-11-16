package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/worker"
)

// TestBuildSpansRecordTheAttemptItsPhasesAndItsPublish: a build attempt is
// one root linea.build span linked to the request that asked for it, with
// its ID, attempt, verification and outcome, a linea.build.execute child
// holding the builder's known phases and a linea.build.publish child when
// it stores an outcome. A failed build says why with a fixed reason; no
// attribute holds the runtime's name, the builder's or an error message.
func TestBuildSpansRecordTheAttemptItsPhasesAndItsPublish(t *testing.T) {
	const name = "secretname"
	identity := "sha256:" + strings.Repeat("e", 64)
	phases := []engine.Phase{
		{Name: worker.BuildPhaseInputsDownload, Duration: time.Millisecond},
		{Name: "unknown.phase", Offset: time.Millisecond, Duration: time.Millisecond},
		{Name: worker.BuildPhaseRuntimeBuild, Offset: time.Millisecond, Duration: time.Hour},
	}
	for _, tc := range []struct {
		name    string
		build   worker.BuildFunc
		cancel  bool
		state   string
		failure string
		publish bool
		phases  int
	}{
		{"done", func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
			r := fakeBuild([]byte("binary"), identity)(ctx, job, log)
			r.Phases = phases
			return r
		}, false, "done", "", true, 2},
		{"failed", func(context.Context, worker.BuildJob, func(string)) worker.BuildResult {
			failed := append([]engine.Phase(nil), phases[:1]...)
			failed[0].Failed = true
			return worker.BuildResult{Error: "secret module failure", Phases: failed}
		}, false, "failed", "builder", true, 1},
		{"rejected", func(ctx context.Context, job worker.BuildJob, log func(string)) worker.BuildResult {
			r := fakeBuild([]byte("binary"), identity)(ctx, job, log)
			r.Hello.ProductVersion = "0.0.0-other"
			return r
		}, false, "failed", "rejected", true, 0},
		{"cancelled", func(ctx context.Context, _ worker.BuildJob, _ func(string)) worker.BuildResult {
			<-ctx.Done()
			return worker.BuildResult{Error: "stopped"}
		}, true, "cancelled", "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			s, _, url := builderPlatformWith(t, Options{TracerProvider: provider})
			serveTestBuilds(t, url, tc.build)
			submitter := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{3}, SpanID: trace.SpanID{4}, TraceFlags: trace.FlagsSampled, Remote: true})
			build, err := s.BuildRuntime(trace.ContextWithSpanContext(context.Background(), submitter), buildAlice,
				serverBuildRequest(name, procArchive(t, "")))
			if err != nil {
				t.Fatal(err)
			}
			if tc.cancel {
				waitFor(t, "the build to run", func() bool {
					b, err := s.GetRuntimeBuild(context.Background(), buildAlice, build.ID, 0)
					return err == nil && b.State == BuildRunning
				})
				if _, err := s.CancelRuntimeBuild(context.Background(), buildAlice, build.ID); err != nil {
					t.Fatal(err)
				}
			}
			waitServerBuild(t, s, buildAlice, build.ID)
			var root sdktrace.ReadOnlySpan
			waitFor(t, "the build span", func() bool {
				for _, sp := range recorder.Ended() {
					if sp.Name() == spanBuild {
						root = sp
						return true
					}
				}
				return false
			})

			attrs := map[attribute.Key]attribute.Value{}
			for _, a := range root.Attributes() {
				attrs[a.Key] = a.Value
			}
			if attrs[attrBuildID].AsString() != build.ID || attrs[attrBuildAttempt].AsInt64() != 1 ||
				attrs[attrBuildState].AsString() != tc.state || attrs[attrBuildFailure].AsString() != tc.failure {
				t.Fatalf("build span attributes %v", root.Attributes())
			}
			if _, ok := attrs[attrBuildVerified]; !ok {
				t.Fatalf("build span has no %s: %v", attrBuildVerified, root.Attributes())
			}
			if root.Parent().IsValid() || len(root.Links()) != 1 || root.Links()[0].SpanContext.SpanID() != submitter.SpanID() {
				t.Fatalf("build span parent %v, links %v", root.Parent(), root.Links())
			}
			if wantErr := tc.state == "failed"; (root.Status().Code == codes.Error) != wantErr || (wantErr && root.Status().Description != tc.failure) {
				t.Fatalf("build span status %+v", root.Status())
			}

			children := map[string]sdktrace.ReadOnlySpan{}
			phaseSpans := 0
			for _, sp := range recorder.Ended() {
				for _, a := range sp.Attributes() {
					if v := a.Value.String(); strings.Contains(v, name) || strings.Contains(v, "secret") || strings.Contains(v, "builder-a") {
						t.Fatalf("span %s attribute %s = %q", sp.Name(), a.Key, v)
					}
				}
				if sp.Parent().SpanID() == root.SpanContext().SpanID() {
					children[sp.Name()] = sp
				}
			}
			execute := children[spanBuildExecute]
			if execute == nil {
				t.Fatalf("no execute span under the build span: %v", children)
			}
			for _, sp := range recorder.Ended() {
				if sp.Parent().SpanID() != execute.SpanContext().SpanID() {
					continue
				}
				phaseSpans++
				if !strings.HasPrefix(sp.Name(), spanBuilderPhase) || sp.EndTime().After(execute.EndTime()) {
					t.Fatalf("phase span %s ends %v, execute ends %v", sp.Name(), sp.EndTime(), execute.EndTime())
				}
			}
			if phaseSpans != tc.phases {
				t.Fatalf("%d phase spans, want %d", phaseSpans, tc.phases)
			}
			for _, a := range execute.Attributes() {
				if a.Key == attrBuilderPhases && a.Value.AsInt64() != int64(tc.phases) {
					t.Fatalf("execute span %s = %d", a.Key, a.Value.AsInt64())
				}
			}
			if publish := children[spanBuildPublish]; (publish != nil) != tc.publish {
				t.Fatalf("publish span %v, want %v", publish != nil, tc.publish)
			}
		})
	}
}
