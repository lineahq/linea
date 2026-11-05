package livecaptureextension

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
)

type traceIngress struct {
	ptraceotlp.UnimplementedGRPCServer
	received chan ptrace.Traces
}

func (s *traceIngress) Export(_ context.Context, r ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	s.received <- r.Traces()
	return ptraceotlp.NewExportResponse(), nil
}

func TestInternalOTLPFactoryByteQueueAndOutage(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	ingress := &traceIngress{received: make(chan ptrace.Traces, 10)}
	ptraceotlp.RegisterGRPCServer(server, ingress)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	m, _ := testManager(t)
	m.host = testHost{}
	m.config.Delivery.ClientConfig.Endpoint = listener.Addr().String()
	m.config.Limits.MaxQueueBytes = 2048
	m.delivery = newOTLPDelivery(m)
	r := traceRequest("actual-otlp")
	s := start(t, m, r)
	m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	select {
	case received := <-ingress.received:
		if received.SpanCount() != 2 {
			t.Fatal("internal exporter lost selected spans")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture not delivered via actual OTLP exporter")
	}
	server.Stop()
	started := time.Now()
	for range 100 {
		m.CaptureTraces(context.Background(), r.CapturePoints[0], testTraces())
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("dead ingress blocked capture calls")
	}
	if s.stats().Dropped["queue_full"] == 0 {
		t.Fatal("bounded byte queue never overflowed")
	}
	occupancy := m.settings.MeterProvider.(queueMeterProvider).queues.sum(context.Background())
	if occupancy <= 0 || occupancy > 2048/3 {
		t.Fatalf("queue occupancy %d outside byte cap", occupancy)
	}
	// Stopping ends admission only; queued copies keep draining in the
	// extension's exporter.
	stopStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := m.StopSession(ctx, r.SessionID); err != nil {
		t.Fatal(err)
	}
	if time.Since(stopStart) > 500*time.Millisecond {
		t.Fatal("stop waited for the exporter")
	}
}

func TestLocalControlStrictDecodingAndLiveLifecycle(t *testing.T) {
	m, _ := testManager(t)
	if err := m.Start(context.Background(), testHost{}); err != nil {
		t.Fatal(err)
	}
	url := "http://" + m.local.listener.Addr().String()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, body := range []string{`{"session_id":"x","endpoint":"attacker:4317"}`, `{} {}`, `{"enabled":true}`} {
		response, err := client.Post(url+"/v1/sessions", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatal("unknown/session configuration field accepted")
		}
	}
	r := traceRequest("local-session")
	encoded, _ := json.Marshal(r)
	response, err := client.Post(url+"/v1/sessions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("local session did not activate")
	}
	req, _ := http.NewRequest(http.MethodGet, url+"/v1/status", nil)
	req.Header.Set("Origin", "https://untrusted.example")
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatal("cross-origin local request accepted")
	}
	req, _ = http.NewRequest(http.MethodDelete, url+"/v1/sessions/"+r.SessionID, nil)
	response, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent || m.active.Load() != nil {
		t.Fatal("local stop failed")
	}
}

// This is a test fixture for the client protocol, not a platform implementation.
type controlFixture struct {
	controlpb.UnimplementedCaptureControlServer
	calls   atomic.Int64
	desired []*controlpb.DesiredState // Sent in order on the first stream.
	reports chan *controlpb.CollectorReport
}

