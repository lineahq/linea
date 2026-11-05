package platform

import (
	"context"
	"slices"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
)

func fleetService(t *testing.T) (*Service, *fleetcapturetest.Server) {
	t.Helper()
	fleet := fleetcapturetest.NewServer(t)
	s := New(Options{Capture: CaptureOptions{Fleet: FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(s.Close)
	return s, fleet
}

// TestFleetInventoryLinksCapture reads fleet's overview and agents; the
// agent the livecapture plugin knows lists its Collector.
func TestFleetInventoryLinksCapture(t *testing.T) {
	s, _ := fleetService(t)
	ctx, me := context.Background(), Local("test")
	o, err := s.GetFleetOverview(ctx, me, "service.name")
	if err != nil || !o.Enabled || !o.ConfigManagement || o.Total != 1 || len(o.Groups) != 1 || o.Endpoint == "" {
		t.Fatalf("overview = %+v, %v", o, err)
	}
	page, err := s.ListFleetAgents(ctx, me, FleetAgentQuery{Selector: "service.name=gateway"})
	if err != nil || len(page.Items) != 1 || !page.CaptureLinked {
		t.Fatalf("agents = %+v, %v", page, err)
	}
	a := page.Items[0]
	if a.ID != fleetcapturetest.Peer.AgentID || !slices.Equal(a.CollectorIDs, []string{fleetcapturetest.Peer.ID}) || a.Attributes["service.name"] != "gateway" || a.Health == nil || !a.Health.Healthy {
		t.Fatalf("agent = %+v", a)
	}
	d, err := s.GetFleetAgent(ctx, me, a.ID)
	if err != nil || len(d.Components) != 1 || d.Config == nil || !d.CaptureLinked {
		t.Fatalf("detail = %+v, %v", d, err)
	}
	if _, err := s.GetFleetAgent(ctx, me, "not-an-id"); err == nil {
		t.Fatal("a malformed agent ID was sent to fleet")
	}
	if _, err := s.RetryFleetAgentConfig(ctx, me, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListFleetAgents(ctx, me, FleetAgentQuery{Limit: 501}); err == nil {
		t.Fatal("a page above fleet's bound was accepted")
	}

	off := New(Options{})
	t.Cleanup(off.Close)
	if o, err := off.GetFleetOverview(ctx, me, ""); err != nil || o.Enabled {
		t.Fatalf("overview without fleet = %+v, %v", o, err)
	}
	_, err = off.ListFleetAgents(ctx, me, FleetAgentQuery{})
	wantCode(t, err, CodeUnavailable)
}

// TestFleetGroupWritesFollowRevisions creates, replaces and deletes a group;
// a stale revision is a conflict.
func TestFleetGroupWritesFollowRevisions(t *testing.T) {
	s, _ := fleetService(t)
	ctx, me := context.Background(), Local("test")
	g, err := s.SaveCollectorGroup(ctx, me, CollectorGroupSave{Name: "Edge two", AgentIDs: []string{}})
	if err != nil || g.Revision != 1 || g.Kind != "static" || len(g.ID) != 32 {
		t.Fatalf("created = %+v, %v", g, err)
	}
	g, err = s.SaveCollectorGroup(ctx, me, CollectorGroupSave{ID: g.ID, Revision: 1, Name: "Edge two", Selector: "service.name=gateway"})
	if err != nil || g.Revision != 2 || g.Selector != "service.name=gateway" {
		t.Fatalf("replaced = %+v, %v", g, err)
	}
	_, err = s.SaveCollectorGroup(ctx, me, CollectorGroupSave{ID: g.ID, Revision: 1, Name: "Edge two", Selector: "host.name"})
	wantCode(t, err, CodeConflict)
	_, err = s.SaveCollectorGroup(ctx, me, CollectorGroupSave{Name: "Both", Selector: "a", AgentIDs: []string{}})
	wantCode(t, err, CodeInvalidRequest)
	wantCode(t, s.DeleteCollectorGroup(ctx, me, g.ID, CollectorGroupDelete{Revision: 1}), CodeConflict)
	if err := s.DeleteCollectorGroup(ctx, me, g.ID, CollectorGroupDelete{Revision: 2}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListFleetEvents(ctx, me, CollectorPageRequest{})
	if err != nil || len(events.Items) != 3 || events.Items[0].Kind != "group.created" || events.NextCursor != "3" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if more, err := s.ListFleetEvents(ctx, me, CollectorPageRequest{Cursor: events.NextCursor}); err != nil || len(more.Items) != 0 {
		t.Fatalf("events after the cursor = %+v, %v", more, err)
	}
	if events.Items[0].ActingFor != nil {
		t.Fatalf("a local caller sent acting-for: %+v", events.Items[0].ActingFor)
	}
}

// TestFleetWritesNameTheActingUser: a team user's fleet writes carry the
// acting-for headers, and fleet's events return them.
func TestFleetWritesNameTheActingUser(t *testing.T) {
	fleet := fleetcapturetest.NewServer(t)
	access, err := NewPolicy([]Binding{{Subject: Subject{Principal: "user:alice"}, Role: "fleet-operator", Scope: ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Access: access, Capture: CaptureOptions{Fleet: FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	alice := Caller{Principal: Principal{ID: "user:alice", Kind: PrincipalUser, Name: "Alice"}, Client: "http", RequestID: "req-1"}
	if _, err := s.SaveCollectorGroup(ctx, alice, CollectorGroupSave{Name: "Edge", AgentIDs: []string{}}); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListFleetEvents(ctx, alice, CollectorPageRequest{})
	if err != nil || len(events.Items) != 1 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	if got, want := events.Items[0].ActingFor, (fleetcapture.Acting{ID: "user:alice", Name: "Alice", RequestID: "req-1"}); got == nil || *got != want {
		t.Fatalf("acting for %+v, want %+v", got, want)
	}
}

// TestFleetConfigLifecycle creates a config and versions, assigns it, and
// steers the rollouts that move the assignment.
func TestFleetConfigLifecycle(t *testing.T) {
	s, fleet := fleetService(t)
	ctx, me := context.Background(), Local("test")
	c, err := s.CreateFleetConfig(ctx, me, FleetConfigCreate{Name: "edge-pipeline", Description: "Edge"})
	if err != nil || c.Revision != 1 {
		t.Fatalf("config = %+v, %v", c, err)
	}
	_, err = s.CreateFleetConfig(ctx, me, FleetConfigCreate{Name: "Edge Pipeline"})
	wantCode(t, err, CodeInvalidRequest)
	name := "edge"
	if c, err = s.UpdateFleetConfig(ctx, me, c.ID, FleetConfigUpdate{Revision: 1, Name: &name}); err != nil || c.Name != "edge" || c.Revision != 2 {
		t.Fatalf("renamed = %+v, %v", c, err)
	}
	_, err = s.UpdateFleetConfig(ctx, me, c.ID, FleetConfigUpdate{Revision: 1, Name: &name})
	wantCode(t, err, CodeConflict)

	body := "processors:\n  batch: {}\n"
	v, err := s.CreateFleetConfigVersion(ctx, me, c.ID, FleetConfigVersionCreate{Body: body, Message: "batch"})
	if err != nil || v.Version != 1 || v.Body != "" {
		t.Fatalf("version = %+v, %v", v, err)
	}
	_, err = s.CreateFleetConfigVersion(ctx, me, c.ID, FleetConfigVersionCreate{Body: "exporters:\n  otlp:\n    password: hunter2\n"})
	wantCode(t, err, CodeInvalidRequest)
	if v, err = s.GetFleetConfigVersion(ctx, me, c.ID, 1); err != nil || v.Body != body {
		t.Fatalf("version read = %+v, %v", v, err)
	}
	if vs, err := s.ListFleetConfigVersions(ctx, me, c.ID, CollectorPageRequest{}); err != nil || len(vs.Items) != 1 || vs.Items[0].Body != "" {
		t.Fatalf("versions = %+v, %v", vs, err)
	}
	_, err = s.ListFleetConfigVersions(ctx, me, c.ID, CollectorPageRequest{Cursor: "x"})
	wantCode(t, err, CodeInvalidRequest)

	a, err := s.CreateFleetAssignment(ctx, me, FleetAssignmentCreate{Name: "edge", ConfigID: c.ID, Version: 1, Layer: 20, Selector: "service.name=gateway",
		Rollout: &FleetRolloutPolicy{Waves: []FleetRolloutWave{{Percent: new(100)}}}})
	if err != nil || a.RolloutID == "" || a.Layer != 20 {
		t.Fatalf("assignment = %+v, %v", a, err)
	}
	_, err = s.CreateFleetAssignment(ctx, me, FleetAssignmentCreate{Name: "both", ConfigID: c.ID, Version: 1, GroupID: c.ID, Selector: "a"})
	wantCode(t, err, CodeInvalidRequest)
	p, err := s.PreviewFleetAssignment(ctx, me, a.ID, CollectorPageRequest{})
	if err != nil || len(p.Items) != 1 || p.Items[0].Desired == nil || p.Items[0].Desired.Layers[0].Name != "20-edge" {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	ro, err := s.GetFleetRollout(ctx, me, a.RolloutID)
	if err != nil || ro.State != "running" || len(ro.Waves) != 1 || ro.Waves[0].Outcomes["pending"] != 1 {
		t.Fatalf("rollout = %+v, %v", ro, err)
	}
	_, err = s.StartFleetRollout(ctx, me, FleetRolloutStart{AssignmentID: a.ID, ToVersion: 1})
	wantCode(t, err, CodeConflict)
	if ro, err = s.ControlFleetRollout(ctx, me, ro.ID, FleetRolloutAction{Action: "pause"}); err != nil || ro.State != "paused" || ro.Reason != "operator" {
		t.Fatalf("paused = %+v, %v", ro, err)
	}
	_, err = s.ControlFleetRollout(ctx, me, ro.ID, FleetRolloutAction{Action: "pause"})
	wantCode(t, err, CodeConflict)
	_, err = s.ControlFleetRollout(ctx, me, ro.ID, FleetRolloutAction{Action: "explode"})
	wantCode(t, err, CodeInvalidRequest)
	targets, err := s.ListFleetRolloutTargets(ctx, me, ro.ID, FleetRolloutTargetQuery{Outcome: "pending"})
	if err != nil || len(targets.Items) != 1 {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	_, err = s.ListFleetRolloutTargets(ctx, me, ro.ID, FleetRolloutTargetQuery{Outcome: "exploded"})
	wantCode(t, err, CodeInvalidRequest)
	next, err := s.StartFleetRollout(ctx, me, FleetRolloutStart{AssignmentID: a.ID, Delete: true, Policy: FleetRolloutPolicy{OnFailure: "rollback"}})
	if err != nil || next.ID == ro.ID {
		t.Fatalf("delete rollout = %+v, %v", next, err)
	}
	_, err = s.StartFleetRollout(ctx, me, FleetRolloutStart{AssignmentID: a.ID, Delete: true, ToVersion: 1})
	wantCode(t, err, CodeInvalidRequest)
	if page, err := s.ListFleetRollouts(ctx, me, CollectorPageRequest{}); err != nil || len(page.Items) != 2 {
		t.Fatalf("rollouts = %+v, %v", page, err)
	}
	if page, err := s.ListFleetAssignments(ctx, me, CollectorPageRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("assignments = %+v, %v", page, err)
	}
	wantCode(t, s.DeleteFleetConfig(ctx, me, c.ID, FleetConfigDelete{Revision: 2}), CodeConflict)
	if page, err := s.ListFleetConfigs(ctx, me, CollectorPageRequest{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("configs = %+v, %v", page, err)
	}
	if got := fleet.Retried(); len(got) != 0 {
		t.Fatalf("retried = %v", got)
	}
}
