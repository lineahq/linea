package livecapture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/pluginsdk"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type memoryRepo struct {
	mu     sync.Mutex
	ops    map[string]Operation
	leases map[string]string
	gen    uint64
}

func newMemoryRepo() *memoryRepo {
	return &memoryRepo{ops: map[string]Operation{}, leases: map[string]string{}}
}
func (r *memoryRepo) AuthorityID() string         { return strings.Repeat("f", 32) }
func (r *memoryRepo) Ready(context.Context) error { return nil }
func (r *memoryRepo) NextGeneration(context.Context, string) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	return r.gen, nil
}
func (r *memoryRepo) Get(_ context.Context, id string) (Operation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.ops[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return cloneOperation(op), nil
}
func (r *memoryRepo) Insert(_ context.Context, op Operation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.ops[op.ID]; exists {
		return ErrConflict
	}
	for _, t := range op.Targets {
		if r.leases[t.PeerID] != "" {
			return ErrConflict
		}
	}
	r.ops[op.ID] = cloneOperation(op)
	for _, t := range op.Targets {
		r.leases[t.PeerID] = op.ID
	}
	return nil
}
func (r *memoryRepo) Save(_ context.Context, op Operation, revision int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ops[op.ID].Revision != revision {
		return ErrPrecondition
	}
	r.ops[op.ID] = cloneOperation(op)
	for _, t := range op.Targets {
		if t.Released {
			delete(r.leases, t.PeerID)
		}
	}
	return nil
}

func fixture(t *testing.T) (*Server, CreateRequest, *peer) {
	t.Helper()
	token := strings.Repeat("d", 64)
	c := Config{ID: "test", Version: "1", serviceToken: strings.Repeat("b", 64), HostEndpoint: "http://127.0.0.1:1"}
	s, err := New(context.Background(), c, newMemoryRepo())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cancel(); s.host.HTTP.CloseIdleConnections() })
	s.hostOK = true
	s.epoch = strings.Repeat("c", 32)
	// A cached introspection stands in for the fleet host.
	hash := sha256.Sum256([]byte(token))
	s.credentials[hash] = cachedCredential{credential: credential{id: "credential", hash: hash, groups: map[string]bool{"test": true}}, admitted: true, until: time.Now().Add(time.Hour)}
	p := &peer{credential: hash, info: PeerInfo{ID: strings.Repeat("a", 64), Generation: 1, CredentialID: "credential", Identity: Identity{Group: "test", Pod: "pod", Cluster: "cluster", Enabled: true, Points: []CatalogPoint{{Pipeline: "traces", Point: "egress", Signal: "traces", Sensitivity: "redacted"}, {Pipeline: "traces", Point: "ingress", Signal: "traces", Sensitivity: "raw"}}}}, lastSeen: time.Now(), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peers[p.info.ID] = p
	channel, err := s.NewChannel(context.Background(), "owner", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := CreateRequest{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, ID: pluginsdk.RandomID(), ChannelID: channel.ID, Targets: []TargetRequest{{p.info.ID, 1}}, Points: []Point{{"traces", "egress"}}, Signals: []string{"traces"}, ExpiresAt: time.Now().Add(30 * time.Second)}
	return s, request, p
}
func TestAdmissionOwnerRawGenerationAndIdempotency(t *testing.T) {
	s, r, p := fixture(t)
	raw := r
	raw.Points = []Point{{"traces", "ingress"}}
	if _, err := s.Create(context.Background(), "owner", raw); !errors.Is(err, ErrForbidden) {
		t.Fatal("raw access admitted")
	}
	stale := r
	stale.PluginInstance = pluginsdk.RandomID()
	if _, err := s.Create(context.Background(), "owner", stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale plugin instance admitted")
	}
	stale = r
	stale.Targets = []TargetRequest{{p.info.ID, 2}}
	if _, err := s.Create(context.Background(), "owner", stale); !errors.Is(err, ErrConflict) {
		t.Fatal("stale generation admitted")
	}
	op, err := s.Create(context.Background(), "owner", r)
	if err != nil {
		t.Fatal(err)
	}
	mutated := op
	mutated.Targets[0].State = "external-mutation"
	if s.operations[r.ID].op.Targets[0].State == "external-mutation" {
		t.Fatal("response aliases mutable execution state")
	}
	retry, err := s.Create(context.Background(), "owner", r)
	if err != nil || retry.ID != op.ID || len(s.operations) != 1 {
		t.Fatal("identical create was not reconciled")
	}
	r.Limits.TotalRecords = 1
	if _, err := s.Create(context.Background(), "owner", r); !errors.Is(err, ErrConflict) {
		t.Fatal("changed-content retry admitted")
	}
	if _, err := s.Get(context.Background(), "other", op.ID); !errors.Is(err, ErrForbidden) {
		t.Fatal("other owner read execution")
	}
	if _, err := s.Stop(context.Background(), "owner", op.ID, 100); !errors.Is(err, ErrPrecondition) {
		t.Fatal("stale stop admitted")
	}
	stop, err := s.Stop(context.Background(), "owner", op.ID, 1)
	if err != nil || stop.State != "stopping" || p.operation != "" {
		t.Fatal("stop did not fence desired start")
	}
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: s.operations[op.ID].op.Targets[0].WireID, State: "active"}}})
	s.mu.Unlock()
	if s.operations[op.ID].op.State != "stopping" {
		t.Fatal("late active acknowledgement undid stop")
	}
	r.ID = pluginsdk.RandomID()
	r.Limits.TotalRecords = 0
	if _, err := s.Create(context.Background(), "owner", r); !errors.Is(err, ErrConflict) {
		t.Fatal("uncertain stop released durable reservation")
	}
	wire := s.operations[op.ID].op.Targets[0].WireID
	s.mu.Lock()
	s.applyReportLocked(p, report{Statuses: []collectorStatus{{SessionID: wire, State: "completed"}}})
	s.mu.Unlock()
	if _, err := s.Create(context.Background(), "owner", r); err != nil {
		t.Fatal("terminal acknowledgement did not release reservation", err)
	}
}

