package livecapturedebug

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/collector/provider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	"github.com/lineahq/linea/collector/livecapture/livecaptureextension"
)

// fakeRun is the debugger's side of a run: it decodes settings, hands out
// loopback listeners, keeps the hooks for the test to call and records the
// evidence the provider reports.
type fakeRun struct {
	components []provider.Component
	settings   string
	pipelines  []provider.Pipeline

	ready, inputsDone []func(context.Context) error
	closes            []func() error

	mu                              sync.Mutex
	supplied, limitations, warnings []string
	listeners                       []net.Listener
}

func (r *fakeRun) Components() []provider.Component { return r.components }
func (r *fakeRun) Pipelines() []provider.Pipeline   { return r.pipelines }
func (r *fakeRun) Settings(target any) error {
	if r.settings == "" {
		return nil
	}
	return json.Unmarshal([]byte(r.settings), target)
}
func (r *fakeRun) Listener(string, string) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		r.listeners = append(r.listeners, l)
	}
	return l, err
}
func (r *fakeRun) Record(string, string, string, []byte) error         { return nil }
func (r *fakeRun) OnReady(f func(context.Context) error)               { r.ready = append(r.ready, f) }
func (r *fakeRun) OnInput(func(context.Context, provider.Input) error) {}
func (r *fakeRun) OnInputsDone(f func(context.Context) error)          { r.inputsDone = append(r.inputsDone, f) }
func (r *fakeRun) OnClose(f func() error)                              { r.closes = append(r.closes, f) }
func (r *fakeRun) Logf(string, ...any)                                 {}
func (r *fakeRun) Supplied(c, what string)                             { r.add(&r.supplied, c, what) }
func (r *fakeRun) Limitation(c, what string)                           { r.add(&r.limitations, c, what) }
func (r *fakeRun) Warning(c, what string)                              { r.add(&r.warnings, c, what) }
func (r *fakeRun) add(to *[]string, c, what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	*to = append(*to, c+": "+what)
}

func (r *fakeRun) close(t *testing.T) {
	t.Helper()
	for _, f := range r.closes {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
}

func hasLine(lines []string, part string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, part) })
}

const oneSession = `{"sessions":[{"request":{"session_id":"s1","principal":"p","capture_points":[{"pipeline":"traces","point":"after"}],"signals":["traces"]}}]}`

func TestCheckReportsWhatKeepsSessionsFromStarting(t *testing.T) {
	capture := []provider.Pipeline{{ID: "traces", Signal: "traces", Processors: []provider.Component{{ID: "processor/capture"}}}}
	tests := []struct {
		name     string
		config   map[string]any
		settings string
		want     []string
		proposal bool
	}{
		{"disabled extension", map[string]any{"enabled": false}, oneSession, []string{"not enabled"}, false},
		{"no control and no local_control", map[string]any{"enabled": true}, oneSession, []string{"neither control.endpoint nor local_control"}, false},
		{"no sessions", map[string]any{"enabled": true, "control": map[string]any{"endpoint": "x:1"}}, "", []string{"declares no capture session"}, true},
		{"disabled state needs no session", map[string]any{"enabled": true, "control": map[string]any{"endpoint": "x:1"}}, `{"disabled":true}`, nil, false},
		{"ready", map[string]any{"enabled": true, "local_control": map[string]any{"enabled": true}}, oneSession, nil, false},
		{"bad settings", map[string]any{"enabled": true}, `{"sessions":3}`, []string{"mocks.providers.livecapture:"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &fakeRun{components: []provider.Component{{ID: "extension/livecapture", Config: tt.config}}, settings: tt.settings, pipelines: capture}
			findings := check(run)
			if len(findings) != len(tt.want) {
				t.Fatalf("findings = %+v, want %d", findings, len(tt.want))
			}
			for i, want := range tt.want {
				if !strings.Contains(findings[i].Message, want) {
					t.Errorf("finding %d = %q, want %q", i, findings[i].Message, want)
				}
			}
			if got := len(findings) > 0 && findings[len(findings)-1].Settings != nil; got != tt.proposal {
				t.Errorf("proposal = %v, want %v", got, tt.proposal)
			}
		})
	}
}

