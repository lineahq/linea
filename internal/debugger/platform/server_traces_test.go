package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// child and a linea.run.execute child holding the execution's phases; a
// failed run carries an error code and never its message.
func TestRunSpansRecordTheAttemptAndItsPublish(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	s := openPlatform(t, memory.NewData(), Options{Now: time.Now, TracerProvider: provider,
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			phases := req.Phases.Track()
			phases.Next(engine.PhaseCollectorRun)
			if req.FixtureJSON == `{"resourceSpans":[{}]}` {
				err := errors.New("collector exited: secret detail")
				phases.Done(err)
				return nil, err
			}
			phases.Done(nil)
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
	waitFor(t, "eight ended spans", func() bool { return len(recorder.Ended()) == 8 })

	runs := map[string]sdktrace.ReadOnlySpan{}
	publishes := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	executes := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	phases := map[trace.SpanID]sdktrace.ReadOnlySpan{}
	for _, sp := range recorder.Ended() {
		switch sp.Name() {
		case spanRun:
			runs[attrOf(sp, attrRunID)] = sp
		case spanRunPublish:
			publishes[sp.Parent().SpanID()] = sp
		case spanRunExecute:
			executes[sp.Parent().SpanID()] = sp
		case spanPhase + engine.PhaseCollectorRun:
			phases[sp.Parent().SpanID()] = sp
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
		exec := executes[sp.SpanContext().SpanID()]
		if exec == nil {
			t.Fatalf("%s: no %s child", tc.state, spanRunExecute)
		}
		if attrOf(exec, attrPhases) != "1" || (exec.Status().Code == codes.Error) != (tc.code != "") {
			t.Errorf("%s: execute %v, %+v", tc.state, exec.Attributes(), exec.Status())
		}
		phase := phases[exec.SpanContext().SpanID()]
		if phase == nil {
			t.Fatalf("%s: no phase under %s", tc.state, spanRunExecute)
		}
		if phase.StartTime().Before(exec.StartTime()) || phase.EndTime().After(exec.EndTime()) ||
			(phase.Status().Code == codes.Error) != (tc.code != "") || phase.Status().Description != "" || len(phase.Attributes()) != 0 {
			t.Errorf("%s: phase %v-%v in %v-%v, %+v %v", tc.state, phase.StartTime(), phase.EndTime(),
				exec.StartTime(), exec.EndTime(), phase.Status(), phase.Attributes())
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

// TestReportedPhasesAreBoundedAndPlacedInTheExecution: a worker's phases
// become children of the execute span only under known names, at most
// maxPhases of them, never with negative times, and clamped into the span.
func TestReportedPhasesAreBoundedAndPlacedInTheExecution(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	s := &Service{}
	s.newServerTraces(provider)
	span, phases := s.startExecuteSpan(context.Background())
	if phases == nil {
		t.Fatal("a recording execute span records no phases")
	}
	phases.Add(
		engine.Phase{Name: engine.PhaseBundleDecode, Offset: 0, Duration: time.Millisecond},
		engine.Phase{Name: "secret.user.value", Offset: 0, Duration: time.Millisecond},
		engine.Phase{Name: engine.PhaseCollectorRun, Offset: -time.Second, Duration: time.Millisecond},
		engine.Phase{Name: engine.PhaseAnalysis, Offset: 0, Duration: -time.Millisecond},
		engine.Phase{Name: engine.PhaseResultUpload, Offset: time.Hour, Duration: time.Hour, Failed: true},
	)
	for range maxPhases {
		phases.Add(engine.Phase{Name: engine.PhaseResultEncode})
	}
	s.endExecuteSpan(span, phases, nil)

	var exec sdktrace.ReadOnlySpan
	names := map[string]int{}
	var children []sdktrace.ReadOnlySpan
	for _, sp := range recorder.Ended() {
		if sp.Name() == spanRunExecute {
			exec = sp
			continue
		}
		names[sp.Name()]++
		children = append(children, sp)
	}
	if exec == nil || attrOf(exec, attrPhases) != fmt.Sprint(maxPhases) || exec.Status().Code == codes.Error {
		t.Fatalf("execute span %v", exec)
	}
	want := map[string]int{spanPhase + engine.PhaseBundleDecode: 1, spanPhase + engine.PhaseResultUpload: 1,
		spanPhase + engine.PhaseResultEncode: maxPhases - 2}
	if !maps.Equal(names, want) {
		t.Fatalf("phase spans %v, want %v", names, want)
	}
	for _, sp := range children {
		if sp.Parent().SpanID() != exec.SpanContext().SpanID() || sp.StartTime().Before(exec.StartTime()) || sp.EndTime().After(exec.EndTime()) {
			t.Errorf("%s at %v-%v is outside its execution %v-%v", sp.Name(), sp.StartTime(), sp.EndTime(), exec.StartTime(), exec.EndTime())
		}
		if (sp.Status().Code == codes.Error) != (sp.Name() == spanPhase+engine.PhaseResultUpload) {
			t.Errorf("%s: status %+v", sp.Name(), sp.Status())
		}
	}
}

// TestUntracedExecutionsRecordNoPhases: without export nothing is recorded.
func TestUntracedExecutionsRecordNoPhases(t *testing.T) {
	s := &Service{}
	s.newServerTraces(nil)
	span, phases := s.startExecuteSpan(context.Background())
	if phases != nil {
		t.Fatal("an execution without trace export records phases")
	}
	s.endExecuteSpan(span, phases, errors.New("failed"))
}

// TestAnotherInstanceLinksTheRunToItsSubmitter: the submitting request's span
// is stored with the run, so the instance that claims it links its
// linea.run span to that request; a run submitted without a span stores none.
func TestAnotherInstanceLinksTheRunToItsSubmitter(t *testing.T) {
	data := memory.NewData()
	held := make(chan engine.Request, 1)
	a := openPlatform(t, data, Options{Shared: true, Execute: blockingExecute(held)})
	submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, held)
	submitter := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{7}, SpanID: trace.SpanID{8}, TraceFlags: trace.FlagsSampled, Remote: true})
	linked, err := a.submit(trace.ContextWithSpanContext(a.ctx, submitter), testCaller, ModeQuick,
		engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	unlinked := submit(t, a, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	b := openPlatform(t, data, Options{Shared: true, TracerProvider: provider,
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return report(req, "completed"), nil
		}})
	for _, r := range []*Handle{linked, unlinked} {
		if v := waitRun(t, r); v.State != StateDone {
			t.Fatalf("run = %s, %v", v.State, r.Err())
		}
	}
	waitFor(t, "two run spans", func() bool {
		n := 0
		for _, sp := range recorder.Ended() {
			if sp.Name() == spanRun {
				n++
			}
		}
		return n == 2
	})
	for _, sp := range recorder.Ended() {
		if sp.Name() != spanRun {
			continue
		}
		links := sp.Links()
		switch attrOf(sp, attrRunID) {
		case linked.ID():
			if len(links) != 1 || links[0].SpanContext.TraceID() != submitter.TraceID() || links[0].SpanContext.SpanID() != submitter.SpanID() || !links[0].SpanContext.IsSampled() {
				t.Fatalf("linked run's links = %+v", links)
			}
		case unlinked.ID():
			if len(links) != 0 {
				t.Fatalf("unlinked run's links = %+v", links)
			}
		default:
			t.Fatalf("unexpected run span %s", attrOf(sp, attrRunID))
		}
	}
	records, err := b.repository.Runs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		var m runMetadata
		if err := json.Unmarshal(record.Metadata, &m); err != nil {
			t.Fatal(err)
		}
		if (m.Submitted != nil) != (record.ID == linked.ID()) {
			t.Fatalf("run %s stores submitted %+v", record.ID, m.Submitted)
		}
	}
	if (&storedSpan{TraceID: "zz", SpanID: "01"}).spanContext().IsValid() {
		t.Fatal("a malformed stored span links")
	}
}