func TestConcurrentClientsCannotShareTarget(t *testing.T) {
	s, request, _ := fixture(t)
	other, err := s.NewChannel(context.Background(), "other", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	second := request
	second.ID = pluginsdk.RandomID()
	second.ChannelID = other.ID
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, entry := range []struct {
		owner   string
		request CreateRequest
	}{{"owner", request}, {"other", second}} {
		go func(owner string, request CreateRequest) {
			<-start
			_, err := s.Create(context.Background(), owner, request)
			results <- err
		}(entry.owner, entry.request)
	}
	close(start)
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("target admission was not exclusive")
	}
}

func TestOrphanedAdmissionCanBeStoppedWithoutReplay(t *testing.T) {
	s, request, _ := fixture(t)
	op, err := s.Create(context.Background(), "owner", request)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	delete(s.operations, op.ID)
	s.mu.Unlock()
	stopped, err := s.Stop(context.Background(), "owner", op.ID, op.Revision)
	if err != nil || stopped.State != "interrupted" || stopped.Reason != "stop_unconfirmed" {
		t.Fatal("orphaned admission was not fenced")
	}
}

func TestExpiryEndsUnfinishedTargets(t *testing.T) {
	for _, tracked := range []bool{true, false} {
		s, request, _ := fixture(t)
		op, err := s.Create(context.Background(), "owner", request)
		if err != nil {
			t.Fatal(err)
		}
		// Expire it as an active operation, then as one known only to storage.
		past := time.Now().Add(-time.Second)
		s.mu.Lock()
		s.operations[op.ID].op.ExpiresAt = past
		s.operations[op.ID].op.Targets[0].State = "active"
		if !tracked {
			delete(s.operations, op.ID)
		}
		s.mu.Unlock()
		repo := s.repo.(*memoryRepo)
		repo.mu.Lock()
		stored := repo.ops[op.ID]
		stored.ExpiresAt = past
		stored.State, stored.Targets[0].State = "active", "active"
		repo.ops[op.ID] = stored
		repo.mu.Unlock()
		got, err := s.Get(context.Background(), "owner", op.ID)
		if err != nil || got.State != "expired" || got.Targets[0].State != "expired" {
			t.Fatalf("tracked=%v: operation %q target %q, %v", tracked, got.State, got.Targets[0].State, err)
		}
	}
}

