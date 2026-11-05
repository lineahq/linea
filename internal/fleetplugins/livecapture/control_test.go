package livecapture

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
)

// The control stream speaks the extension's typed protocol: identity first,
// numbered complete desired states back, enum session states in reports.
func TestWatchSpeaksTypedProtocol(t *testing.T) {
	s, request, _ := fixture(t)
	s.mu.Lock()
	s.dbOK = true
	s.mu.Unlock()
	server := grpc.NewServer()
	controlpb.RegisterCaptureControlServer(server, captureControl{server: s})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+strings.Repeat("d", 64)), 10*time.Second)
	defer cancel()
	stream, err := controlpb.NewCaptureControlClient(conn).WatchCaptures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity := &controlpb.Identity{Cluster: "cluster", Pod: "typed", CollectorGroup: "test", CollectorVersion: "0.162.0", Enabled: true, Capabilities: []string{},
		CapturePoints: []*controlpb.CapturePoint{{Pipeline: "traces", Point: "egress", Signal: controlpb.Signal_SIGNAL_TRACES, Sensitivity: controlpb.Sensitivity_SENSITIVITY_REDACTED}}}
	send := func(r *controlpb.CollectorReport) {
		t.Helper()
		r.Identity = identity
		if err := stream.Send(r); err != nil {
			t.Fatal(err)
		}
	}
	send(&controlpb.CollectorReport{})
	first, err := stream.Recv()
	if err != nil || first.GetGeneration() != 1 || len(first.GetSessions()) != 0 {
		t.Fatalf("first desired state %v, %v", first, err)
	}
	id := identityKey("credential", Identity{Cluster: "cluster", Pod: "typed", Group: "test"})
	s.mu.Lock()
	p := s.peers[id]
	s.mu.Unlock()
	if p == nil || p.info.Identity.Points[0] != (CatalogPoint{Pipeline: "traces", Point: "egress", Signal: "traces", Sensitivity: "redacted"}) || p.info.Identity.CollectorVersion != "0.162.0" {
		t.Fatal("identity not decoded from the typed report")
	}
	request.Targets = []TargetRequest{{p.info.ID, p.info.Generation}}
	request.Sampling = &Sampling{Rate: .5, Mode: "random"}
	request.Filter.Traces.Span = []string{`name == "checkout"`}
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := stream.Recv()
	if err != nil || desired.GetGeneration() != 2 || len(desired.GetSessions()) != 1 {
		t.Fatalf("desired state %v, %v", desired, err)
	}
	session := desired.GetSessions()[0]
	if session.GetPrincipal() != "owner" || session.GetTarget().GetPod() != "typed" || session.GetSignals()[0] != controlpb.Signal_SIGNAL_TRACES ||
		session.GetSampling().GetMode() != controlpb.SamplingMode_SAMPLING_MODE_RANDOM || session.GetFilter().GetSpan()[0] != `name == "checkout"` ||
		session.GetLimits().GetTotalRecords() != 10000 || !session.GetExpiresAt().AsTime().Equal(request.ExpiresAt.Truncate(0)) || session.GetAuthorizedRaw() {
		t.Fatalf("session %v", session)
	}
	state := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			got, err := s.Get(context.Background(), "owner", op.ID)
			if err == nil && got.State == want {
				return
			}
		}
		t.Fatalf("operation never became %s", want)
	}
	status := func(state controlpb.SessionState) *controlpb.SessionStatus {
		return &controlpb.SessionStatus{SessionId: session.GetSessionId(), State: state, ExpiresAt: timestamppb.New(request.ExpiresAt)}
	}
	send(&controlpb.CollectorReport{AppliedGeneration: 2, Sessions: []*controlpb.SessionStatus{status(controlpb.SessionState_SESSION_STATE_ACTIVE)}})
	state("active")
	// Counters are read live without advancing the revision.
	activeOp, _ := s.Get(context.Background(), "owner", op.ID)
	counted := status(controlpb.SessionState_SESSION_STATE_ACTIVE)
	counted.Stats = &controlpb.SessionStats{Selected: 7, EnqueuedRecords: 5, EnqueuedBytes: 640, FilterErrors: 1, Dropped: map[string]int64{"sampling": 2, "queue_full": 0}}
	send(&controlpb.CollectorReport{AppliedGeneration: 2, Sessions: []*controlpb.SessionStatus{counted}})
	stats := func(want TargetStats) Operation {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			got, err := s.Get(context.Background(), "owner", op.ID)
			if err == nil && got.Targets[0].Stats != nil && reflect.DeepEqual(*got.Targets[0].Stats, want) {
				return got
			}
		}
		t.Fatalf("target stats never became %+v", want)
		return Operation{}
	}
	if live := stats(TargetStats{Selected: 7, EnqueuedRecords: 5, EnqueuedBytes: 640, FilterErrors: 1, Dropped: map[string]int64{"sampling": 2}}); live.Revision != activeOp.Revision {
		t.Fatalf("counters advanced the revision %d -> %d", activeOp.Revision, live.Revision)
	}
	// A rejected desired state is counted once however often it is reported.
	for range 2 {
		send(&controlpb.CollectorReport{AppliedGeneration: 2, DesiredStateError: "unsupported", Sessions: []*controlpb.SessionStatus{status(controlpb.SessionState_SESSION_STATE_ACTIVE)}})
	}
	completed := status(controlpb.SessionState_SESSION_STATE_COMPLETED)
	completed.Stats = &controlpb.SessionStats{Selected: 9, EnqueuedRecords: 9, EnqueuedBytes: 900}
	send(&controlpb.CollectorReport{AppliedGeneration: 2, Sessions: []*controlpb.SessionStatus{completed}})
	cleared, err := stream.Recv()
	if err != nil || cleared.GetGeneration() != 3 || len(cleared.GetSessions()) != 0 {
		t.Fatalf("released target still desired: %v, %v", cleared, err)
	}
	// Final counters are saved with the terminal state.
	if final := stats(TargetStats{Selected: 9, EnqueuedRecords: 9, EnqueuedBytes: 900}); final.State != "completed" {
		t.Fatalf("operation %s", final.State)
	}
	if s.observed.desiredRejected.Load() != 1 {
		t.Fatalf("desired_state_rejected_total=%d", s.observed.desiredRejected.Load())
	}
	// Reports with unknown enum values in the catalog are refused like any
	// invalid identity.
	identity.CapturePoints[0].Signal = 42
	send(&controlpb.CollectorReport{AppliedGeneration: 3})
	if _, err := stream.Recv(); err == nil {
		t.Fatal("invalid catalog accepted")
	}
}

