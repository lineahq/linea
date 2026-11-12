package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestRunSpansRecordTheAttemptAndItsPublish: each run attempt is one root
// linea.run span with its ID, mode, attempt, runtime and final state, linked
// to the span of the request that submitted it, with a linea.run.publish
// child; a failed run carries an error code and never its message.
func TestRunSpansRecordTheAttemptAndItsPublish(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	s := openPlatform(t, memory.NewData(), Options{Now: time.Now, TracerProvider: provider,
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			if req.FixtureJSON == `{"resourceSpans":[{}]}` {
				return nil, errors.New("collector exited: secret detail")
			}
			return report(req, "completed"), nil
		}})
	submitter := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled, Remote: true})
	ok, err := s.submit(trace.ContextWithSpanContext(s.ctx, submitter), testCaller, ModeQuick,
		engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	failed := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[{}]}`})
	if v := waitRun(t, ok); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, ok.Err())
	}
	if v := waitRun(t, failed); v.State != StateError {
		t.Fatalf("failing run = %s", v.State)
	}
	waitFor(t, "four ended spans", func() bool { return len(recorder.Ended()) == 4 })

	runs := map[string]sdktrace.ReadOnlySpan{}
	publishes := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, sp := range recorder.Ended() {
		switch sp.Name() {
		case spanRun:
			runs[attrOf(sp, attrRunID)] = sp
		case spanRunPublish:
			publishes[sp.Parent().SpanID()] = sp
		default:
			t.Fatalf("unexpected span %q", sp.Name())
		}
	}
	for _, tc := range []struct {
		run   *Handle
		state string
		code  string
		link  bool
	}{
		{run: ok, state: "done", link: true},
		{run: failed, state: "error", code: string(CodeRunFailed)},
	} {
		sp := runs[tc.run.id]
		if sp == nil {
			t.Fatalf("no %s span for run %s", spanRun, tc.run.id)
		}
		if sp.Parent().IsValid() {
			t.Errorf("%s: run span has a parent; it is a root", tc.run.id)
		}
		want := map[string]string{attrRunID: tc.run.id, attrRunMode: string(ModeQuick), attrRunAttempt: "1", attrRuntimeID: "", attrRunState: tc.state}
		if tc.code != "" {
			want[attrErrorCode] = tc.code
		}
		got := map[string]string{}
		for _, kv := range sp.Attributes() {
			got[string(kv.Key)] = kv.Value.String()
		}
		if _, ok := got[attrRuntimeID]; ok {
			want[attrRuntimeID] = got[attrRuntimeID]
		} else {
			delete(want, attrRuntimeID)
		}
		if len(got) != len(want) {
			t.Errorf("%s: attributes %v, want %v", tc.state, got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.state, k, got[k], v)
			}
		}
		if wantErr := tc.code != ""; (sp.Status().Code == codes.Error) != wantErr || sp.Status().Description == "collector exited: secret detail" {
			t.Errorf("%s: status %+v", tc.state, sp.Status())
		}
		linked := len(sp.Links()) == 1 && sp.Links()[0].SpanContext.SpanID() == submitter.SpanID()
		if linked != tc.link {
			t.Errorf("%s: links %v, want the submitter's: %v", tc.state, sp.Links(), tc.link)
		}
		pub := publishes[sp.SpanContext().SpanID()]
		if pub == nil {
			t.Fatalf("%s: no %s child", tc.state, spanRunPublish)
		}
		if attrOf(pub, attrRunEvidence) != "true" || pub.Status().Code == codes.Error {
			t.Errorf("%s: publish %v, %+v", tc.state, pub.Attributes(), pub.Status())
		}
	}
}

func attrOf(sp sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range sp.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

// TestRunErrorCodesNameTheCauseNotTheMessage classifies a run's error.
func TestRunErrorCodesNameTheCauseNotTheMessage(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want Code
	}{
		{errors.New("collector exited"), CodeRunFailed},
		{persistenceError(errors.New("disk full")), CodeStorageError},
		{&Error{Code: CodeCancelled, Message: "cancelled"}, CodeCancelled},
	} {
		if got := runErrorCode(tc.err); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.err, got, tc.want)
		}
	}
}
