package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// TestExtendedComponentsMatchTheReference runs each configuration in
// testdata/components for real, instrumented, and checks that its output
// equals an uninstrumented reference Collector's and that the stage analysis
// shows what it did.
func TestExtendedComponentsMatchTheReference(t *testing.T) {
	var (
		componentsDir = filepath.Join("..", "..", "..", "testdata", "components")
		traces        = filepath.Join("..", "..", "..", "testdata", "traces", "traces.json")
		logs          = filepath.Join("..", "..", "..", "testdata", "multisignal", "logs.json")
		metrics       = filepath.Join("..", "..", "..", "samples", "metrics-rename", "input.json")
		deltas        = filepath.Join(componentsDir, "delta_sums.json")
	)
	tests := []struct {
		config, fixture, stage string
		outcome                analysis.Outcome
		removed                int
		// changes are change paths the stage must report.
		changes []string
	}{
		{"redaction", traces, "redaction", analysis.OutcomeEmitted, 0, []string{`attributes["user.email"]`, `attributes["redaction.masked.count"]`}},
		{"groupbyattrs", traces, "groupbyattrs", analysis.OutcomeEmitted, 0, []string{`attributes["http.status_code"]`}},
		{"probabilistic_sampler", traces, "probabilistic_sampler", analysis.OutcomeNoOutput, 3, nil},
		{"span", traces, "span", analysis.OutcomeEmitted, 0, []string{"name"}},
		{"metricstransform", metrics, "metricstransform", analysis.OutcomeEmitted, 0, []string{"metric.name", `attributes["unit.source"]`}},
		{"cumulativetodelta", metrics, "cumulativetodelta", analysis.OutcomeEmitted, 1, nil},
		{"deltatocumulative", deltas, "deltatocumulative", analysis.OutcomeEmitted, 0, []string{"value", "start_time_unix_nano", "metric.aggregation_temporality"}},
		{"memory_limiter", logs, "memory_limiter", analysis.OutcomeEmitted, 0, nil},
		{"forward", logs, "filter/info", analysis.OutcomeEmitted, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.config, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{Config: filepath.Join(componentsDir, tt.config+".yaml"), Fixture: tt.fixture, Compare: true})
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Comparison.Equal {
				t.Fatalf("comparison: %v", rep.Comparison.Differences)
			}
			var in *analysis.StageInput
			for i, s := range rep.Stages {
				if s.Component == tt.stage && len(s.Inputs) == 1 {
					in = &rep.Stages[i].Inputs[0]
				}
			}
			if in == nil {
				t.Fatalf("no stage %s with one input in %+v", tt.stage, rep.Stages)
			}
			if in.Outcome != tt.outcome || in.Diff == nil || len(in.Diff.Removed) != tt.removed {
				t.Fatalf("%s: outcome %s, diff %+v; want %s with %d removed", tt.stage, in.Outcome, in.Diff, tt.outcome, tt.removed)
			}
			var paths []string
			for _, c := range in.Diff.Changes {
				paths = append(paths, c.Path)
			}
			for _, p := range tt.changes {
				if !slices.Contains(paths, p) {
					t.Errorf("changes %v lack %s", paths, p)
				}
			}
			// Every record is followed to an end the analysis is sure of.
			for _, l := range rep.Lineage {
				for _, s := range l.Stops {
					if s.End == analysis.EndUnresolved {
						t.Errorf("%s ends unresolved at %s", l.Name, s.Component)
					}
				}
			}
		})
	}
}
