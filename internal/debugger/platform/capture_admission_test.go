package platform

import (
	"context"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

var logsRaw = CapturePoint{Pipeline: "logs", Point: "egress", Signal: "logs", Sensitivity: "raw"}

// rawUsers is limitedUsers plus carol, an engineer who also holds
// raw-capture in the default project.
func rawUsers(t *testing.T) (*Policy, Caller, Caller, Caller) {
	t.Helper()
	scope := Scope{Kind: authz.ScopeProject, ID: artifact.DefaultProject}
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: scope},
		{Subject: Subject{Principal: "user:bob"}, Role: "engineer", Scope: scope},
		{Subject: Subject{Principal: "user:carol"}, Role: "engineer", Scope: scope},
		{Subject: Subject{Principal: "user:carol"}, Role: "raw-capture", Scope: scope},
	})
	if err != nil {
		t.Fatal(err)
	}
	user := func(id string) Caller {
		return Caller{Principal: Principal{ID: id, Kind: PrincipalUser, Name: strings.TrimPrefix(id, "user:")}, Client: "test", RequestID: "req-" + id}
	}
	return access, user("user:alice"), user("user:bob"), user("user:carol")
}

// TestRawCaptureNeedsCaptureRaw: an explicit selection of a raw point is
// refused without capture.raw and admitted with it, and fleet gets the
// acting user.
func TestRawCaptureNeedsCaptureRaw(t *testing.T) {
	access, alice, _, carol := rawUsers(t)
	f := newFleetFixture(t, func(o *Options) { o.Access = access }, fakePeer(0, logsRaw))
	ctx := context.Background()
	_, err := f.s.CreateCaptureSession(ctx, alice, f.req)
	wantCode(t, err, CodeForbidden)
	if !strings.Contains(err.Error(), "capture.raw") {
		t.Fatalf("refusal does not name the permission: %v", err)
	}
	if _, err := f.s.ValidateCaptureRequest(ctx, alice, f.req); err == nil {
		t.Fatal("validation admitted a raw selection without capture.raw")
	}
	if len(f.fleet.requests) != 0 {
		t.Fatalf("fleet saw a refused capture: %+v", f.fleet.requests)
	}
	if _, err := f.s.CreateCaptureSession(ctx, carol, f.req); err != nil {
		t.Fatalf("raw-capture holder = %v", err)
	}
	if want := (fleetcapture.Acting{ID: "user:carol", Name: "carol", RequestID: "req-user:carol"}); f.fleet.acting != want {
		t.Fatalf("acting for %+v, want %+v", f.fleet.acting, want)
	}
}

// TestGroupCaptureDropsRawCollectors: a group capture without capture.raw
// keeps the Collectors whose selected points are redacted and pins them as
// explicit targets, so fleet cannot choose a raw one.
func TestGroupCaptureDropsRawCollectors(t *testing.T) {
	access, alice, _, _ := rawUsers(t)
	f := newFleetFixture(t, func(o *Options) { o.Access = access }, fakePeer(0, logsRaw), fakePeer(1, logsEgress))
	peers := f.fleet.peers
	f.fleet.members["checkout"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}, {ID: peers[1].AgentID, Connected: true}}
	req := cloneCapture(f.req)
	req.CollectorIDs, req.GroupID = nil, "checkout"
	v, err := f.s.CreateCaptureSession(context.Background(), alice, req)
	if err != nil || len(v.Targets) != 1 || v.Targets[0].CollectorID != peers[1].ID {
		t.Fatalf("%+v %v", v, err)
	}
	sent, _ := f.fleet.request(v.ID)
	if sent.GroupID != "" || sent.Selector != "" || len(sent.Agents) != 0 || len(sent.Targets) != 1 || sent.Targets[0].PeerID != peers[1].ID {
		t.Fatalf("fleet got the group instead of pinned targets: %+v", sent)
	}
	f.fleet.members["raw-only"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}}
	req.GroupID = "raw-only"
	_, err = f.s.CreateCaptureSession(context.Background(), alice, req)
	wantCode(t, err, CodeForbidden)
}

// TestRawCaptureRecordsNeedCaptureRaw: reading a capture that holds raw
// data needs capture.raw as well as capture.read.
func TestRawCaptureRecordsNeedCaptureRaw(t *testing.T) {
	access, alice, _, carol := rawUsers(t)
	f := newFleetFixture(t, func(o *Options) { o.Access = access }, fakePeer(0, logsRaw))
	ctx := context.Background()
	v, err := f.s.CreateCaptureSession(ctx, carol, f.req)
	if err != nil {
		t.Fatal(err)
	}
	f.fleet.set(v.ID, "active", "active", nil)
	f.eventually(t, v.ID, func(v *CaptureSession) bool { return v.Data.ContainsRaw })
	_, err = f.s.GetCaptureRecords(ctx, alice, v.ID, CaptureRecordsRequest{})
	wantCode(t, err, CodeForbidden)
	_, err = f.s.ExportCaptureSelection(ctx, alice, v.ID, CaptureExportRequest{})
	wantCode(t, err, CodeForbidden)
	if _, err := f.s.GetCaptureSession(ctx, alice, v.ID); err != nil {
		t.Fatalf("the session itself stays readable: %v", err)
	}
	if _, err := f.s.GetCaptureRecords(ctx, carol, v.ID, CaptureRecordsRequest{}); err != nil {
		t.Fatalf("raw-capture holder = %v", err)
	}
}

