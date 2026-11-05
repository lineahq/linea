package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

func TestCaptureLifecycleFollowsFleetAndWaitDoesNotStop(t *testing.T) {
	f := newCaptureFixture(t)
	ctx := context.Background()
	v, err := f.s.ValidateCaptureRequest(ctx, testCaller, f.req)
	if err != nil || v.Selection.Limits.TotalRecords != 10000 || len(v.Collectors) != 1 || v.Collectors[0].ID != f.fleet.peers[0].ID {
		t.Fatalf("%+v %v", v, err)
	}
	session := f.start(t)
	id := session.ID
	req, ch := f.fleet.request(id)
	peer := f.fleet.peers[0]
	if len(req.Targets) != 1 || req.Targets[0].PeerID != peer.ID || req.Targets[0].Generation != peer.Generation ||
		!req.ExpiresAt.Equal(session.ExpiresAt) || !ch.expiresAt.Equal(session.ExpiresAt.Add(channelGrace)) ||
		req.Limits.TotalRecords != 10000 || req.CapturePoints[0] != (fleetcapture.Point{Pipeline: "logs", Point: "egress"}) {
		t.Fatalf("fleet request %+v", req)
	}
	if id != "capture-"+req.ID || session.State != "starting" || session.Terminal || session.Targets[0].State != "pending" ||
		session.Targets[0].Source == nil || session.Targets[0].Source.Pod != "collector-0" {
		t.Fatalf("admitted %+v", session)
	}
	session.Selection.CapturePoints[0].Point = "mutated"
	f.fleet.set(id, "active", "active", &fleetcapture.TargetStats{EnqueuedRecords: 3, Dropped: map[string]int64{"queue_full": 2}})
	current := f.eventually(t, id, func(v *CaptureSession) bool { return v.State == "active" && v.Targets[0].Stats != nil })
	if current.Targets[0].State != "active" || current.Selection.CapturePoints[0].Point != "egress" {
		t.Fatalf("%+v", current)
	}
	current.Targets[0].Stats.Dropped["queue_full"] = 999
	current, _ = f.s.GetCaptureSession(ctx, testCaller, id)
	if current.Targets[0].Stats.Dropped["queue_full"] != 2 {
		t.Fatal("stats alias")
	}
	wait, cancel := context.WithCancel(ctx)
	cancel()
	_, err = f.s.WaitCaptureSession(wait, testCaller, id)
	wantCode(t, err, CodeCancelled)
	if current, _ = f.s.GetCaptureSession(ctx, testCaller, id); current.Terminal {
		t.Fatal("cancelled wait stopped the capture")
	}
	stopped, err := f.s.StopCaptureSession(ctx, testCaller, id)
	if err != nil || stopped.State != "stopping" || stopped.Terminal || stopped.StopReason != "stop_requested" || f.fleet.stops != 1 {
		t.Fatalf("%+v %v", stopped, err)
	}
	if again, err := f.s.StopCaptureSession(ctx, testCaller, id); err != nil || again.State != "stopping" || f.fleet.stops != 1 {
		t.Fatalf("repeated stop %+v %v", again, err)
	}
	f.fleet.set(id, "stopped", "completed", nil)
	current, err = f.s.WaitCaptureSession(ctx, testCaller, id)
	if err != nil || !current.Terminal || current.State != "stopped" || !current.Targets[0].Terminal {
		t.Fatalf("%+v %v", current, err)
	}
	f.eventually(t, id, func(v *CaptureSession) bool { return v.Data.State == "frozen" })
	if !f.fleet.channelDeleted(id) {
		t.Fatal("finished capture kept its channel")
	}
	if err := f.s.DeleteCaptureSession(ctx, testCaller, id); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.GetCaptureSession(ctx, testCaller, id)
	wantCode(t, err, CodeNotFound)
	f.start(t)
}