func TestDeliveryRequiresReceiptAndPreservesPartialOutcome(t *testing.T) {
	s, r, p := fixture(t)
	op, err := s.Create(context.Background(), "owner", r)
	if err != nil {
		t.Fatal(err)
	}
	wire := s.operations[op.ID].op.Targets[0].WireID
	attributes := pcommon.NewMap()
	for key, value := range map[string]string{"capture.session.id": wire, "capture.point": "egress", "capture.pipeline": "traces", "capture.collector.pod": "pod", "capture.cluster": "cluster"} {
		attributes.PutStr(key, value)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+strings.Repeat("d", 64)))
	result := make(chan struct {
		accepted int64
		err      error
	}, 1)
	go func() {
		accepted, err := s.deliver(ctx, "traces", []pcommon.Map{attributes}, 2, []byte("bounded-synthetic"))
		result <- struct {
			accepted int64
			err      error
		}{accepted, err}
	}()
	var envelope *Envelope
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		envelope, err = s.Next("owner", r.ChannelID)
		if err != nil {
			t.Fatal(err)
		}
		if envelope != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if envelope == nil {
		t.Fatal("no delivery offered")
	}
	select {
	case <-result:
		t.Fatal("OTLP acknowledged before consumer receipt")
	default:
	}
	ack := Acknowledgement{ID: envelope.ID, Accepted: 1}
	if err := s.Ack("other", r.ChannelID, ack); !errors.Is(err, ErrForbidden) {
		t.Fatal("cross-owner receipt accepted")
	}
	if err := s.Ack("owner", r.ChannelID, ack); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack("owner", r.ChannelID, ack); err != nil {
		t.Fatal("identical receipt not reconciled")
	}
	out := <-result
	if out.err != nil || out.accepted != 1 {
		t.Fatal("partial acceptance lost")
	}
	m := s.Metrics()
	if m.Counters["consumer_accepted_traces_records_total"] != 1 || m.Counters["consumer_rejected_traces_records_total"] != 1 || m.Counters["receipt_retries_total"] != 1 || m.Gauges["pending_bytes"] != 0 {
		t.Fatal("partial/duplicate receipt metrics lost or double counted")
	}
	s.mu.Lock()
	s.peers[p.info.ID].info.Generation = 2
	s.mu.Unlock()
	if _, err := s.deliver(ctx, "traces", []pcommon.Map{attributes}, 1, []byte("x")); status.Code(err) != codes.PermissionDenied {
		t.Fatal("old-generation samples accepted")
	}
}

func TestStoppingTargetDrainsUntilReleased(t *testing.T) {
	s, r, _ := fixture(t)
	op, err := s.Create(context.Background(), "owner", r)
	if err != nil {
		t.Fatal(err)
	}
	attributes := pcommon.NewMap()
	for key, value := range map[string]string{"capture.session.id": s.operations[op.ID].op.Targets[0].WireID, "capture.point": "egress", "capture.pipeline": "traces", "capture.collector.pod": "pod", "capture.cluster": "cluster"} {
		attributes.PutStr(key, value)
	}
	bind := func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _, err := s.bindingLocked("credential", "traces", []pcommon.Map{attributes})
		return err
	}
	if err := bind(); err != nil {
		t.Fatal(err)
	}
	if stopped, err := s.Stop(context.Background(), "owner", op.ID, op.Revision); err != nil || stopped.State != "stopping" {
		t.Fatalf("%+v %v", stopped, err)
	}
	if err := bind(); err != nil {
		t.Fatalf("copies queued before the stop refused: %v", err)
	}
	s.mu.Lock()
	s.operations[op.ID].op.Targets[0].Released = true
	s.mu.Unlock()
	if err := bind(); !errors.Is(err, ErrForbidden) {
		t.Fatal("released target still accepted copies")
	}
}

func TestMissingConsumerReceiptStopsAndExposesUncertainty(t *testing.T) {
	s, r, _ := fixture(t)
	op, err := s.Create(context.Background(), "owner", r)
	if err != nil {
		t.Fatal(err)
	}
	wire := s.operations[op.ID].op.Targets[0].WireID
	attributes := pcommon.NewMap()
	for key, value := range map[string]string{"capture.session.id": wire, "capture.point": "egress", "capture.pipeline": "traces", "capture.collector.pod": "pod", "capture.cluster": "cluster"} {
		attributes.PutStr(key, value)
	}
	ctx, cancel := context.WithTimeout(metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+strings.Repeat("d", 64))), 20*time.Millisecond)
	defer cancel()
	if _, err := s.deliver(ctx, "traces", []pcommon.Map{attributes}, 1, []byte("x")); status.Code(err) != codes.Unavailable {
		t.Fatal("uncertain receipt falsely accepted")
	}
	stored, err := s.Get(context.Background(), "owner", op.ID)
	if err != nil || stored.State != "stopping" || stored.Reason != "receipt_uncertain" {
		t.Fatal("loss not visible in execution metadata", stored.State, stored.Reason)
	}
}

