package platform

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// fakeFleet is an in-memory livecapture plugin and fleet core. It admits,
// stops and delivers as the plugin does, without the Collectors: tests drive
// target states and batches.
type fakeFleet struct {
	mu       sync.Mutex
	peers    []fleetcapture.Peer
	groups   []fleetcapture.Group
	members  map[string][]fleetcapture.Agent // by group ID or selector
	acting   fleetcapture.Acting             // of the last CreateCapture
	channels map[string]*fakeChannel
	ops      map[string]*fleetcapture.Operation
	requests []fleetcapture.CaptureRequest
	stops    int
	failures map[string][]error // queued per method
	finishAs string             // admits captures already in this state
	ids      int
	configs  fakeConfigs
}

type fakeChannel struct {
	expiresAt time.Time
	queue     []fleetcapture.Envelope
	acks      map[string]int64
	deleted   bool
}

func newFakeFleet(peers ...fleetcapture.Peer) *fakeFleet {
	return &fakeFleet{peers: peers, members: map[string][]fleetcapture.Agent{}, channels: map[string]*fakeChannel{}, ops: map[string]*fleetcapture.Operation{}, failures: map[string][]error{}}
}

func fakePeer(i int, points ...CapturePoint) fleetcapture.Peer {
	p := fleetcapture.Peer{
		AuthorityID: strings.Repeat("a", 32), PluginInstance: strings.Repeat("b", 32),
		ID: fmt.Sprintf("%064x", i+1), Generation: 1, CredentialID: fmt.Sprintf("credential-%d", i), AgentID: fmt.Sprintf("%032x", i+1),
		Identity: fleetcapture.Identity{Cluster: "lab", Namespace: "otel", Pod: fmt.Sprintf("collector-%d", i), CollectorGroup: "gateway", CollectorVersion: "0.150.0", Enabled: true},
	}
	for _, point := range points {
		p.Identity.CapturePoints = append(p.Identity.CapturePoints, fleetcapture.CapturePoint(point))
	}
	return p
}

var logsEgress = CapturePoint{Pipeline: "logs", Point: "egress", Signal: "logs", Sensitivity: "redacted"}

func refusal(status int, message string) error {
	return &fleetcapture.Error{Status: status, Code: "refused", Message: message}
}

// fail queues errors the next calls of a method return.
func (f *fakeFleet) fail(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method] = append(f.failures[method], errs...)
}

func (f *fakeFleet) failure(method string) error {
	if q := f.failures[method]; len(q) > 0 {
		f.failures[method] = q[1:]
		return q[0]
	}
	return nil
}

func (f *fakeFleet) newID() string {
	f.ids++
	return fmt.Sprintf("%032x", 1<<40+f.ids)
}

func (f *fakeFleet) Peers(_ context.Context, cursor string, limit int) (*fleetcapture.PeerPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("Peers"); err != nil {
		return nil, err
	}
	start, _ := strconv.Atoi(cursor)
	end := min(start+limit, len(f.peers))
	page := &fleetcapture.PeerPage{Items: slices.Clone(f.peers[start:end])}
	if end < len(f.peers) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeFleet) CreateChannel(_ context.Context, expiresAt time.Time) (*fleetcapture.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("CreateChannel"); err != nil {
		return nil, err
	}
	id := f.newID()
	f.channels[id] = &fakeChannel{expiresAt: expiresAt, acks: map[string]int64{}}
	return &fleetcapture.Channel{AuthorityID: strings.Repeat("a", 32), PluginInstance: strings.Repeat("b", 32), ID: id, ExpiresAt: expiresAt, HeartbeatSeconds: 5}, nil
}

func (f *fakeFleet) channel(id string) (*fakeChannel, error) {
	ch := f.channels[id]
	if ch == nil || ch.deleted {
		return nil, refusal(404, "channel not found")
	}
	return ch, nil
}

func (f *fakeFleet) DeleteChannel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, err := f.channel(id)
	if err == nil {
		ch.deleted = true
	}
	return err
}

func (f *fakeFleet) Next(_ context.Context, id string) (*fleetcapture.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("Next"); err != nil {
		return nil, err
	}
	ch, err := f.channel(id)
	if err != nil || len(ch.queue) == 0 {
		return nil, err
	}
	e := ch.queue[0]
	ch.queue = ch.queue[1:]
	return &e, nil
}