func TestCaptureSelectionModesResolveAsFleetDoes(t *testing.T) {
	traces := CapturePoint{Pipeline: "traces", Point: "egress", Signal: "traces", Sensitivity: "redacted"}
	f := newFleetFixture(t, nil, fakePeer(0, logsEgress), fakePeer(1, traces), fakePeer(2, logsEgress))
	ctx := context.Background()
	peers := f.fleet.peers
	f.fleet.members["checkout"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}, {ID: peers[1].AgentID, Connected: true}, {ID: peers[2].AgentID}}
	f.fleet.members["env=prod"] = []fleetcapture.Agent{{ID: peers[2].AgentID, Connected: true}}
	base := cloneCapture(f.req)
	base.CollectorIDs = nil
	for name, tc := range map[string]struct {
		change func(*CaptureSelection)
		want   []string
		code   Code
	}{
		"agents":        {change: func(r *CaptureSelection) { r.AgentIDs = []string{peers[2].AgentID} }, want: []string{peers[2].ID}},
		"agent cannot":  {change: func(r *CaptureSelection) { r.AgentIDs = []string{peers[1].AgentID} }, code: CodeInvalidRequest},
		"group capable": {change: func(r *CaptureSelection) { r.GroupID = "checkout" }, want: []string{peers[0].ID}},
		"selector":      {change: func(r *CaptureSelection) { r.Selector = "env=prod" }, want: []string{peers[2].ID}},
		"group nobody":  {change: func(r *CaptureSelection) { r.GroupID = "checkout"; r.Signals = []string{"metrics"} }, code: CodeConflict},
		"unknown group": {change: func(r *CaptureSelection) { r.GroupID = "missing" }, code: CodeNotFound},
		"two modes":     {change: func(r *CaptureSelection) { r.GroupID = "checkout"; r.Selector = "env=prod" }, code: CodeInvalidRequest},
		"no mode":       {change: func(*CaptureSelection) {}, code: CodeInvalidRequest},
		"unknown peer":  {change: func(r *CaptureSelection) { r.CollectorIDs = []string{strings.Repeat("f", 64)} }, code: CodeNotFound},
		"invalid peer":  {change: func(r *CaptureSelection) { r.CollectorIDs = []string{"missing"} }, code: CodeInvalidRequest},
		"invalid agent": {change: func(r *CaptureSelection) { r.AgentIDs = []string{"agent"} }, code: CodeInvalidRequest},
		"absent signal": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[0].ID}
			r.Signals = []string{"logs", "traces"}
		}, code: CodeInvalidRequest},
		"too long":       {change: func(r *CaptureSelection) { r.CollectorIDs = []string{peers[0].ID}; r.DurationSeconds = 281 }, code: CodeInvalidRequest},
		"negative limit": {change: func(r *CaptureSelection) { r.CollectorIDs = []string{peers[0].ID}; r.Limits.TotalBytes = -1 }, code: CodeInvalidRequest},
		"long filter": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[0].ID}
			r.Filter.Resource = []string{strings.Repeat("x", 4097)}
		}, code: CodeInvalidRequest},
		"many filters": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[0].ID}
			r.Filter.Resource = make([]string, 33)
			for i := range r.Filter.Resource {
				r.Filter.Resource[i] = "true"
			}
		}, code: CodeInvalidRequest},
		"sampling": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[0].ID}
			r.Sampling = &CaptureSampling{Rate: 2}
		}, code: CodeInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			req := cloneCapture(base)
			tc.change(&req)
			v, err := f.s.ValidateCaptureRequest(ctx, testCaller, req)
			if tc.code != "" {
				wantCode(t, err, tc.code)
				return
			}
			if err != nil || len(v.Collectors) != len(tc.want) || v.Collectors[0].ID != tc.want[0] {
				t.Fatalf("%+v %v", v, err)
			}
		})
	}
	req := cloneCapture(base)
	req.GroupID = "checkout"
	session, err := f.s.CreateCaptureSession(ctx, testCaller, req)
	if err != nil || len(session.Targets) != 1 || session.Targets[0].CollectorID != peers[0].ID {
		t.Fatalf("group admission %+v %v", session, err)
	}
	if sent, _ := f.fleet.request(session.ID); sent.GroupID != "checkout" || len(sent.Targets) != 0 {
		t.Fatalf("fleet resolves groups itself: %+v", sent)
	}
	f.fleet.setPeer(0, func(p *fleetcapture.Peer) { p.Identity.Enabled = false })
	f.advance(peerFreshness)
	_, err = f.s.ValidateCaptureRequest(ctx, testCaller, f.req)
	wantCode(t, err, CodeConflict)
	_, err = f.s.CreateCaptureSession(ctx, Caller{Principal: Principal{ID: "collector", Kind: PrincipalService}, Client: "collector"}, f.req)
	wantCode(t, err, CodeForbidden)
}

