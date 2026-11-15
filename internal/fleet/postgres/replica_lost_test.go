package postgres

import (
	"slices"
	"testing"
)

func TestLostSessionsAreTheAgentsNotOwned(t *testing.T) {
	ids := []string{"a", "b", "c"}
	generations := []int64{3, 5, 7}
	tests := []struct {
		name  string
		owned map[string]bool
		want  []lostSession
	}{
		{"all owned", map[string]bool{"a": true, "b": true, "c": true}, nil},
		{"none owned", nil, []lostSession{{"a", 3}, {"b", 5}, {"c", 7}}},
		{"one lost", map[string]bool{"a": true, "c": true}, []lostSession{{"b", 5}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := lostSessions(ids, generations, tc.owned); !slices.Equal(got, tc.want) {
				t.Fatalf("lost %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCloseLostSupersedesTheLostGeneration(t *testing.T) {
	type call struct {
		id         string
		generation uint64
	}
	var calls []call
	s := &Store{}
	// Without a gateway bound there is nothing to close.
	s.closeLost([]lostSession{{"a", 3}})
	s.SetSignals(func(id string, generation uint64) { calls = append(calls, call{id, generation}) }, func(string) {})
	s.closeLost([]lostSession{{"a", 3}, {"b", 5}})
	// The gateway closes a session whose generation is below the one given,
	// so the lost generation closes and a newer local session survives.
	if want := []call{{"a", 4}, {"b", 6}}; !slices.Equal(calls, want) {
		t.Fatalf("takeover calls %v, want %v", calls, want)
	}
}
