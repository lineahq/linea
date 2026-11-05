package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// compact removes the spacing jsonb adds to stored event details.
func compact(detail []byte) string {
	var b bytes.Buffer
	_ = json.Compact(&b, detail)
	return b.String()
}

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
func TestPostgresInventory(t *testing.T) {
	base := os.Getenv("LINEA_FLEET_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx := context.Background()
	fixture, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Exec(ctx, "CREATE DATABASE fleet_inventory"); err != nil {
		t.Fatal(err)
	}
	_ = fixture.Close(ctx)
	url := strings.Replace(base, "/fleet?", "/fleet_inventory?", 1)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal("fresh migration:", err)
	}
	issued, err := Bootstrap(ctx, url, "inventory-fleet", "bootstrap-admin")
	if err != nil {
		t.Fatal("bootstrap:", err)
	}
	s, err := Open(ctx, url, Options{ServerID: "inventory-fleet", MaxAgents: 5, MaxEvents: 1000, Retention: time.Hour,
		Heartbeat: 100 * time.Millisecond, Alive: 600 * time.Millisecond, Flush: 100 * time.Millisecond})
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(s.Close)
	admin := issued.Credential.ID
	c, err := auth.Generate(auth.CredentialRequest{ID: "00000000000000000000000000000041", Name: "inventory-agent", Role: "agent", Labels: map[string]string{"capture": "edge,core"}, ExpiresAt: time.Now().Add(time.Hour)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertCredential(ctx, admin, c.Credential); err != nil {
		t.Fatal(err)
	}
	credential := c.Credential.ID
	byHash, err := s.CredentialByHash(ctx, c.Credential.Hash)
	if err != nil || byHash.ID != credential || byHash.Labels["capture"] != "edge,core" {
		t.Fatalf("credential by hash: %+v %v", byHash, err)
	}
	introspected, err := (&auth.Service{Repository: s}).Introspect(ctx, [32]byte(c.Credential.Hash))
	if err != nil || !introspected.Active || introspected.Labels["capture"] != "edge,core" {
		t.Fatalf("introspection: %+v %v", introspected, err)
	}
	if adminView, err := (&auth.Service{Repository: s}).Introspect(ctx, [32]byte(issued.Credential.Hash)); err != nil || adminView.Active {
		t.Fatal("admin credential introspected as active")
	}
	listed, err := s.ListCredentials(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range listed.Items {
		if l.ID == credential && l.Labels["capture"] != "edge,core" {
			t.Fatal("listed credential lost labels")
		}
	}
	if _, err := s.CredentialByHash(ctx, make([]byte, 32)); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatal("unknown hash found")
	}

	healthy, unhealthy := true, false
	hash := strings.Repeat("ab", 32)
	type seed struct {
		service, namespace, os string
		healthy                *bool
	}
	ids := []string{}
	generations := map[string]uint64{}
	for i, a := range []seed{{"checkout", "shop", "linux", &healthy}, {"cart", "shop", "windows", &unhealthy}, {"edge", "ingress", "", nil}, {"cart", "staging", "linux", &healthy}} {
		id := strings.Repeat("4"+string(rune('1'+i)), 16)
		attributes := map[string]string{"service.name": a.service, "k8s.namespace.name": a.namespace}
		if a.os != "" {
			attributes["os.type"] = a.os
		}
		report := fleet.Report{Capabilities: 1, Healthy: a.healthy, Attributes: attributes}
		if a.healthy != nil {
			report.Status = &fleet.Status{Status: "StatusOK"}
			report.Components = []fleet.Component{{ID: "receiver:otlp", Pipelines: []string{"traces"}, Healthy: a.healthy}}
			report.ConfigHash = hash
		}
		generation, _, err := s.ConnectAgent(ctx, id, credential, report, time.Now())
		if err != nil {
			t.Fatal("connect:", err)
		}
		ids, generations[id] = append(ids, id), generation
	}

	// Detail reads carry components; pages leave them out.
	a, err := s.GetAgent(ctx, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if a.Attributes["os.type"] != "windows" || a.CredentialID != credential || a.ConfigHash != hash || len(a.Components) != 1 ||
		a.Health == nil || a.Health.Status != "StatusOK" || a.Health.ComponentsUnhealthy != 1 {
		t.Fatalf("detail = %+v health = %+v", a, a.Health)
	}
	page, err := s.ListAgents(ctx, fleet.AgentQuery{Limit: 10})
	if err != nil || len(page.Items) != 4 || page.Items[1].Components != nil || page.Items[1].Attributes["service.name"] != "cart" {
		t.Fatalf("page = %+v err = %v", page, err)
	}
	services := func(q fleet.AgentQuery) string {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = 10
		}
		p, err := s.ListAgents(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, a := range p.Items {
			names = append(names, a.Attributes["service.name"])
		}
		return strings.Join(names, ",")
	}
	selector := func(text string) fleet.Selector {
		t.Helper()
		sel, err := fleet.ParseSelector(text)
		if err != nil {
			t.Fatal(err)
		}
		return sel
	}
	for text, want := range map[string]string{
		"k8s.namespace.name=shop":              "checkout,cart",
		"service.name in (cart, edge)":         "cart,edge,cart",
		"os.type=linux":                        "checkout,cart",
		"os.type in (windows,solaris)":         "cart",
		"os.type":                              "checkout,cart,cart",
		"service.name=cart,os.type=linux":      "cart",
		"host.name":                            "",
		"k8s.namespace.name in (shop),os.type": "checkout,cart",
	} {
		if got := services(fleet.AgentQuery{Selector: selector(text)}); got != want {
			t.Fatalf("selector %q = %q, want %q", text, got, want)
		}
	}
	if got := services(fleet.AgentQuery{Healthy: &unhealthy}); got != "cart" {
		t.Fatalf("healthy=false = %q", got)
	}
	if got := services(fleet.AgentQuery{Sort: "service.name"}); got != "cart,cart,checkout,edge" {
		t.Fatalf("sorted = %q", got)
	}
	first, err := s.ListAgents(ctx, fleet.AgentQuery{Sort: "service.name", Limit: 3})
	if err != nil || first.NextAfter == "" {
		t.Fatal("sorted page has no position:", err)
	}
	if got := services(fleet.AgentQuery{Sort: "service.name", After: first.NextAfter}); got != "edge" {
		t.Fatalf("second sorted page = %q", got)
	}
	summary, err := s.Summary(ctx, "service.name")
	if err != nil || summary.Total != 4 || len(summary.Groups) != 3 || summary.Groups[0] != (fleet.SummaryGroup{Value: "cart", Total: 2, Connected: 2}) {
		t.Fatalf("summary = %+v err = %v", summary, err)
	}

	// Observed changes name the sections that changed.
	cursor, err := s.Events(ctx, "", 500)
	if err != nil {
		t.Fatal(err)
	}
	changed := fleet.Report{Capabilities: 1, Healthy: &healthy, Status: &fleet.Status{Status: "StatusOK"},
		Attributes: map[string]string{"service.name": "cart", "k8s.namespace.name": "shop", "os.type": "linux"},
		Components: []fleet.Component{{ID: "receiver:otlp", Pipelines: []string{"traces"}, Healthy: &healthy}}, ConfigHash: hash}
	if err := s.ObserveAgent(ctx, ids[1], credential, generations[ids[1]], changed, time.Now()); err != nil {
		t.Fatal("observe:", err)
	}
	events, err := s.Events(ctx, cursor.NextCursor, 100)
	if err != nil || len(events.Items) != 1 || events.Items[0].Kind != "agent.changed" ||
		compact(events.Items[0].Detail) != `{"changed":["health","attributes","components"]}` {
		t.Fatalf("change events = %+v err = %v", events.Items, err)
	}
	if a, _ := s.GetAgent(ctx, ids[1]); a.Health.ComponentsUnhealthy != 0 || a.Attributes["os.type"] != "linux" {
		t.Fatal("observed inventory not stored")
	}
	// An unchanged report writes no event; omitted sections keep stored values.
	if err := s.ObserveAgent(ctx, ids[1], credential, generations[ids[1]], fleet.Report{Capabilities: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAgent(ctx, ids[1]); a.Attributes["os.type"] != "linux" || a.ConfigHash != hash || len(a.Components) != 1 {
		t.Fatal("omitted sections were cleared")
	}
	// A reconnect's empty first message returns the stored report, so the
	// gateway's cache matches an unchanged full state.
	generation, stored, err := s.ConnectAgent(ctx, ids[1], credential, fleet.Report{Capabilities: 1}, time.Now())
	if err != nil || generation != generations[ids[1]]+1 || len(fleet.Changes(stored, changed)) != 0 {
		t.Fatalf("reconnect returned %+v, changes %v, err %v", stored, fleet.Changes(stored, changed), err)
	}
	generations[ids[1]] = generation

	// Selector groups count matches on detail reads only.
	shop := "00000000000000000000000000000042"
	g, err := s.PutGroup(ctx, admin, fleet.Group{ID: shop, Name: "shop", Selector: "k8s.namespace.name = shop"}, 0)
	if err != nil || g.Kind != fleet.GroupSelector || g.Selector != "k8s.namespace.name=shop" {
		t.Fatalf("selector group = %+v err = %v", g, err)
	}
	if g, err = s.GetGroup(ctx, shop); err != nil || g.MemberCount == nil || *g.MemberCount != 2 || len(g.AgentIDs) != 0 {
		t.Fatalf("selector detail = %+v err = %v", g, err)
	}
	static := "00000000000000000000000000000043"
	if _, err := s.PutGroup(ctx, admin, fleet.Group{ID: static, Name: "pinned", AgentIDs: []string{ids[2], ids[0]}}, 0); err != nil {
		t.Fatal(err)
	}
	groups, err := s.ListGroups(ctx, "", 10)
	if err != nil || len(groups.Items) != 2 || groups.Items[0].MemberCount != nil || groups.Items[1].MemberCount == nil || *groups.Items[1].MemberCount != 2 {
		t.Fatalf("group page = %+v err = %v", groups, err)
	}
	if got := services(fleet.AgentQuery{Group: static}); got != "checkout,edge" {
		t.Fatalf("static members = %q", got)
	}
	for _, invalid := range []fleet.Group{
		{ID: static, Name: "both", AgentIDs: []string{ids[0]}, Selector: "os.type"},
		{ID: static, Name: "bad", Selector: "os.type in ()"},
		{ID: static, Name: "large", AgentIDs: make([]string, fleet.MaxGroupMembers+1)},
	} {
		if _, err := s.PutGroup(ctx, admin, invalid, 1); !errors.Is(err, fleet.ErrInvalid) {
			t.Fatalf("invalid group %q accepted: %v", invalid.Name, err)
		}
	}

	// Capacity evicts the longest-disconnected agent; connected ones stay.
	// Both stay within the one-hour retention the leader enforces meanwhile.
	if err := s.DisconnectAgent(ctx, ids[2], generations[ids[2]], time.Now().Add(-50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectAgent(ctx, ids[3], generations[ids[3]], time.Now().Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fifth := strings.Repeat("45", 16)
	if _, _, err := s.ConnectAgent(ctx, fifth, credential, fleet.Report{}, time.Now()); err != nil {
		t.Fatal("connect within capacity:", err)
	}
	cursor, _ = s.Events(ctx, "", 1000)
	sixth := strings.Repeat("46", 16)
	if _, _, err := s.ConnectAgent(ctx, sixth, credential, fleet.Report{}, time.Now()); err != nil {
		t.Fatal("eviction did not make room:", err)
	}
	if _, err := s.GetAgent(ctx, ids[2]); err != fleet.ErrNotFound {
		t.Fatal("longest-disconnected agent was not evicted:", err)
	}
	if g, _ := s.GetGroup(ctx, static); g.Revision != 2 || len(g.AgentIDs) != 1 {
		t.Fatalf("eviction left static membership: %+v", g)
	}
	events, _ = s.Events(ctx, cursor.NextCursor, 100)
	kinds := []string{}
	for _, e := range events.Items {
		kinds = append(kinds, e.Kind)
		if e.Kind == "agents.expired" && compact(e.Detail) != `{"count":1,"reason":"capacity"}` {
			t.Fatalf("eviction detail = %s", e.Detail)
		}
	}
	if got := strings.Join(kinds, ","); got != "group.updated,agents.expired,agent.registered,agent.connected" {
		t.Fatalf("eviction events = %s", got)
	}
	if _, _, err := s.ConnectAgent(ctx, strings.Repeat("47", 16), credential, fleet.Report{}, time.Now()); err != nil {
		t.Fatal("second eviction:", err)
	}
	if _, _, err := s.ConnectAgent(ctx, strings.Repeat("48", 16), credential, fleet.Report{}, time.Now()); err != fleet.ErrCapacity {
		t.Fatal("capacity of connected agents was exceeded:", err)
	}

	// Retention deletes only agents disconnected longer than the period.
	if err := s.DisconnectAgent(ctx, fifth, 1, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DisconnectAgent(ctx, sixth, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.retain(ctx); err != nil {
		t.Fatal("retention:", err)
	}
	if _, err := s.GetAgent(ctx, fifth); err != fleet.ErrNotFound {
		t.Fatal("expired agent retained:", err)
	}
	if _, err := s.GetAgent(ctx, sixth); err != nil {
		t.Fatal("recent agent expired:", err)
	}
}