// A filter must fit the limits each target Collector reports, the version 1
// limits when it reports none; a group keeps the Collectors it fits and
// names the limit when it fits none.
func TestCaptureFilterFollowsCollectorLimits(t *testing.T) {
	large := fakePeer(1, logsEgress)
	large.Identity.FilterLimits = &fleetcapture.FilterLimits{MaxConditions: 64, MaxConditionChars: 1024}
	f := newFleetFixture(t, nil, fakePeer(0, logsEgress), large)
	ctx := context.Background()
	peers := f.fleet.peers
	f.fleet.members["both"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}, {ID: peers[1].AgentID, Connected: true}}
	f.fleet.members["v1"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}}
	conditions := make([]string, 17)
	for i := range conditions {
		conditions[i] = "true"
	}
	for name, tc := range map[string]struct {
		change  func(*CaptureSelection)
		want    string
		refusal string
	}{
		"v1 count":       {change: func(r *CaptureSelection) { r.CollectorIDs = []string{peers[0].ID}; r.Filter.Resource = conditions }, refusal: "17 conditions; the target Collector compiles at most 16"},
		"reported count": {change: func(r *CaptureSelection) { r.CollectorIDs = []string{peers[1].ID}; r.Filter.Resource = conditions }, want: peers[1].ID},
		"v1 length": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[0].ID}
			r.Filter.Resource = []string{strings.Repeat("é", 257)}
		}, refusal: "257 characters; the target Collector compiles at most 256"},
		"reported length": {change: func(r *CaptureSelection) {
			r.CollectorIDs = []string{peers[1].ID}
			r.Filter.Resource = []string{strings.Repeat("é", 257)}
		}, want: peers[1].ID},
		"group keeps fit": {change: func(r *CaptureSelection) { r.CollectorIDs = nil; r.GroupID = "both"; r.Filter.Resource = conditions }, want: peers[1].ID},
		"group fits none": {change: func(r *CaptureSelection) { r.CollectorIDs = nil; r.GroupID = "v1"; r.Filter.Resource = conditions }, refusal: "at most 16"},
	} {
		t.Run(name, func(t *testing.T) {
			req := cloneCapture(f.req)
			tc.change(&req)
			v, err := f.s.ValidateCaptureRequest(ctx, testCaller, req)
			if tc.refusal != "" {
				wantCode(t, err, CodeInvalidRequest)
				if !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("refusal = %v, want %q", err, tc.refusal)
				}
				return
			}
			if err != nil || len(v.Collectors) != 1 || v.Collectors[0].ID != tc.want {
				t.Fatalf("%+v %v", v, err)
			}
		})
	}
}