// TestViewerReadsButDoesNotDeriveCaptures: capture.read reads records;
// deriving or exporting a fixture or scenario inspects the destination
// configuration, so it also needs run.submit.
func TestViewerReadsButDoesNotDeriveCaptures(t *testing.T) {
	scope := Scope{Kind: authz.ScopeProject, ID: artifact.DefaultProject}
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Principal: testCaller.Principal.ID}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "user:alice"}, Role: "engineer", Scope: scope},
		{Subject: Subject{Principal: "user:dave"}, Role: "viewer", Scope: scope},
	})
	if err != nil {
		t.Fatal(err)
	}
	user := func(id string) Caller {
		return Caller{Principal: Principal{ID: id, Kind: PrincipalUser, Name: strings.TrimPrefix(id, "user:")}, Client: "test", RequestID: "req-" + id}
	}
	alice, dave := user("user:alice"), user("user:dave")
	f := newFleetFixture(t, func(o *Options) { o.Access = access }, fakePeer(0, logsEgress))
	ctx := context.Background()
	v := dispatchData(t, f)
	ingestLog(t, f, v.ID, "one")
	page, err := f.s.GetCaptureRecords(ctx, dave, v.ID, CaptureRecordsRequest{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("viewer records %+v %v", page, err)
	}
	sel := CaptureInputSelection{Stream: page.Items[0].Stream, Watermark: page.Watermark, Receiver: "otlp"}
	scenario := CaptureScenarioRequest{ConfigYAML: captureDestination, Inputs: []CaptureInputSelection{sel}}
	fixture := CaptureDeriveRequest{ConfigYAML: captureDestination, Selection: sel}
	for name, derive := range map[string]func() error{
		"scenario": func() error { _, err := f.s.CreateScenarioFromCapture(ctx, dave, v.ID, scenario); return err },
		"fixture":  func() error { _, err := f.s.CreateFixtureFromCapture(ctx, dave, v.ID, fixture); return err },
		"export": func() error {
			_, err := f.s.ExportCaptureSelection(ctx, dave, v.ID, CaptureExportRequest{Fixture: &fixture})
			return err
		},
	} {
		err := derive()
		wantCode(t, err, CodeForbidden)
		if !strings.Contains(err.Error(), "run.submit") {
			t.Fatalf("%s refusal does not name the permission: %v", name, err)
		}
	}
	if _, err := f.s.CreateScenarioFromCapture(ctx, alice, v.ID, scenario); err != nil {
		t.Fatalf("engineer derivation = %v", err)
	}
}

// TestBusyCollectorNamesItsHolder: a Collector serves one capture at a
// time, and the refusal says who holds it and until when.
func TestBusyCollectorNamesItsHolder(t *testing.T) {
	access, alice, bob, _ := rawUsers(t)
	f := newFleetFixture(t, func(o *Options) { o.Access = access }, fakePeer(0, logsEgress), fakePeer(1, logsEgress))
	ctx := context.Background()
	held, err := f.s.CreateCaptureSession(ctx, alice, f.req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.CreateCaptureSession(ctx, bob, f.req)
	wantCode(t, err, CodeConflict)
	for _, want := range []string{"otel/collector-0", "user:alice", held.ID, "UTC", "one capture at a time"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	_, err = f.s.CreateCaptureSession(ctx, alice, f.req)
	if err == nil || !strings.Contains(err.Error(), "your capture "+held.ID) {
		t.Fatalf("own capture refusal = %v", err)
	}
	// A group skips the held Collector, as fleet does.
	peers := f.fleet.peers
	f.fleet.members["checkout"] = []fleetcapture.Agent{{ID: peers[0].AgentID, Connected: true}, {ID: peers[1].AgentID, Connected: true}}
	req := cloneCapture(f.req)
	req.CollectorIDs, req.GroupID = nil, "checkout"
	v, err := f.s.CreateCaptureSession(ctx, bob, req)
	if err != nil || len(v.Targets) != 1 || v.Targets[0].CollectorID != peers[1].ID {
		t.Fatalf("group beside a held Collector: %+v %v", v, err)
	}
	f.finish(t, held.ID, "completed")
	if _, err := f.s.CreateCaptureSession(ctx, bob, f.req); err != nil {
		t.Fatalf("after the holder ended = %v", err)
	}
}

// TestFleetBusyCollectorIsAConflict: a Collector another debugger or client
// holds is refused by fleet as collector_busy.
func TestFleetBusyCollectorIsAConflict(t *testing.T) {
	f := newCaptureFixture(t)
	f.fleet.fail("CreateCapture", &fleetcapture.Error{Status: 409, Code: "collector_busy", Message: "collector is serving another capture"})
	_, err := f.s.CreateCaptureSession(context.Background(), testCaller, f.req)
	wantCode(t, err, CodeConflict)
	if !strings.Contains(err.Error(), "already serving another capture") {
		t.Fatalf("busy refusal = %v", err)
	}
}