func TestProposeSessionNamesEachPipelinesFirstCaptureProcessor(t *testing.T) {
	pipelines := []provider.Pipeline{
		{ID: "traces", Signal: "traces", Processors: []provider.Component{{ID: "processor/batch"}, {ID: "processor/capture/in"}, {ID: "processor/capture/out"}}},
		{ID: "logs", Signal: "logs", Processors: []provider.Component{{ID: "processor/capture", Config: map[string]any{"pipeline": "logs/raw", "sensitivity": "raw"}}}},
		{ID: "metrics", Signal: "metrics", Processors: []provider.Component{{ID: "processor/capturex"}}},
	}
	session, ok := proposeSession(pipelines)
	if !ok {
		t.Fatal("no session proposed")
	}
	request := session["request"].(map[string]any)
	wantPoints := []livecaptureextension.Point{{Pipeline: "traces", Point: "in"}, {Pipeline: "logs/raw", Point: ""}}
	if got := request["capture_points"].([]livecaptureextension.Point); !slices.Equal(got, wantPoints) {
		t.Errorf("capture points = %v, want %v", got, wantPoints)
	}
	if got := request["signals"].([]string); !slices.Equal(got, []string{"traces", "logs"}) {
		t.Errorf("signals = %v", got)
	}
	if session["authorized_raw"] != true {
		t.Error("a raw capture processor needs authorized_raw")
	}
	if _, ok := proposeSession([]provider.Pipeline{{ID: "traces", Signal: "traces", Processors: []provider.Component{{ID: "processor/batch"}}}}); ok {
		t.Error("a configuration without a capture processor got a proposal")
	}
}

func TestLocalEndpointDefaultsOnlyWhenEnabled(t *testing.T) {
	tests := []struct {
		config map[string]any
		want   string
	}{
		{map[string]any{}, ""},
		{map[string]any{"local_control": map[string]any{"enabled": false, "endpoint": "127.0.0.1:1"}}, ""},
		{map[string]any{"local_control": map[string]any{"enabled": true}}, "127.0.0.1:17777"},
		{map[string]any{"local_control": map[string]any{"enabled": true, "endpoint": "127.0.0.1:9"}}, "127.0.0.1:9"},
	}
	for _, tt := range tests {
		if got := localEndpoint(provider.Component{Config: tt.config}); got != tt.want {
			t.Errorf("localEndpoint(%v) = %q, want %q", tt.config, got, tt.want)
		}
	}
}

func controlRun(settings string) *fakeRun {
	return &fakeRun{components: []provider.Component{{ID: "extension/livecapture", Config: map[string]any{"enabled": true, "control": map[string]any{"endpoint": "replaced"}}}}, settings: settings}
}

func localRun(srv *httptest.Server) *fakeRun {
	return &fakeRun{components: []provider.Component{{ID: "extension/livecapture", Config: map[string]any{"enabled": true,
		"local_control": map[string]any{"enabled": true, "endpoint": strings.TrimPrefix(srv.URL, "http://")}}}}, settings: oneSession}
}

func activeReport(applied uint64, selected int64) *controlpb.CollectorReport {
	return &controlpb.CollectorReport{AppliedGeneration: applied, Sessions: []*controlpb.SessionStatus{{SessionId: "s1", State: controlpb.SessionState_SESSION_STATE_ACTIVE,
		Stats: &controlpb.SessionStats{Selected: selected, EnqueuedRecords: selected, EnqueuedBytes: 10 * selected}}}}
}