func TestCaptureFleetRefusalsReleaseTheChannel(t *testing.T) {
	f := newCaptureFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		err  error
		code Code
	}{
		{refusal(403, "raw points need permission"), CodeForbidden},
		{refusal(409, "target is busy"), CodeConflict},
		{refusal(422, "too many targets"), CodeInvalidRequest},
		{refusal(429, "too many captures"), CodeResourceExhausted},
		{refusal(401, "token secret-value rejected"), CodeUnavailable},
	} {
		f.fleet.fail("CreateCapture", tc.err)
		_, err := f.s.CreateCaptureSession(ctx, testCaller, f.req)
		wantCode(t, err, tc.code)
		if tc.code == CodeForbidden && !strings.Contains(err.Error(), "fleet: raw points need permission") {
			t.Fatalf("fleet reason lost: %v", err)
		}
		if strings.Contains(err.Error(), "secret-value") {
			t.Fatal("credential refusal echoed fleet's message")
		}
	}
	f.fleet.mu.Lock()
	for id, ch := range f.fleet.channels {
		if !ch.deleted {
			t.Errorf("refused admission kept channel %s", id)
		}
	}
	f.fleet.mu.Unlock()
	f.fleet.fail("CreateChannel", errors.New("connection refused"))
	_, err := f.s.CreateCaptureSession(ctx, testCaller, f.req)
	wantCode(t, err, CodeUnavailable)
	// A transport failure is retried once with the same capture ID.
	f.fleet.fail("CreateCapture", errors.New("connection reset"))
	session := f.start(t)
	if len(f.fleet.requests) != 1 || session.State != "starting" {
		t.Fatalf("retried admission %+v", f.fleet.requests)
	}
	page, err := f.s.ListCaptureSessions(ctx, testCaller, CollectorPageRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("refusals left sessions: %+v %v", page, err)
	}
}

func TestCaptureWithoutFleet(t *testing.T) {
	ctx := context.Background()
	s := New(Options{})
	t.Cleanup(s.Close)
	page, err := s.ListCollectors(ctx, testCaller, CollectorPageRequest{})
	if err != nil || page.Enabled || len(page.Items) != 0 {
		t.Fatalf("%+v %v", page, err)
	}
	_, err = s.ValidateCaptureRequest(ctx, testCaller, CaptureSelection{})
	wantCode(t, err, CodeUnavailable)
	_, err = s.ListCollectorGroups(ctx, testCaller, CollectorPageRequest{})
	wantCode(t, err, CodeUnavailable)
	if (CaptureOptions{Fleet: FleetOptions{Token: "secret"}}).Validate() == nil {
		t.Fatal("token without endpoint accepted")
	}
	invalid := CaptureOptions{Fleet: FleetOptions{Endpoint: "http://fleet.example:8080", Token: "secret"}}
	if invalid.Validate() == nil {
		t.Fatal("plaintext to a remote fleet accepted")
	}
	s = New(Options{Capture: invalid})
	t.Cleanup(s.Close)
	_, err = s.ListCollectors(ctx, testCaller, CollectorPageRequest{})
	wantCode(t, err, CodeInternal)
}

func TestCaptureEndedWithUnfinishedTargetReportsUnknown(t *testing.T) {
	f := newCaptureFixture(t)
	id := f.start(t).ID
	f.fleet.set(id, "interrupted", "active", nil)
	got := f.eventually(t, id, func(v *CaptureSession) bool { return v.Terminal })
	if got.State != "interrupted" || got.Targets[0].State != "unknown" || !got.Targets[0].Terminal {
		t.Fatalf("%+v", got.Targets)
	}
	page, err := f.s.ListCaptureSessions(context.Background(), testCaller, CollectorPageRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ActiveCount != 0 || page.Items[0].UncertainCount != 1 {
		t.Fatalf("%+v %v", page, err)
	}
}

func TestCaptureLostByFleetBecomesUncertain(t *testing.T) {
	for name, tc := range map[string]struct {
		lose   func(f *captureFixture, id string)
		reason string
	}{
		"operation gone": {func(f *captureFixture, id string) {
			f.fleet.mu.Lock()
			delete(f.fleet.ops, strings.TrimPrefix(id, "capture-"))
			f.fleet.mu.Unlock()
		}, "fleet_no_longer_reports_capture"},
		"channel gone":   {func(f *captureFixture, id string) { f.fleet.fail("Next", refusal(410, "plugin restarted")) }, "capture_channel_lost"},
		"never terminal": {func(f *captureFixture, id string) { f.advance(61*time.Second + captureGuard) }, "no_terminal_state_from_fleet_after_expiry"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCaptureFixture(t)
			v := dispatchData(t, f)
			f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.State == "active" })
			tc.lose(f, v.ID)
			got := f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.Terminal })
			if got.State != "uncertain" || got.Reason != tc.reason || got.Targets[0].State != "unknown" {
				t.Fatalf("%+v", got)
			}
			f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.Data.State == "frozen" })
			if !f.fleet.channelDeleted(v.ID) && name != "channel gone" {
				t.Fatal("lost capture kept its channel")
			}
		})
	}
}

