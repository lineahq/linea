package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// TestCandidateOverridesExpressOnlySwaps: a swap is one override of the
// pipeline's processor list; other changes are not verifiable yet.
func TestCandidateOverridesExpressOnlySwaps(t *testing.T) {
	idx := 0
	cases := []struct {
		name string
		rec  recommend.Recommendation
		want []string
	}{
		{"swap", recommend.Recommendation{Pipeline: "logs/main", Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"filter/a", "batch"}}},
			[]string{`yaml:service::pipelines::logs/main::processors: ["filter/a","batch"]`}},
		{"no change", recommend.Recommendation{Pipeline: "logs"}, nil},
		{"statement", recommend.Recommendation{Pipeline: "logs", Change: &recommend.ConfigChange{Op: recommend.OpRemoveStatement, Component: "transform", Index: &idx}}, nil},
		{"option", recommend.Recommendation{Pipeline: "traces", Change: &recommend.ConfigChange{Op: recommend.OpSetOption, Component: "tail_sampling", Option: "decision_wait"}}, nil},
		{"no pipeline", recommend.Recommendation{Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"batch"}}}, nil},
		{"separator", recommend.Recommendation{Pipeline: "logs", Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"a::b"}}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CandidateOverrides(c.rec)
			if c.want == nil {
				if !errors.Is(err, ErrNotVerifiable) {
					t.Fatalf("overrides = %v, %v; want ErrNotVerifiable", got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Fatalf("overrides = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}
