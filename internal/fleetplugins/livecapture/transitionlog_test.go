package livecapture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	"github.com/lineahq/linea/pluginsdk"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// logged returns the fixture's lines named event.
func logged(rec *translog.Recorder, event string) []translog.Entry {
	var out []translog.Entry
	for _, e := range rec.Entries() {
		if e.Name() == event {
			out = append(out, e)
		}
	}
	return out
}

// Every operation state change is one line with the operation ID, the old and
// new state and, when there is one, the reason. Nothing else about the
// operation (owner, filter, targets) is logged.
func TestOperationTransitionsAreLogged(t *testing.T) {
	s, request, p := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, p)
	wire := op.Targets[0].WireID
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "active"}}})
	s.mu.Unlock()
	// Each step waits for the writer: a save still queued absorbs the next one,
	// and the log shows saved states only.
	s.saves.waitSettled()
	stopped, err := s.Stop(context.Background(), "owner", op.ID, s.operations[op.ID].op.Revision)
	if err != nil || stopped.State != "stopping" {
		t.Fatalf("stop: %v, %v", stopped.State, err)
	}
	s.saves.waitSettled()
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "completed"}}})
	s.mu.Unlock()
	s.saves.waitSettled()

	type step struct{ from, to, reason string }
	var got []step
	for _, e := range logged(rec, "operation_state") {
		if e.Text("operation") != op.ID {
			t.Fatalf("line for another operation: %v", e)
		}
		got = append(got, step{e.Text("from"), e.Text("to"), e.Text("reason")})
	}
	want := []step{{"none", "starting", ""}, {"starting", "active", ""}, {"active", "stopping", "requested_stop"}, {"stopping", "stopped", "requested_stop"}}
	if len(got) != len(want) {
		t.Fatalf("transitions %v, want %v\n%s", got, want, rec.Raw())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition %d is %v, want %v", i, got[i], want[i])
		}
	}
	for _, secret := range []string{"owner", request.ChannelID, wire, request.Points[0].Point} {
		if strings.Contains(rec.Raw(), secret) {
			t.Fatalf("log line holds %q: %s", secret, rec.Raw())
		}
	}
}

// A stop request for an operation only the database knows (after a restart)
// is also a transition.
func TestStoredOperationTransitionsAreLogged(t *testing.T) {
	s, _, _ := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	repo := s.repo.(*memoryRepo)
	op := Operation{AuthorityID: repo.AuthorityID(), ID: pluginsdk.RandomID(), Owner: "owner", State: "active", Revision: 3, ExpiresAt: time.Now().Add(time.Minute)}
	if err := repo.Insert(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(context.Background(), "owner", op.ID, 3); err != nil {
		t.Fatal(err)
	}
	lines := logged(rec, "operation_state")
	if len(lines) != 1 || lines[0].Text("from") != "active" || lines[0].Text("to") != "interrupted" || lines[0].Text("reason") != "stop_unconfirmed" {
		t.Fatalf("lines %s", rec.Raw())
	}
}

type failingSave struct{ *memoryRepo }

func (failingSave) Save(context.Context, Operation, int64) error { return ErrUnavailable }

// Every fatal exit carries an enum cause: the log line, the error Run returns
// and CauseOf agree, and only the first cause counts.
func TestFatalCarriesItsCause(t *testing.T) {
	s, request, _ := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	s.repo = failingSave{s.repo.(*memoryRepo)}
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stopLocked(s.operations[op.ID], "stopping")
	s.mu.Unlock()
	s.saves.waitSettled()
	s.mu.Lock()
	s.fatalLocked(translog.ReasonStorageLost)
	s.mu.Unlock()
	fatal := <-s.Fatal()
	if CauseOf(fatal) != translog.ReasonSaveFailed || !errors.Is(fatal, ErrUnavailable) {
		t.Fatalf("fatal %v has cause %v", fatal, CauseOf(fatal))
	}
	lines := logged(rec, "plugin_fatal")
	if len(lines) != 1 || lines[0].Text("cause") != "save_failed" || lines[0].Text("level") != "ERROR" {
		t.Fatalf("lines %s", rec.Raw())
	}
	if len(logged(rec, "operation_state")) != 1 {
		t.Fatalf("a transition that was not saved was logged: %s", rec.Raw())
	}
}

func TestCauseOf(t *testing.T) {
	for name, tt := range map[string]struct {
		err  error
		want translog.Reason
	}{
		"nil":            {nil, translog.ReasonShutdown},
		"attached":       {WithCause(translog.ReasonTLSInvalid, ErrInvalid), translog.ReasonTLSInvalid},
		"attached twice": {WithCause(translog.ReasonStorageLost, WithCause(translog.ReasonSaveFailed, ErrUnavailable)), translog.ReasonStorageLost},
		"owner busy":     {ErrOwnerBusy, translog.ReasonOwnerBusy},
		"invalid":        {ErrInvalid, translog.ReasonInvalidInput},
		"unavailable":    {ErrStorageLost, translog.ReasonUnavailable},
		"other":          {errors.New("a secret in an error string"), translog.ReasonFailed},
	} {
		if got := CauseOf(tt.err); got != tt.want {
			t.Errorf("%s: %v, want %v", name, got, tt.want)
		}
	}
	if WithCause(translog.ReasonFailed, nil) != nil {
		t.Fatal("WithCause made an error from nil")
	}
}

// The database is logged where maintain's liveness flag moves: the first failed
// ping, the recovery after failed pings, and the end.
func TestDatabaseTransitionsAreLogged(t *testing.T) {
	log, rec := translog.NewRecorder()
	s := &Server{log: log}
	observe := func(err error) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.observeStorageLocked(err)
	}
	for _, err := range []error{nil, ErrUnavailable, ErrUnavailable, nil, nil, ErrStorageBusy} {
		if observe(err) {
			t.Fatal("fatal too early")
		}
	}
	if !observe(ErrStorageLost) {
		t.Fatal("a lost connection is fatal")
	}
	events := rec.Events()
	if len(events) != 2 || events[0] != "database_degraded" || events[1] != "database_recovered" {
		t.Fatalf("events %v", events)
	}
	if recovered := logged(rec, "database_recovered")[0]; recovered["count"] != float64(2) {
		t.Fatalf("recovered line %v", recovered)
	}
}