func TestDecodeStatsIsBounded(t *testing.T) {
	if v, err := decodeStats(nil); v != nil || err != nil {
		t.Fatal("absent stats decoded")
	}
	many := map[string]int64{}
	for i := range 17 {
		many[string(rune('a'+i))] = 1
	}
	for _, bad := range []*controlpb.SessionStats{{Selected: -1}, {Dropped: many}, {Dropped: map[string]int64{"": 1}}, {Dropped: map[string]int64{strings.Repeat("x", 33): 1}}, {Dropped: map[string]int64{"budget": -1}}} {
		if _, err := decodeStats(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// A Collector's reported filter limits are kept; one that reports none, or a
// zero field, gets the version 1 limits.
func TestDecodeReportFilterLimits(t *testing.T) {
	for _, c := range []struct {
		wire *controlpb.FilterLimits
		want FilterLimits
	}{
		{nil, v1FilterLimits},
		{&controlpb.FilterLimits{MaxConditions: 32}, FilterLimits{MaxConditions: 32, MaxConditionChars: 256, MaxRegexChars: 256}},
		{&controlpb.FilterLimits{MaxConditions: 4, MaxConditionChars: 1 << 20, MaxRegexChars: 64}, FilterLimits{MaxConditions: 4, MaxConditionChars: 1 << 16, MaxRegexChars: 64}},
	} {
		r, err := decodeReport(&controlpb.CollectorReport{Identity: &controlpb.Identity{Pod: "p", CollectorGroup: "g", FilterLimits: c.wire}})
		if err != nil || r.Identity.FilterLimits != c.want {
			t.Fatalf("%v decoded as %+v, %v", c.wire, r.Identity.FilterLimits, err)
		}
	}
}
