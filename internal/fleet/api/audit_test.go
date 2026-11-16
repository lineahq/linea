package api

import (
	"context"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

type fakeAudit struct {
	fakeControl
	got fleet.AuditQuery
}

func (f *fakeAudit) Audit(_ context.Context, q fleet.AuditQuery) (fleet.AuditPage, error) {
	f.got = q
	return fleet.AuditPage{Items: []fleet.Event{{ID: 7, Kind: "credential.revoked"}}, NextBefore: "7"}, nil
}

// The audit record is for admins only, and its filters reach the store.
func TestAuditRouteIsAdminOnly(t *testing.T) {
	audit := &fakeAudit{}
	h := NewAuthorized(fleet.NewStore(1), roles{}, ServerInfo{ServerID: "test"}, audit, nil)
	for _, role := range []string{"operator", "reader"} {
		if w := call(t, h, "GET", "/api/v1/audit", role, nil); w.Code != 403 {
			t.Fatalf("%s read the audit record: %d", role, w.Code)
		}
	}
	w := call(t, h, "GET", "/api/v1/audit?limit=5&before=9&kind=credential.revoked&actor=a&since=2026-01-01T00:00:00Z&until=2026-02-01T00:00:00Z", "admin", nil)
	if w.Code != 200 {
		t.Fatalf("admin audit read: %d %s", w.Code, w.Body)
	}
	since, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if q := audit.got; q.Limit != 5 || q.Before != 9 || q.Kind != "credential.revoked" || q.Actor != "a" || !q.Since.Equal(since) || q.Until.IsZero() {
		t.Fatalf("query = %+v", q)
	}
	for _, bad := range []string{"before=0", "before=x", "since=yesterday", "kind=", "cursor=1"} {
		if w := call(t, h, "GET", "/api/v1/audit?"+bad, "admin", nil); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	if w := call(t, h, "GET", "/api/v1/audit", "admin", nil); w.Code != 200 {
		t.Fatal("unfiltered audit read failed")
	}
	plain := NewAuthorized(fleet.NewStore(1), roles{}, ServerInfo{ServerID: "test"}, fakeControl{}, nil)
	if w := call(t, plain, "GET", "/api/v1/audit", "admin", nil); w.Code != 404 {
		t.Fatalf("store without an audit record: %d", w.Code)
	}
}

func TestAuditedKinds(t *testing.T) {
	for _, c := range []struct {
		kind, actor string
		want        bool
	}{
		{"credential.revoked", "admin", true}, {"credential.deleted", "server", true}, {"group.updated", "op", true},
		{"config.held", "server", false}, {"config.hold_cleared", "server", false}, {"config.hold_cleared", "op", true},
		{"rollout.wave_released", "server", true}, {"plugin.disabled", "admin", true},
		{"agent.connected", "server", false}, {"replica.started", "server", false}, {"agents.expired", "server", false},
	} {
		if got := fleet.Audited(c.kind, c.actor); got != c.want {
			t.Errorf("Audited(%s, %s) = %v", c.kind, c.actor, got)
		}
	}
}
