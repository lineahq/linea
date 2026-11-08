package recommend

import (
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// MeasureMetric names what a Measure counts.
type MeasureMetric string

const (
	// MeasureRecordPasses counts the records the processors of the
	// recommendation's pipeline received, each processor counting every
	// record once (the total of a record benefit).
	MeasureRecordPasses MeasureMetric = "record_passes"
	// MeasureEvaluations counts the evaluations of the component's OTTL
	// statements (verified replays) or filter conditions (verified
	// requests).
	MeasureEvaluations MeasureMetric = "evaluations"
	// MeasureBatchSends counts the sends of the pipeline's batch processors.
	MeasureBatchSends MeasureMetric = "batch_sends"
)

// Measure is one count of the work a recommendation is about, in the run
// (Before) and in the candidate that applied its change on the same inputs
// (After). Subject is the pipeline or component ref it counts.
type Measure struct {
	Metric  MeasureMetric `json:"metric"`
	Subject string        `json:"subject"`
	Before  int           `json:"before"`
	After   int           `json:"after"`
}

// Measured counts the work a recommendation is about in its run and in its
// verification's candidate. It measures only what both runs show: a count
// either run cannot give is left out, so an absent measure is unknown, not
// zero.
func Measured(rec Recommendation, before, after Input) []Measure {
	var out []Measure
	if rec.Pipeline != "" {
		b, a := processorPasses(before, rec.Pipeline), processorPasses(after, rec.Pipeline)
		if b > 0 || a > 0 {
			out = append(out, Measure{MeasureRecordPasses, rec.Pipeline, b, a})
		}
	}
	if len(rec.Components) > 0 {
		ref := rec.Components[0]
		id := strings.TrimPrefix(ref, string(topology.KindProcessor)+"/")
		var b, a int
		switch {
		case strings.HasPrefix(rec.Rule, "ottl."):
			b, a = statementEvaluations(before.OTTL, id), statementEvaluations(after.OTTL, id)
		case strings.HasPrefix(rec.Rule, "filter."):
			b, a = conditionEvaluations(before.Filtering, id), conditionEvaluations(after.Filtering, id)
		}
		if b > 0 || a > 0 {
			out = append(out, Measure{MeasureEvaluations, ref, b, a})
		}
	}
	if rec.Pipeline != "" {
		b, bok := batchSends(before.Batching, rec.Pipeline)
		a, aok := batchSends(after.Batching, rec.Pipeline)
		regroups := strings.HasPrefix(rec.Rule, "batch.") || strings.HasPrefix(rec.Rule, "grouping.")
		if (bok || aok) && (regroups || a != b) {
			out = append(out, Measure{MeasureBatchSends, rec.Pipeline, b, a})
		}
	}
	return out
}

// batchSends counts the sends of a pipeline's batch processors, and whether
// the pipeline has any.
func batchSends(bs []*analysis.Batching, pipeline string) (int, bool) {
	n, found := 0, false
	for _, b := range bs {
		if b.Pipeline != pipeline {
			continue
		}
		found = true
		n += len(b.Flushes)
	}
	return n, found
}