// hostStub answers the host session route from mutable state.
type hostStub struct {
	mu          sync.Mutex
	instance    string
	epoch       string
	permissions []string
	down        bool
}

func (h *hostStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(pluginsdk.Session{PluginID: "test", InstanceID: h.instance, Epoch: h.epoch, Permissions: h.permissions})
}

func (h *hostStub) set(epoch string, permissions []string, down bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epoch, h.permissions, h.down = epoch, permissions, down
}

// The host session is logged as lost with a reason and established with its
// epoch, once per transition however often it is checked.
func TestHostSessionTransitionsAreLogged(t *testing.T) {
	s, _, _ := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	s.mu.Lock()
	s.hostOK, s.epoch = false, ""
	s.mu.Unlock()
	one, two := strings.Repeat("1", 32), strings.Repeat("2", 32)
	both := []string{pluginsdk.AgentsRead, pluginsdk.CredentialsIntrospect}
	stub := &hostStub{instance: s.instance}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	s.host.Endpoint = server.URL

	type line struct{ event, reason, epoch string }
	steps := []struct {
		epoch       string
		permissions []string
		down        bool
		want        []line
	}{
		{one, both, false, []line{{"host_session_established", "", one}}},
		{one, both, false, nil},
		{one, both, true, []line{{"host_session_lost", "host_unreachable", ""}}},
		{one, both, true, nil},
		{one, both, false, []line{{"host_session_established", "", one}}},
		{two, both, false, []line{{"host_session_lost", "epoch_changed", ""}, {"host_session_established", "", two}}},
		{two, nil, false, []line{{"host_session_lost", "permissions_missing", ""}}},
		{two, both, false, []line{{"host_session_established", "", two}}},
	}
	seen := 0
	for i, step := range steps {
		stub.set(step.epoch, step.permissions, step.down)
		s.checkHost()
		var got []line
		for _, e := range rec.Entries()[seen:] {
			got = append(got, line{e.Name(), e.Text("reason"), e.Text("epoch")})
		}
		seen = len(rec.Entries())
		if len(got) != len(step.want) {
			t.Fatalf("step %d logged %v, want %v", i, got, step.want)
		}
		for j := range got {
			if got[j] != step.want[j] {
				t.Fatalf("step %d line %d is %v, want %v", i, j, got[j], step.want[j])
			}
		}
	}
	if e := logged(rec, "host_session_established")[0]; e.Text("instance") != s.instance {
		t.Fatalf("established line %v", e)
	}
}

// A peer that connects and leaves is two lines; why it left is an enum, and
// no identity (pod, cluster, group) or credential secret is in them.
func TestPeerConnectionIsLogged(t *testing.T) {
	setResendInterval(t, time.Hour)
	s, _, _ := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	stream, _, send := watch(t, s)
	send(&controlpb.CollectorReport{})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	id := identityKey("credential", Identity{Cluster: "cluster", Pod: "typed", Group: "test"})
	connected := logged(rec, "peer_connected")
	if len(connected) != 1 || connected[0].Text("peer") != id || connected[0].Text("credential") != "credential" || connected[0].Text("link") != "pending" {
		t.Fatalf("connected %s", rec.Raw())
	}
	s.mu.Lock()
	s.retirePeerLocked(s.peers[id], translog.ReasonCredentialRejected)
	s.mu.Unlock()
	// The RPC ends after the handler's deferred log, so the error means the line is written.
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stream survived its peer's retirement")
	}
	left := logged(rec, "peer_disconnected")
	if len(left) != 1 || left[0].Text("peer") != id || left[0].Text("reason") != "credential_rejected" {
		t.Fatalf("disconnected %s", rec.Raw())
	}
	if _, ok := left[0]["duration_ms"]; !ok {
		t.Fatalf("no duration: %v", left[0])
	}
	for _, secret := range []string{"typed", "cluster", strings.Repeat("d", 64)} {
		if strings.Contains(rec.Raw(), secret) {
			t.Fatalf("log holds %q: %s", secret, rec.Raw())
		}
	}
}