func (f *fakeFleet) Acknowledge(_ context.Context, id string, ack fleetcapture.Ack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, err := f.channel(id)
	if err == nil {
		ch.acks[ack.BatchID] = ack.Accepted
	}
	return err
}

func (f *fakeFleet) CreateCapture(ctx context.Context, req fleetcapture.CaptureRequest) (*fleetcapture.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acting, _ = fleetcapture.ActingFrom(ctx)
	if err := f.failure("CreateCapture"); err != nil {
		return nil, err
	}
	if op := f.ops[req.ID]; op != nil {
		return cloneCapture(op), nil
	}
	if _, err := f.channel(req.ChannelID); err != nil {
		return nil, refusal(409, "channel not found")
	}
	var chosen []fleetcapture.Peer
	switch {
	case len(req.Targets) > 0:
		for _, t := range req.Targets {
			i := slices.IndexFunc(f.peers, func(p fleetcapture.Peer) bool { return p.ID == t.PeerID && p.Generation == t.Generation })
			if i < 0 {
				return nil, refusal(409, "target generation is not connected")
			}
			chosen = append(chosen, f.peers[i])
		}
	case len(req.Agents) > 0:
		for _, agent := range req.Agents {
			i := slices.IndexFunc(f.peers, func(p fleetcapture.Peer) bool { return p.AgentID == agent })
			if i < 0 {
				return nil, refusal(409, "agent has no peer")
			}
			chosen = append(chosen, f.peers[i])
		}
	default:
		key := req.GroupID + req.Selector
		for _, a := range f.members[key] {
			for _, p := range f.peers {
				if a.Connected && p.AgentID == a.ID && fakeCapable(p, req) {
					chosen = append(chosen, p)
				}
			}
		}
		if len(chosen) == 0 {
			return nil, refusal(409, "no capable peer")
		}
	}
	f.requests = append(f.requests, req)
	op := &fleetcapture.Operation{AuthorityID: req.AuthorityID, ID: req.ID, State: "starting", Revision: 1, ExpiresAt: req.ExpiresAt}
	for _, p := range chosen {
		op.Targets = append(op.Targets, fleetcapture.TargetOutcome{PeerID: p.ID, Generation: p.Generation, AgentID: p.AgentID, State: "pending"})
	}
	if f.finishAs != "" {
		setFakeState(op, f.finishAs, "completed")
	}
	f.ops[req.ID] = op
	return cloneCapture(op), nil
}

func (f *fakeFleet) GetCapture(_ context.Context, id string) (*fleetcapture.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("GetCapture"); err != nil {
		return nil, err
	}
	op := f.ops[id]
	if op == nil {
		return nil, refusal(404, "capture not found")
	}
	return cloneCapture(op), nil
}

func (f *fakeFleet) StopCapture(_ context.Context, id string, revision int64) (*fleetcapture.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("StopCapture"); err != nil {
		return nil, err
	}
	op := f.ops[id]
	switch {
	case op == nil:
		return nil, refusal(404, "capture not found")
	case op.Revision != revision:
		return nil, refusal(412, "revision changed")
	case op.Terminal() || op.State == "stopping":
		return cloneCapture(op), nil
	}
	f.stops++
	setFakeState(op, "stopping", "stopping")
	return cloneCapture(op), nil
}

