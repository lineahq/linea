package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

func TestCollectorsAreFleetPeersAndCopiesAreOwned(t *testing.T) {
	raw := CapturePoint{Pipeline: "logs", Point: "ingress", Signal: "logs", Sensitivity: "raw"}
	f := newFleetFixture(t, nil, fakePeer(1, logsEgress, raw), fakePeer(0, logsEgress))
	ctx := context.Background()
	page, err := f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{})
	if err != nil || !page.Enabled || len(page.Items) != 2 {
		t.Fatalf("%+v %v", page, err)
	}
	first := page.Items[0]
	if first.ID != fakePeer(0).ID || first.AgentID != fakePeer(0).AgentID || first.Pod != "collector-0" || first.PointCount != 1 || first.RawPointCount != 0 {
		t.Fatalf("peers are sorted by ID and carry identity: %+v", first)
	}
	second, err := f.s.GetCollector(ctx, testCaller, fakePeer(1).ID)
	if err != nil || second.PointCount != 2 || second.RawPointCount != 1 || second.Generation != 1 {
		t.Fatalf("%+v %v", second, err)
	}
	points, err := f.s.ListCapturePoints(ctx, testCaller, second.ID, CollectorPageRequest{})
	if err != nil || len(points.Items) != 2 || points.Items[0].Point != "egress" || points.Items[1].Sensitivity != "raw" {
		t.Fatalf("points %+v, %v", points, err)
	}
	points.Items[0].Point = "client-mutated"
	if points, _ = f.s.ListCapturePoints(ctx, testCaller, second.ID, CollectorPageRequest{}); points.Items[0].Point != "egress" {
		t.Fatal("query result aliased the peer snapshot")
	}
	_, err = f.s.GetCollector(ctx, testCaller, strings.Repeat("f", 64))
	wantCode(t, err, CodeNotFound)
	_, err = f.s.GetCollector(ctx, testCaller, "collector-0")
	wantCode(t, err, CodeNotFound)
	// A reconnect advances the generation the next read reports.
	f.fleet.setPeer(0, func(p *fleetcapture.Peer) { p.Generation = 2 }) // fleet lists peer 1 first
	if c, _ := f.s.GetCollector(ctx, testCaller, second.ID); c.Generation != 1 {
		t.Fatal("peer reads are not cached for their freshness window")
	}
	f.advance(peerFreshness)
	if c, _ := f.s.GetCollector(ctx, testCaller, second.ID); c.Generation != 2 {
		t.Fatalf("stale generation %d", c.Generation)
	}
	f.fleet.fail("Peers", refusal(503, "unavailable"))
	f.advance(peerFreshness)
	_, err = f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{})
	wantCode(t, err, CodeUnavailable)
	f.s.Close()
	_, err = f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{})
	wantCode(t, err, CodeUnavailable)
}

func TestCollectorPagingRestartsWhenThePeersChange(t *testing.T) {
	peers := []fleetcapture.Peer{}
	for i := range 70 {
		peers = append(peers, fakePeer(i, logsEgress))
	}
	invalid := fakePeer(99, logsEgress)
	invalid.Identity.Pod = ""
	peers = append(peers, invalid)
	f := newFleetFixture(t, nil, peers...)
	ctx := context.Background()
	// A 410 means the plugin restarted mid-read; the read starts again.
	f.fleet.fail("Peers", refusal(410, "restarted"))
	seen := map[string]bool{}
	page, err := f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{Limit: 50})
	if err != nil || len(page.Items) != 50 || page.NextCursor == "" {
		t.Fatalf("%d %v", len(page.Items), err)
	}
	for _, c := range page.Items {
		seen[c.ID] = true
	}
	// Status reads within the freshness window keep the cursor valid.
	next, err := f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{Limit: 50, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 20 || next.NextCursor != "" {
		t.Fatalf("%d %v", len(next.Items), err)
	}
	for _, c := range next.Items {
		seen[c.ID] = true
	}
	if len(seen) != 70 || seen[invalid.ID] {
		t.Fatal("paging lost, repeated or admitted an invalid peer")
	}
	f.fleet.setPeer(0, func(p *fleetcapture.Peer) { p.Identity.CapturePoints[0].Sensitivity = "raw" })
	f.advance(peerFreshness)
	_, err = f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{Limit: 50, Cursor: page.NextCursor})
	wantCode(t, err, CodeConflict)
	_, err = f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{Limit: 50, Cursor: "not-a-cursor"})
	wantCode(t, err, CodeInvalidRequest)
	_, err = f.s.ListCollectors(ctx, testCaller, CollectorPageRequest{Limit: 101})
	wantCode(t, err, CodeInvalidRequest)
}