func TestCaptureMemoryAndHistoryCapacity(t *testing.T) {
	// Each open capture holds its own Collector, as a Collector serves one
	// capture at a time.
	peers := []fleetcapture.Peer{}
	for i := range captureMemoryBytes/captureSessionBytes + 1 {
		peers = append(peers, fakePeer(int(i), logsEgress))
	}
	f := newFleetFixture(t, nil, peers...)
	f.s.collectors.history = 12
	ctx := context.Background()
	open := []string{}
	for _, p := range peers[1:] {
		f.req.CollectorIDs = []string{p.ID}
		open = append(open, f.start(t).ID)
	}
	f.req.CollectorIDs = []string{peers[0].ID}
	_, err := f.s.CreateCaptureSession(ctx, testCaller, f.req)
	wantCode(t, err, CodeResourceExhausted)
	for _, id := range open {
		f.finish(t, id, "completed")
	}
	f.fleet.finishAs = "completed"
	for range 12 - len(open) {
		v := f.start(t)
		f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.Data.State == "frozen" })
	}
	_, err = f.s.CreateCaptureSession(ctx, testCaller, f.req)
	wantCode(t, err, CodeResourceExhausted)
	page, err := f.s.ListCaptureSessions(ctx, testCaller, CollectorPageRequest{Limit: 100})
	if err != nil || len(page.Items) != 12 {
		t.Fatalf("%d %v", len(page.Items), err)
	}
	// Nothing expires: only deleting a capture frees history.
	f.advance(24*time.Hour + time.Second)
	_, err = f.s.CreateCaptureSession(ctx, testCaller, f.req)
	wantCode(t, err, CodeResourceExhausted)
	if err := f.s.DeleteCaptureSession(ctx, testCaller, open[0]); err != nil {
		t.Fatal(err)
	}
	f.start(t)
}

func TestCaptureShutdownStopsFleetCaptureAndReleasesWaiters(t *testing.T) {
	f := newCaptureFixture(t)
	ctx := context.Background()
	session := dispatchData(t, f)
	f.eventually(t, session.ID, func(v *CaptureSession) bool { return v.State == "active" })
	completed := make(chan error, 1)
	go func() { _, err := f.s.WaitCaptureSession(ctx, testCaller, session.ID); completed <- err }()
	f.s.Close()
	select {
	case err := <-completed:
		wantCode(t, err, CodeUnavailable)
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked capture waiter")
	}
	if op := f.fleet.op(session.ID); op.State != "stopping" || !f.fleet.channelDeleted(session.ID) {
		t.Fatalf("shutdown left fleet capture %s", op.State)
	}
	_, err := f.s.CreateCaptureSession(ctx, testCaller, f.req)
	wantCode(t, err, CodeUnavailable)
}

func TestCaptureSummaryPageDoesNotCarryVerboseSelection(t *testing.T) {
	f := newCaptureFixture(t, Budgets{PageBytes: 2048})
	ctx := context.Background()
	for i := 0; i < 16; i++ {
		f.req.Filter.Resource = append(f.req.Filter.Resource, strings.Repeat("x", 250))
	}
	session := f.start(t)
	full, _ := json.Marshal(session)
	if len(full) < 2048 {
		t.Fatal("fixture must exceed page budget")
	}
	page, err := f.s.ListCaptureSessions(ctx, testCaller, CollectorPageRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].TargetCount != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	raw, _ := json.Marshal(page)
	if len(raw) > 2048 || strings.Contains(string(raw), "filter") {
		t.Fatal("list leaked full selection or exceeded budget")
	}
	read, err := f.s.GetCaptureSession(ctx, testCaller, session.ID)
	if err != nil || len(read.Selection.Filter.Resource) != 16 {
		t.Fatal("full status lost selection")
	}
}