func (f *fakeFleet) Groups(_ context.Context, after string, limit int) (*fleetcapture.GroupPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start, _ := strconv.Atoi(after)
	end := min(start+limit, len(f.groups))
	page := &fleetcapture.GroupPage{Items: slices.Clone(f.groups[start:end])}
	if end < len(f.groups) {
		page.NextAfter = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeFleet) Group(_ context.Context, id string) (*fleetcapture.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, g := range f.groups {
		if g.ID == id {
			return &g, nil
		}
	}
	return nil, refusal(404, "group not found")
}

func (f *fakeFleet) agents(key, after string, limit int) (*fleetcapture.AgentPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, ok := f.members[key]
	if !ok {
		return nil, refusal(404, "group not found")
	}
	start, _ := strconv.Atoi(after)
	end := min(start+limit, len(all))
	page := &fleetcapture.AgentPage{Items: slices.Clone(all[start:end])}
	if end < len(all) {
		page.NextAfter = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeFleet) GroupAgents(_ context.Context, id, after string, limit int) (*fleetcapture.AgentPage, error) {
	return f.agents(id, after, limit)
}

func (f *fakeFleet) Agents(_ context.Context, selector, after string, limit int) (*fleetcapture.AgentPage, error) {
	return f.agents(selector, after, limit)
}

// fakeCapable keeps the peers a group or selector capture can use, as fleet
// does: enabled and offering every selected point and signal.
func fakeCapable(p fleetcapture.Peer, req fleetcapture.CaptureRequest) bool {
	covered := map[string]bool{}
	for _, point := range req.CapturePoints {
		found := false
		for _, known := range p.Identity.CapturePoints {
			if known.Pipeline == point.Pipeline && known.Point == point.Point && slices.Contains(req.Signals, known.Signal) {
				found, covered[known.Signal] = true, true
			}
		}
		if !found {
			return false
		}
	}
	return p.Identity.Enabled && len(covered) == len(req.Signals)
}

func setFakeState(op *fleetcapture.Operation, state, target string) {
	op.State = state
	op.Revision++
	for i := range op.Targets {
		op.Targets[i].State = target
	}
}

// set changes an admitted capture as fleet would report it.
func (f *fakeFleet) set(session, state, target string, stats *fleetcapture.TargetStats) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := f.ops[strings.TrimPrefix(session, "capture-")]
	setFakeState(op, state, target)
	for i := range op.Targets {
		op.Targets[i].Stats = stats
	}
}

func (f *fakeFleet) op(session string) fleetcapture.Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *cloneCapture(f.ops[strings.TrimPrefix(session, "capture-")])
}

func (f *fakeFleet) request(session string) (fleetcapture.CaptureRequest, *fakeChannel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if "capture-"+req.ID == session {
			return req, f.channels[req.ChannelID]
		}
	}
	panic("no capture request for " + session)
}

func (f *fakeFleet) setPeer(i int, change func(*fleetcapture.Peer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.peers[i])
}

// enqueue queues a batch for a capture's channel and returns its ID.
func (f *fakeFleet) enqueue(session string, e fleetcapture.Envelope) string {
	req, ch := f.request(session)
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.BatchID == "" {
		e.BatchID = f.newID()
	}
	if e.CaptureID == "" {
		e.CaptureID = req.ID
	}
	if e.Encoding == "" {
		e.Encoding = "otlp-protobuf"
	}
	if e.Signal == "" {
		e.Signal = "logs"
	}
	if e.Records == 0 {
		e.Records = 1
	}
	ch.queue = append(ch.queue, e)
	return e.BatchID
}