func TestCollectorGroupsAreReadFromFleet(t *testing.T) {
	f := newFleetFixture(t, nil, fakePeer(0, logsEgress), fakePeer(1, logsEgress))
	ctx := context.Background()
	two := 2
	f.fleet.groups = []fleetcapture.Group{
		{ID: "checkout", Name: "Checkout", Kind: "static", Revision: 3, MemberCount: &two, AgentIDs: []string{fakePeer(0).AgentID, fakePeer(5).AgentID}},
		{ID: "prod", Name: "Production", Kind: "selector", Revision: 1, Selector: "env=prod"},
	}
	f.fleet.members["checkout"] = []fleetcapture.Agent{{ID: fakePeer(0).AgentID, Connected: true}, {ID: fakePeer(5).AgentID}}
	page, err := f.s.ListCollectorGroups(ctx, testCaller, CollectorPageRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "checkout" || page.NextCursor == "" {
		t.Fatalf("%+v %v", page, err)
	}
	page, err = f.s.ListCollectorGroups(ctx, testCaller, CollectorPageRequest{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(page.Items) != 1 || page.Items[0].Selector != "env=prod" || page.NextCursor != "" {
		t.Fatalf("%+v %v", page, err)
	}
	g, err := f.s.GetCollectorGroup(ctx, testCaller, "checkout")
	if err != nil || g.Kind != "static" || *g.MemberCount != 2 || len(g.AgentIDs) != 2 {
		t.Fatalf("%+v %v", g, err)
	}
	members, err := f.s.ListCollectorGroupMembers(ctx, testCaller, "checkout", CollectorPageRequest{})
	if err != nil || len(members.Items) != 2 {
		t.Fatalf("%+v %v", members, err)
	}
	if m := members.Items[0]; !m.Connected || len(m.CollectorIDs) != 1 || m.CollectorIDs[0] != fakePeer(0).ID {
		t.Fatalf("connected member %+v", m)
	}
	if m := members.Items[1]; m.Connected || len(m.CollectorIDs) != 0 {
		t.Fatalf("disconnected member %+v", m)
	}
	_, err = f.s.GetCollectorGroup(ctx, testCaller, "missing")
	wantCode(t, err, CodeNotFound)
	_, err = f.s.ListCollectorGroupMembers(ctx, testCaller, "missing", CollectorPageRequest{})
	wantCode(t, err, CodeNotFound)
	_, err = f.s.ListCollectorGroups(ctx, testCaller, CollectorPageRequest{Limit: 101})
	wantCode(t, err, CodeInvalidRequest)
	if !members.CaptureLinked {
		t.Fatal("members of a fleet with live capture are not capture linked")
	}
}

// TestCollectorGroupMembersWithoutLiveCapture lists a group's members on a
// fleet whose livecapture plugin refuses: the members stay readable, without
// Collectors.
func TestCollectorGroupMembersWithoutLiveCapture(t *testing.T) {
	f := newFleetFixture(t, nil, fakePeer(0, logsEgress))
	f.fleet.groups = []fleetcapture.Group{{ID: "edge", Name: "edge", Kind: "selector", Revision: 1, Selector: "env=edge"}}
	f.fleet.members["edge"] = []fleetcapture.Agent{{ID: fakePeer(0).AgentID, Connected: true}}
	refused := errors.New("fleet: plugin API request refused")
	f.fleet.failures["Peers"] = []error{refused, refused, refused}
	members, err := f.s.ListCollectorGroupMembers(context.Background(), testCaller, "edge", CollectorPageRequest{})
	if err != nil || members.CaptureLinked || len(members.Items) != 1 || !members.Items[0].Connected || len(members.Items[0].CollectorIDs) != 0 {
		t.Fatalf("%+v %v", members, err)
	}
}
