package recommend

import (
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// TestMeasuredCountsTheWorkInBothRuns: a verification measures the
// pipeline's record passes, the component's evaluations and, for batching
// rules, the batch sends, and leaves out what neither run shows.
func TestMeasuredCountsTheWorkInBothRuns(t *testing.T) {
	stage := func(id string, items ...int) analysis.Stage {
		s := analysis.Stage{Component: id, Pipeline: "logs", Kind: analysis.KindProcessor}
		for _, n := range items {
			s.Inputs = append(s.Inputs, analysis.StageInput{Diff: &analysis.Diff{Before: telemetry.Counts{Items: n}}})
		}
		return s
	}
	// The batch stage's inputs have no diff: its arrivals count its records.
	sends := func(n int, arrived ...int) []*analysis.Batching {
		b := &analysis.Batching{Processor: "batch", Pipeline: "logs", Flushes: make([]analysis.BatchFlush, n)}
		for _, items := range arrived {
			b.Arrivals = append(b.Arrivals, analysis.BatchArrival{Items: items})
		}
		return []*analysis.Batching{b}
	}
	batchStage := analysis.Stage{Component: "batch", Pipeline: "logs", Kind: analysis.KindProcessor, Inputs: []analysis.StageInput{{}, {}}}
	before := Input{
		Stages:    []analysis.Stage{batchStage, stage("filter/health", 10), {Component: "otlp", Pipeline: "logs", Kind: analysis.KindConnector}},
		Filtering: []*analysis.Filtering{filtering(10, 4, analysis.FilterVerified, analysis.FilterCondition{Evaluated: 10}, analysis.FilterCondition{Evaluated: 6})},
		Batching:  sends(3, 4, 6),
	}
	after := Input{
		Stages:    []analysis.Stage{stage("filter/health", 10), batchStage},
		Filtering: []*analysis.Filtering{filtering(10, 4, analysis.FilterVerified, analysis.FilterCondition{Evaluated: 10})},
		Batching:  sends(3, 2, 4),
	}
	cases := []struct {
		name string
		rec  Recommendation
		want []Measure
	}{
		{"filter condition", Recommendation{Rule: "filter.condition_never_matched", Pipeline: "logs", Components: []string{"processor/filter/health"}},
			[]Measure{{MeasureRecordPasses, "logs", 20, 16}, {MeasureEvaluations, "processor/filter/health", 16, 10}}},
		{"batching rule keeps equal sends", Recommendation{Rule: "batch.chained", Pipeline: "logs"},
			[]Measure{{MeasureRecordPasses, "logs", 20, 16}, {MeasureBatchSends, "logs", 3, 3}}},
		{"no pipeline", Recommendation{Rule: "config.unused_component", Components: []string{"processor/x"}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Measured(c.rec, before, after); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Measured = %+v, want %+v", got, c.want)
			}
		})
	}
}