// TestControlServiceSendsSessionsAndDeclaresReports plays the extension
// against the provider's control service: it reports, receives the
// scenario's desired state, and the report that applies the state resent
// after the inputs becomes the run's evidence. A periodic report from
// before the resend must not stand in for it.
func TestControlServiceSendsSessionsAndDeclaresReports(t *testing.T) {
	run := controlRun(oneSession)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := prepare(ctx, run); err != nil {
		t.Fatal(err)
	}
	if len(run.listeners) != 1 || len(run.ready) != 1 || len(run.inputsDone) != 1 || len(run.closes) != 1 {
		t.Fatalf("prepare registered %d listeners, %d/%d/%d hooks", len(run.listeners), len(run.ready), len(run.inputsDone), len(run.closes))
	}
	conn, err := grpc.NewClient(run.listeners[0].Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := controlpb.NewCaptureControlClient(conn).WatchCaptures(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ready := make(chan error, 1)
	go func() { ready <- run.ready[0](ctx) }()
	if err := stream.Send(&controlpb.CollectorReport{}); err != nil {
		t.Fatal(err)
	}
	desired, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if desired.GetGeneration() != 1 || len(desired.GetSessions()) != 1 || desired.GetSessions()[0].GetSessionId() != "s1" {
		t.Fatalf("desired state = %v", desired)
	}
	if desired.GetSessions()[0].GetExpiresAt() == nil {
		t.Error("a session without expires_at was sent without one")
	}
	if err := stream.Send(activeReport(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		t.Fatalf("OnReady = %v", err)
	}
	// A periodic report of generation 1 leaves a signal, whether the
	// service keeps it before or after the resend below.
	if err := stream.Send(activeReport(1, 1)); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- run.inputsDone[0](ctx) }()
	if desired, err := stream.Recv(); err != nil || desired.GetGeneration() != 2 {
		t.Fatalf("resent desired state = %v, %v", desired, err)
	}
	select {
	case err := <-done:
		t.Fatalf("OnInputsDone returned (%v) before the extension applied the resent state", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := stream.Send(activeReport(2, 3)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("OnInputsDone = %v", err)
	}
	run.close(t)

	for _, want := range []string{"session s1 for 1 capture point(s), signals [traces]", "session s1 active: 3 selected, 3 records (30 bytes) enqueued"} {
		if !hasLine(run.supplied, want) {
			t.Errorf("supplied = %q, want %q", run.supplied, want)
		}
	}
	if len(run.warnings) != 0 {
		t.Errorf("warnings = %q", run.warnings)
	}
	if !hasLine(run.limitations, "the control service is the debugger's") {
		t.Errorf("limitations = %q", run.limitations)
	}
}

func TestControlServiceWarnsAboutRejectionsAndFailures(t *testing.T) {
	ctl := &control{reports: make(chan struct{}, 1)}
	ctl.keep(&controlpb.CollectorReport{AppliedGeneration: 1, DesiredStateError: "two sessions",
		Sessions: []*controlpb.SessionStatus{{SessionId: "s1", State: controlpb.SessionState_SESSION_STATE_FAILED, Reason: "no capture point"}}})
	run := &fakeRun{}
	ctl.declare(run, "extension/livecapture", settings{Disabled: true}, "limited")
	for _, want := range []string{"rejected the desired state: two sessions", "session s1 failed: 0 selected, 0 records (0 bytes) enqueued, no capture point"} {
		if !hasLine(run.warnings, want) {
			t.Errorf("warnings = %q, want %q", run.warnings, want)
		}
	}
	if !hasLine(run.supplied, "desired state: capture disabled") {
		t.Errorf("supplied = %q", run.supplied)
	}
}

func TestControlServiceWarnsWhenTheExtensionNeverConnects(t *testing.T) {
	run := controlRun(oneSession)
	if err := prepare(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := run.inputsDone[0](ctx); err != nil {
		t.Fatal(err)
	}
	run.close(t)
	if !hasLine(run.warnings, "never connected to the control service") || len(run.supplied) != 0 {
		t.Errorf("warnings = %q, supplied = %q", run.warnings, run.supplied)
	}
}

// TestLocalControlPostsSessionsAndPollsStatus plays the extension's
// development local_control API.
func TestLocalControlPostsSessionsAndPollsStatus(t *testing.T) {
	var mu sync.Mutex
	var posted []livecaptureextension.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
			var req livecaptureextension.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			posted = append(posted, req)
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
			_, _ = io.WriteString(w, `{"sessions":[{"session_id":"s1","state":"active","stats":{"selected":2,"enqueued_records":2,"enqueued_bytes":20}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	run := localRun(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := prepare(ctx, run); err != nil {
		t.Fatal(err)
	}
	if len(run.listeners) != 0 {
		t.Error("local_control needs no control listener")
	}
	if err := run.ready[0](ctx); err != nil {
		t.Fatal(err)
	}
	if err := run.inputsDone[0](ctx); err != nil {
		t.Fatal(err)
	}
	run.close(t)

	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0].SessionID != "s1" || posted[0].ExpiresAt.IsZero() {
		t.Fatalf("posted = %+v", posted)
	}
	if !hasLine(run.supplied, "session s1 active: 2 selected, 2 records (20 bytes) enqueued") || len(run.warnings) != 0 {
		t.Errorf("supplied = %q, warnings = %q", run.supplied, run.warnings)
	}
	if !hasLine(run.limitations, "local_control.allow_raw") {
		t.Errorf("limitations = %q", run.limitations)
	}
}

func TestLocalControlWarnsWhenASessionIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "raw capture is not allowed", http.StatusForbidden)
	}))
	defer srv.Close()
	run := localRun(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := prepare(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := run.ready[0](ctx); err != nil {
		t.Fatal(err)
	}
	run.close(t)
	if !hasLine(run.warnings, "session s1 was not started through local_control") || !hasLine(run.warnings, "403 Forbidden: raw capture is not allowed") {
		t.Errorf("warnings = %q", run.warnings)
	}
}
