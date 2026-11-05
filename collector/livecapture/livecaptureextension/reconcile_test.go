package livecaptureextension

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestUpdateRetainsBudgetsAndDeadline(t *testing.T) {
	m, _ := testManager(t)
	r := traceRequest("budget-update")
	r.Limits.TotalRecords = 3
	r.ExpiresAt = time.Now().Add(30 * time.Second)
	old := start(t, m, r)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	r.Filter.Traces.Span = []string{`name != "ignored"`}
	r.ExpiresAt = time.Now().Add(time.Minute)
	updated := start(t, m, r)
	if updated == old || updated.stats().EnqueuedRecords != 2 || !updated.request.ExpiresAt.Equal(old.request.ExpiresAt) {
		t.Fatal("update reset session counters or extended its deadline")
	}
	m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	<-updated.done
	if updated.stats().EnqueuedRecords != 3 || updated.termination.Load().state != "limit_reached" {
		t.Fatal("updated session exceeded the original record budget")
	}
}

func TestRawPermissionRevocationFailsClosed(t *testing.T) {
	m, _ := testManager(t)
	m.Unregister(CapturePoint{Point: Point{Pipeline: "traces", Point: "ingress"}, Signal: Traces})
	if err := m.Register(CapturePoint{Point: Point{Pipeline: "traces", Point: "ingress"}, Signal: Traces, Sensitivity: "raw"}); err != nil {
		t.Fatal(err)
	}
	r := traceRequest("raw-revocation")
	r.ExpiresAt = time.Now().Add(time.Minute)
	if err := m.Reconcile(context.Background(), []AuthorizedRequest{{Request: r, AuthorizedRaw: true}}, false); err != nil {
		t.Fatal(err)
	}
	s := m.active.Load()
	if err := m.Reconcile(context.Background(), []AuthorizedRequest{{Request: r, AuthorizedRaw: false}}, false); err == nil {
		t.Fatal("revocation accepted as authorized")
	}
	if m.active.Load() != nil {
		t.Fatal("raw capture continued after authorization revocation")
	}
	<-s.done
	if s.termination.Load().state != "failed" {
		t.Fatal("revoked session did not fail closed")
	}
}

func TestKillSwitchPrecedesInvalidDesiredSet(t *testing.T) {
	m, _ := testManager(t)
	s := start(t, m, traceRequest("kill-first"))
	if err := m.Reconcile(context.Background(), make([]AuthorizedRequest, 2), true); err != nil {
		t.Fatal(err)
	}
	if m.active.Load() != nil || m.enabled.Load() {
		t.Fatal("invalid desired set prevented the kill switch")
	}
	<-s.done
}

func TestRemoteRequiresAbsoluteDeadline(t *testing.T) {
	m, _ := testManager(t)
	if err := m.Reconcile(context.Background(), []AuthorizedRequest{{Request: traceRequest("no-deadline")}}, false); err == nil || m.active.Load() != nil {
		t.Fatal("remote desired state without expiry could be replayed indefinitely")
	}
}

func TestGroupDesiredStateCapturesEachPodIndependently(t *testing.T) {
	first, firstSink := testManager(t)
	second, secondSink := testManager(t)
	second.config.Pod = "pod-b"
	r := traceRequest("group-session")
	r.Target.CollectorGroup = "development"
	r.ExpiresAt = time.Now().Add(20 * time.Second)
	for _, manager := range []*Manager{first, second} {
		if err := manager.Reconcile(context.Background(), []AuthorizedRequest{{Request: r}}, false); err != nil {
			t.Fatal(err)
		}
		manager.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	}
	for pod, sink := range map[string]*recordingDelivery{"pod-a": firstSink, "pod-b": secondSink} {
		if len(sink.trace) != 1 || sink.trace[0].SpanCount() != 2 {
			t.Fatal("group session missed pod")
		}
		attribute, ok := sink.trace[0].ResourceSpans().At(0).Resource().Attributes().Get("capture.collector.pod")
		if !ok || attribute.Str() != pod {
			t.Fatal("group copies lost their pod identity")
		}
	}
	if err := first.Reconcile(context.Background(), nil, false); err != nil {
		t.Fatal(err)
	}
	if first.active.Load() != nil || second.active.Load() == nil {
		t.Fatal("pod-local stop affected another pod")
	}
}

func TestRedactionMasksBeforeUTF8SafeTruncation(t *testing.T) {
	r, err := newRedactor(Redaction{MaxStringLength: 8, MaxLogBodyLength: 8, Masks: []string{`Bearer [A-Za-z0-9]+`}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.text("Bearer SECRET123456", 8); strings.Contains(got, "Bearer") || strings.Contains(got, "SECRET") {
		t.Fatal("truncation exposed a partial secret")
	}
	if got := truncate("日本語", 8); !utf8.ValidString(got) || len(got) > 8 {
		t.Fatal("truncated invalid UTF-8")
	}
}

func TestSessionProtoRoundTrip(t *testing.T) {
	r := Request{SessionID: "round-trip", Principal: "engineer", Target: Target{CollectorGroup: "development", Pod: "pod-a"}, CapturePoints: []Point{{Pipeline: "traces", Point: "ingress"}}, Signals: []Signal{Traces, Logs}, Sampling: &Sampling{Rate: .5, Mode: "random"}, Limits: SessionLimits{BytesPerSecond: 1, RecordsPerSecond: 2, TotalBytes: 3, TotalRecords: 4, RecordBytes: 5}, ExpiresAt: time.Unix(1700000000, 5).UTC()}
	r.Filter.Resource = []string{"a"}
	r.Filter.Traces.Span = []string{"b"}
	r.Filter.Logs.Log = []string{"c"}
	r.Filter.Metrics.Metric = []string{"d"}
	r.Filter.Metrics.Datapoint = []string{"e"}
	want := AuthorizedRequest{Request: r, AuthorizedRaw: true}
	got, err := requestFromProto(want.Proto())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the session:\n got %+v\nwant %+v", got, want)
	}
}