// acked waits for a batch's acknowledgement.
func (f *fakeFleet) acked(t *testing.T, session, batch string) int64 {
	t.Helper()
	_, ch := f.request(session)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		accepted, ok := ch.acks[batch]
		f.mu.Unlock()
		if ok {
			return accepted
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("batch %s was never acknowledged", batch)
	return 0
}

func (f *fakeFleet) channelDeleted(session string) bool {
	_, ch := f.request(session)
	f.mu.Lock()
	defer f.mu.Unlock()
	return ch.deleted
}

type captureFixture struct {
	s       *Service
	fleet   *fakeFleet
	now     time.Time
	clockMu sync.Mutex
	req     CaptureSelection
}

func (f *captureFixture) clock() time.Time {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	return f.now
}

func (f *captureFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

func (f *captureFixture) options() Options {
	return Options{Capture: CaptureOptions{fleet: f.fleet, Now: f.clock}}
}

func newFleetFixture(t *testing.T, opts func(*Options), peers ...fleetcapture.Peer) *captureFixture {
	t.Helper()
	f := &captureFixture{fleet: newFakeFleet(peers...), now: time.Now()}
	o := f.options()
	if opts != nil {
		opts(&o)
	}
	f.s = New(o)
	t.Cleanup(f.s.Close)
	f.req = CaptureSelection{CollectorIDs: []string{peers[0].ID}, CapturePoints: []CapturePointSelection{{Pipeline: "logs", Point: "egress"}}, Signals: []string{"logs"}, DurationSeconds: 60}
	return f
}

func newCaptureFixture(t *testing.T, budgets ...Budgets) *captureFixture {
	t.Helper()
	return newFleetFixture(t, func(o *Options) {
		if len(budgets) > 0 {
			o.Budgets = budgets[0]
		}
	}, fakePeer(0, logsEgress))
}

// dataFixture captures one Collector's logs/egress point, in a workspace
// when persistent, with raw sensitivity when raw.
func dataFixture(t *testing.T, persistent, raw bool) (*captureFixture, *memory.Data) {
	t.Helper()
	point := logsEgress
	if raw {
		point.Sensitivity = "raw"
	}
	f := &captureFixture{fleet: newFakeFleet(fakePeer(0, point)), now: time.Now()}
	var data *memory.Data
	if persistent {
		data = memory.NewData()
		var err error
		if f.s, err = openMemory(data, f.options()); err != nil {
			t.Fatal(err)
		}
	} else {
		f.s = New(f.options())
	}
	t.Cleanup(f.s.Close)
	f.req = CaptureSelection{CollectorIDs: []string{f.fleet.peers[0].ID}, CapturePoints: []CapturePointSelection{{Pipeline: "logs", Point: "egress"}}, Signals: []string{"logs"}, DurationSeconds: 60}
	return f, data
}

func (f *captureFixture) start(t *testing.T) *CaptureSession {
	t.Helper()
	v, err := f.s.CreateCaptureSession(context.Background(), testCaller, f.req)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// dispatchData starts a capture its target confirmed.
func dispatchData(t *testing.T, f *captureFixture) *CaptureSession {
	t.Helper()
	v := f.start(t)
	f.fleet.set(v.ID, "active", "active", nil)
	return v
}

// eventually polls a capture until cond holds.
func (f *captureFixture) eventually(t *testing.T, id string, cond func(*CaptureSession) bool) *CaptureSession {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := f.s.GetCaptureSession(context.Background(), testCaller, id)
		if err != nil {
			t.Fatal(err)
		}
		if cond(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("capture never reached the expected state: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// finish ends a capture in fleet and waits for its data to freeze.
func (f *captureFixture) finish(t *testing.T, id, state string) *CaptureSession {
	t.Helper()
	f.fleet.set(id, state, "completed", nil)
	return f.eventually(t, id, func(v *CaptureSession) bool {
		return v.Terminal && v.Data.State != "open" && v.Data.State != "draining"
	})
}

// deliver sends a batch from a peer and returns the records accepted.
func (f *captureFixture) deliver(t *testing.T, id string, peer int, raw []byte, records int64) int64 {
	t.Helper()
	p := f.fleet.peers[peer]
	batch := f.fleet.enqueue(id, fleetcapture.Envelope{PeerID: p.ID, Generation: p.Generation, Data: raw, Records: records})
	return f.fleet.acked(t, id, batch)
}

func captureLogBytes(t *testing.T, body string) []byte {
	t.Helper()
	data := plog.NewLogs()
	resource := data.ResourceLogs().AppendEmpty()
	resource.SetSchemaUrl("resource-schema")
	a := resource.Resource().Attributes()
	a.PutStr("capture.session.id", "wire-session")
	a.PutStr("capture.point", "egress")
	a.PutStr("capture.pipeline", "logs")
	a.PutInt("precise", 9223372036854775806)
	scope := resource.ScopeLogs().AppendEmpty()
	scope.Scope().SetName("scope")
	scope.Scope().SetVersion("v1")
	scope.SetSchemaUrl("scope-schema")
	log := scope.LogRecords().AppendEmpty()
	log.Body().SetStr(body)
	log.SetTimestamp(1791980000123456789)
	raw, err := telemetry.FromLogs(data).Proto()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ingestLog(t *testing.T, f *captureFixture, id, body string) {
	t.Helper()
	if accepted := f.deliver(t, id, 0, captureLogBytes(t, body), 1); accepted != 1 {
		t.Fatalf("accepted %d records", accepted)
	}
}