func TestPluginHTTPAuthorizesOwnerAndEpoch(t *testing.T) {
	s, r, _ := fixture(t)
	call := func(role, owner, epoch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", pluginsdk.APIPath+"v1/captures", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+s.config.serviceToken)
		req.Header.Set(pluginsdk.CallerIDHeader, owner)
		req.Header.Set(pluginsdk.CallerRoleHeader, role)
		req.Header.Set(pluginsdk.InstanceHeader, s.instance)
		req.Header.Set(pluginsdk.EpochHeader, epoch)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if call("reader", "owner", s.epoch).Code != 403 || call("admin", "owner", strings.Repeat("e", 32)).Code != 409 {
		t.Fatal("role/epoch not enforced")
	}
	if _, err := s.Create(context.Background(), "owner", r); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(r)
	req := httptest.NewRequest(http.MethodPost, pluginsdk.APIPath+"v1/captures", strings.NewReader(string(encoded[:len(encoded)-1])+`,"authorized_raw":true}`))
	req.Header.Set("Authorization", "Bearer "+s.config.serviceToken)
	req.Header.Set(pluginsdk.CallerIDHeader, "owner")
	req.Header.Set(pluginsdk.CallerRoleHeader, "admin")
	req.Header.Set(pluginsdk.InstanceHeader, s.instance)
	req.Header.Set(pluginsdk.EpochHeader, s.epoch)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("caller supplied raw authorization")
	}
}

func TestDeliveryCapacityWithdrawsCapture(t *testing.T) {
	s, r, _ := fixture(t)
	op, err := s.Create(context.Background(), "owner", r)
	if err != nil {
		t.Fatal(err)
	}
	attributes := pcommon.NewMap()
	for key, value := range map[string]string{"capture.session.id": s.operations[op.ID].op.Targets[0].WireID, "capture.point": "egress", "capture.pipeline": "traces", "capture.collector.pod": "pod", "capture.cluster": "cluster"} {
		attributes.PutStr(key, value)
	}
	s.channels[r.ChannelID].bytes = 1 << 20
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+strings.Repeat("d", 64)))
	if _, err := s.deliver(ctx, "traces", []pcommon.Map{attributes}, 1, []byte("x")); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("bounded channel falsely accepted data")
	}
	stored, err := s.Get(context.Background(), "owner", op.ID)
	if err != nil || stored.State != "stopping" || stored.Reason != "delivery_capacity" {
		t.Fatal("capacity withdrawal not visible", stored.State, stored.Reason)
	}
}

func TestLateAuthorityContextCannotAdmit(t *testing.T) {
	s, r, _ := fixture(t)
	ctx := context.WithValue(context.Background(), authorityContextKey{}, pluginsdk.RandomID())
	if _, err := s.Create(ctx, "owner", r); !errors.Is(err, ErrConflict) {
		t.Fatal("stale host epoch admitted capture")
	}
	if _, err := s.NewChannel(ctx, "owner", time.Now().Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatal("stale host epoch opened a channel")
	}
	if len(s.operations) != 0 {
		t.Fatal("stale authority dispatched work")
	}
}

// TestCaptureRecordsActingForAndNamesBusyCollector: the acting-for headers
// fleet forwards are recorded on the operation, malformed ones are refused,
// and a second capture on a busy Collector gets the collector_busy code.
func TestCaptureRecordsActingForAndNamesBusyCollector(t *testing.T) {
	s, r, _ := fixture(t)
	post := func(request CreateRequest, acting http.Header) *httptest.ResponseRecorder {
		encoded, _ := json.Marshal(request)
		req := httptest.NewRequest(http.MethodPost, pluginsdk.APIPath+"v1/captures", strings.NewReader(string(encoded)))
		for k, v := range acting {
			req.Header[k] = v
		}
		req.Header.Set("Authorization", "Bearer "+s.config.serviceToken)
		req.Header.Set(pluginsdk.CallerIDHeader, "owner")
		req.Header.Set(pluginsdk.CallerRoleHeader, "admin")
		req.Header.Set(pluginsdk.InstanceHeader, s.instance)
		req.Header.Set(pluginsdk.EpochHeader, s.epoch)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := post(r, http.Header{pluginsdk.ActingNameHeader: {"no principal"}}); w.Code != 400 {
		t.Fatalf("malformed acting-for = %d", w.Code)
	}
	acting := pluginsdk.Acting{ID: "user:alice", Name: "Alice", RequestID: "req-1"}
	headers := http.Header{}
	acting.Set(headers)
	w := post(r, headers)
	var op Operation
	if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &op) != nil || op.ActingFor == nil || *op.ActingFor != acting {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	stored, err := s.repo.Get(context.Background(), r.ID)
	if err != nil || stored.ActingFor == nil || *stored.ActingFor != acting {
		t.Fatalf("stored acting-for = %+v, %v", stored.ActingFor, err)
	}

	second := r
	second.ID = pluginsdk.RandomID()
	w = post(second, nil)
	var refusal struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Code != "collector_busy" {
		t.Fatalf("busy collector = %d %s", w.Code, w.Body)
	}
}