// The link state is logged when it changes, not at every refresh.
func TestPeerLinkChangesAreLogged(t *testing.T) {
	f := newLinkFixture(t)
	log, rec := translog.NewRecorder()
	f.s.log = log
	agent := strings.Repeat("a", 32)
	answer := pluginsdk.ResolveResponse{}
	f.answer = func(string) pluginsdk.ResolveResponse { return answer }
	f.add(strings.Repeat("1", 64), "pod")
	steps := []struct {
		answer pluginsdk.ResolveResponse
		want   string
	}{
		{pluginsdk.ResolveResponse{}, "no_match"},
		{pluginsdk.ResolveResponse{}, ""},
		{pluginsdk.ResolveResponse{AgentIDs: []string{agent}}, "linked"},
		{pluginsdk.ResolveResponse{AgentIDs: []string{agent}}, ""},
		{pluginsdk.ResolveResponse{AgentIDs: []string{agent, strings.Repeat("b", 32)}}, "ambiguous"},
	}
	for i, step := range steps {
		answer = step.answer
		f.clock = f.clock.Add(time.Hour)
		before := len(rec.Entries())
		f.pass()
		lines := rec.Entries()[before:]
		switch {
		case step.want == "" && len(lines) != 0:
			t.Fatalf("step %d logged %s", i, rec.Raw())
		case step.want != "" && (len(lines) != 1 || lines[0].Name() != "peer_link" || lines[0].Text("link") != step.want):
			t.Fatalf("step %d: want %s, logged %s", i, step.want, rec.Raw())
		}
	}
	if linked := logged(rec, "peer_link")[1]; linked.Text("agent") != agent {
		t.Fatalf("linked line %v", linked)
	}
}

// Rejected credentials on the three listeners become one summary line each per
// 10 s window, only when there were any, and nothing is logged per request.
func TestAuthFailuresAreSummarized(t *testing.T) {
	s, _, _ := fixture(t)
	log, rec := translog.NewRecorder()
	s.log = log
	start := time.Unix(1_700_000_000, 0)
	s.authFailures = translog.NewFailureSummary(log, translog.SummaryEvery, start)

	httpCall := func(token string) {
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		s.ServeHTTP(httptest.NewRecorder(), req)
	}
	httpCall("wrong-1")
	httpCall("wrong-2")
	httpCall(s.config.serviceToken)
	// A credential the plugin refuses is counted on the gRPC listeners; an outage
	// (the host cannot introspect) is not a rejected credential.
	if err := s.Watch(ctxStream{ctx: context.Background()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("control without a credential: %v", err)
	}
	if err := s.deliver(bearer("unknown"), "traces", nil, 1, nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("delivery while the host is unreachable: %v", err)
	}
	if err := s.deliver(context.Background(), "traces", nil, 1, nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("delivery without a credential: %v", err)
	}
	if err := s.deliver(context.Background(), "logs", nil, 1, nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("delivery without a credential: %v", err)
	}
	if len(rec.Entries()) != 0 {
		t.Fatalf("a request was logged: %s", rec.Raw())
	}
	s.authFailures.Tick(start.Add(9 * time.Second))
	s.authFailures.Tick(start.Add(10 * time.Second))
	counts := map[string]float64{}
	for _, e := range logged(rec, "auth_failures") {
		counts[e.Text("listener")] = e["count"].(float64)
	}
	if len(counts) != 3 || counts["http_api"] != 2 || counts["control_grpc"] != 1 || counts["otlp_grpc"] != 2 {
		t.Fatalf("counts %v: %s", counts, rec.Raw())
	}
	for _, secret := range []string{"wrong-1", "wrong-2", s.config.serviceToken} {
		if strings.Contains(rec.Raw(), secret) {
			t.Fatalf("log holds a credential: %s", rec.Raw())
		}
	}
	s.authFailures.Tick(start.Add(20 * time.Second))
	if len(rec.Entries()) != 3 {
		t.Fatalf("a quiet window logged: %s", rec.Raw())
	}
}

// ctxStream is a server stream that has only a context, enough for a call that
// is refused before it reads or sends.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (c ctxStream) Context() context.Context { return c.ctx }