func (f *controlFixture) WatchCaptures(stream grpc.BidiStreamingServer[controlpb.CollectorReport, controlpb.DesiredState]) error {
	call := f.calls.Add(1)
	hello, err := stream.Recv()
	if err != nil {
		return err
	}
	f.reports <- hello
	if call == 1 {
		for _, desired := range f.desired {
			if err := stream.Send(desired); err != nil {
				return err
			}
			report, err := stream.Recv()
			if err != nil {
				return err
			}
			f.reports <- report
		}
		return nil
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func startControl(t *testing.T, desired ...*controlpb.DesiredState) (*Manager, *recordingDelivery, *controlFixture) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &controlFixture{desired: desired, reports: make(chan *controlpb.CollectorReport, 100)}
	server := grpc.NewServer()
	controlpb.RegisterCaptureControlServer(server, fixture)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	m, sink := testManager(t)
	m.config.Control.Endpoint = listener.Addr().String()
	m.config.Control.TLS.Insecure = true
	if err := m.Start(context.Background(), testHost{}); err != nil {
		t.Fatal(err)
	}
	return m, sink, fixture
}

func remoteSession(id string, expires time.Time) *controlpb.Session {
	return &controlpb.Session{SessionId: id, Principal: "test-engineer", CapturePoints: []*controlpb.Point{{Pipeline: "traces", Point: "ingress"}}, Signals: []controlpb.Signal{controlpb.Signal_SIGNAL_TRACES}, ExpiresAt: timestamppb.New(expires)}
}

func nextReport(t *testing.T, f *controlFixture) *controlpb.CollectorReport {
	t.Helper()
	select {
	case report := <-f.reports:
		return report
	case <-time.After(3 * time.Second):
		t.Fatal("no report")
		return nil
	}
}

func TestOutboundControlReconnectPreservesLocalTTL(t *testing.T) {
	expires := time.Now().Add(500 * time.Millisecond)
	m, _, fixture := startControl(t, &controlpb.DesiredState{Generation: 1, Sessions: []*controlpb.Session{remoteSession("remote", expires)}})
	wait(t, func() bool { return m.active.Load() != nil })
	s := m.active.Load()
	wait(t, func() bool { return fixture.calls.Load() >= 2 })
	if !s.request.ExpiresAt.Equal(expires) {
		t.Fatal("reconnect extended deadline")
	}
	<-s.done
	if s.termination.Load().state != "expired" {
		t.Fatal("control disconnect prevented expiry")
	}
	hello := nextReport(t, fixture)
	identity := hello.GetIdentity()
	if identity.GetPod() != "pod-a" || len(identity.GetCapturePoints()) != 3 || hello.GetAppliedGeneration() != 0 {
		t.Fatal("identity/capture discovery missing")
	}
	if identity.GetCollectorVersion() == "" || identity.GetCollectorVersion()[0] == 'v' {
		t.Fatalf("collector version %q not read from the build", identity.GetCollectorVersion())
	}
	if limits := identity.GetFilterLimits(); limits.GetMaxConditions() != 16 || limits.GetMaxConditionChars() != 256 || limits.GetMaxRegexChars() != 256 {
		t.Fatalf("filter limits %v differ from the ones the filter compiler enforces", limits)
	}
	applied := nextReport(t, fixture)
	if applied.GetAppliedGeneration() != 1 || applied.GetDesiredStateError() != "" || len(applied.GetSessions()) != 1 || applied.GetSessions()[0].GetState() != controlpb.SessionState_SESSION_STATE_ACTIVE {
		t.Fatalf("desired state not acknowledged: %v", applied)
	}
}

// A desired state the Collector cannot apply is reported on the stream; the
// stream stays up and the next desired state still applies.
func TestControlReportsRejectedDesiredState(t *testing.T) {
	expires := time.Now().Add(time.Minute)
	unknown := remoteSession("bad-signal", expires)
	unknown.Signals = []controlpb.Signal{controlpb.Signal(42)}
	m, _, fixture := startControl(t,
		&controlpb.DesiredState{Generation: 1, Sessions: []*controlpb.Session{remoteSession("needs-feature", expires)}, RequiredCapabilities: []string{"future_feature"}},
		&controlpb.DesiredState{Generation: 2, Sessions: []*controlpb.Session{unknown}},
		&controlpb.DesiredState{Generation: 3, Sessions: []*controlpb.Session{remoteSession("a", expires), remoteSession("b", expires)}},
		&controlpb.DesiredState{Generation: 4, Sessions: []*controlpb.Session{remoteSession("good", expires)}},
	)
	nextReport(t, fixture)
	for i, want := range []string{"future_feature", "unsupported signal", "at most one session"} {
		report := nextReport(t, fixture)
		if generation := uint64(i + 1); report.GetAppliedGeneration() != generation || !strings.Contains(report.GetDesiredStateError(), want) {
			t.Fatalf("generation %d: report %v, want error containing %q", generation, report, want)
		}
	}
	report := nextReport(t, fixture)
	if report.GetAppliedGeneration() != 4 || report.GetDesiredStateError() != "" {
		t.Fatalf("valid desired state after rejections: %v", report)
	}
	if s := m.active.Load(); s == nil || s.request.SessionID != "good" || fixture.calls.Load() != 1 {
		t.Fatal("rejections tore down the stream or blocked the valid session")
	}
	failed := false
	for _, status := range report.GetSessions() {
		failed = failed || (status.GetSessionId() == "bad-signal" && status.GetState() == controlpb.SessionState_SESSION_STATE_FAILED)
	}
	if !failed {
		t.Fatal("unreadable session has no failed status")
	}
}

var _ component.Host = testHost{}
var _ extension.Extension = (*Manager)(nil)
