package fleettest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

// InventorySubject is one fresh, empty agent inventory under test.
type InventorySubject struct {
	Store fleet.Repository
	// Credentials are two distinct, active agent credentials the store
	// accepts in ConnectAgent.
	Credentials [2]string
}

// RunInventory runs the agent inventory contract every store must keep:
// generation fencing, the binding of a connected UID to its credential, and
// keyset paging with the connected filter. Adopting a disconnected UID is
// left out: the memory store knows no credentials to judge it by.
func RunInventory(t *testing.T, newSubject func(t *testing.T) InventorySubject) {
	ctx := context.Background()
	healthy := true
	agent := func(i int) string { return strings.Repeat(string(rune('a'+i))+"0", 16) }

	t.Run("a newer connection fences the older generation", func(t *testing.T) {
		s := newSubject(t)
		now := time.Now().UTC()
		first, _, err := s.Store.ConnectAgent(ctx, agent(0), s.Credentials[0], fleet.Report{Capabilities: 1, Healthy: &healthy}, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Store.ObserveAgent(ctx, agent(0), s.Credentials[0], first, fleet.Report{Capabilities: 3}, now); err != nil {
			t.Fatal("observe current generation:", err)
		}
		second, _, err := s.Store.ConnectAgent(ctx, agent(0), s.Credentials[0], fleet.Report{Capabilities: 5}, now)
		if err != nil || second <= first {
			t.Fatalf("takeover: generation %d after %d: %v", second, first, err)
		}
		if err := s.Store.ObserveAgent(ctx, agent(0), s.Credentials[0], first, fleet.Report{Capabilities: 99}, now); !errors.Is(err, fleet.ErrStale) {
			t.Fatalf("observe old generation: %v", err)
		}
		if err := s.Store.DisconnectAgent(ctx, agent(0), first, now); err != nil {
			t.Fatal(err)
		}
		a, err := s.Store.GetAgent(ctx, agent(0))
		if err != nil || !a.Connected || a.Generation != second || a.Capabilities != 5 {
			t.Fatalf("old generation changed the agent: %+v %v", a, err)
		}
		if err := s.Store.DisconnectAgent(ctx, agent(0), second, now); err != nil {
			t.Fatal(err)
		}
		if a, err = s.Store.GetAgent(ctx, agent(0)); err != nil || a.Connected {
			t.Fatalf("disconnect of the current generation: %+v %v", a, err)
		}
	})

	t.Run("a connected UID stays with its credential", func(t *testing.T) {
		s := newSubject(t)
		now := time.Now().UTC()
		gen, _, err := s.Store.ConnectAgent(ctx, agent(1), s.Credentials[0], fleet.Report{Capabilities: 7}, now)
		if err != nil {
			t.Fatal(err)
		}
		if g, _, err := s.Store.ConnectAgent(ctx, agent(1), s.Credentials[1], fleet.Report{Capabilities: 1}, now); !errors.Is(err, fleet.ErrUIDHeld) || g != 0 {
			t.Fatalf("another credential: generation %d, %v", g, err)
		}
		a, err := s.Store.GetAgent(ctx, agent(1))
		if err != nil || a.CredentialID != s.Credentials[0] || a.Generation != gen || a.Capabilities != 7 || !a.Connected {
			t.Fatalf("refused connect changed the agent: %+v %v", a, err)
		}
		if _, err := s.Store.GetAgent(ctx, agent(9)); !errors.Is(err, fleet.ErrNotFound) {
			t.Fatalf("unknown agent: %v", err)
		}
	})

	t.Run("pages follow the agent ID and the connected filter", func(t *testing.T) {
		s := newSubject(t)
		now := time.Now().UTC()
		generations := map[string]uint64{}
		for _, i := range []int{4, 2, 3} {
			g, _, err := s.Store.ConnectAgent(ctx, agent(i), s.Credentials[0], fleet.Report{}, now)
			if err != nil {
				t.Fatal(err)
			}
			generations[agent(i)] = g
		}
		var ids []string
		q := fleet.AgentQuery{Limit: 2}
		for pages := 0; ; pages++ {
			if pages > 3 {
				t.Fatal("paging does not end")
			}
			page, err := s.Store.ListAgents(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > 2 {
				t.Fatalf("page of %d above the limit", len(page.Items))
			}
			for _, a := range page.Items {
				ids = append(ids, a.ID)
			}
			if page.NextAfter == "" {
				break
			}
			q.After = page.NextAfter
		}
		if want := []string{agent(2), agent(3), agent(4)}; !slices.Equal(ids, want) {
			t.Fatalf("listed %v, want %v", ids, want)
		}
		if err := s.Store.DisconnectAgent(ctx, agent(3), generations[agent(3)], now); err != nil {
			t.Fatal(err)
		}
		for _, connected := range []bool{true, false} {
			page, err := s.Store.ListAgents(ctx, fleet.AgentQuery{Limit: 10, Connected: &connected})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, a := range page.Items {
				got = append(got, a.ID)
			}
			want := []string{agent(2), agent(4)}
			if !connected {
				want = []string{agent(3)}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("connected=%t listed %v, want %v", connected, got, want)
			}
		}
	})
}
