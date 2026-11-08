package recommend

import (
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// benefitTotal is the total a recommendation's benefit is part of
// (Benefit.Of), or zero when the run does not show one.
func benefitTotal(in Input, rule string, rec Recommendation) int {
	switch rec.Benefit.Metric {
	case MetricRecordsWithoutEffect, MetricRecordsNotProcessed:
		return processorPasses(in, rec.Pipeline)
	case MetricEvaluationsWithoutEffect:
		if len(rec.Components) == 0 {
			return 0
		}
		id := strings.TrimPrefix(rec.Components[0], string(topology.KindProcessor)+"/")
		switch {
		case strings.HasPrefix(rule, "ottl."):
			return statementEvaluations(in.OTTL, id)
		case rule == "filter.condition_never_matched":
			return conditionEvaluations(in.Filtering, id)
		}
	}
	return 0
}

// processorPasses counts the records the processors of a pipeline received,
// each processor counting every record once: the work a record metric is
// part of. A batch processor's inputs have no diff, since it sends later
// and regrouped; its batching evidence counts the items of each arrival.
func processorPasses(in Input, pipeline string) int {
	if pipeline == "" {
		return 0
	}
	arrivals := map[string]int{}
	for _, b := range in.Batching {
		if b.Pipeline != pipeline {
			continue
		}
		for _, a := range b.Arrivals {
			arrivals[b.Processor] += a.Items
		}
	}
	n := 0
	for _, s := range in.Stages {
		if s.Kind != analysis.KindProcessor || s.Pipeline != pipeline {
			continue
		}
		if items, ok := arrivals[s.Component]; ok {
			n += items
			continue
		}
		for _, input := range s.Inputs {
			if input.Diff != nil {
				n += input.Diff.Before.Items
			}
		}
	}
	return n
}

// statementEvaluations counts the evaluations of every statement of a
// transform processor in the requests whose replay was verified, as the
// OTTL rules count theirs.
func statementEvaluations(reports []*ottldebug.Report, processor string) int {
	n := 0
	for _, r := range reports {
		if r.Processor != processor || r.Verification != ottldebug.VerificationReplayVerified {
			continue
		}
		for _, g := range r.Groups {
			if g.Status != ottldebug.StatusReplayed {
				continue
			}
			for _, s := range g.Statements {
				n += s.Matched + s.NotMatched
			}
		}
	}
	return n
}

// conditionEvaluations counts the evaluations of every condition of a filter
// processor in the verified requests, as conditionNeverMatched counts its
// own.
func conditionEvaluations(fs []*analysis.Filtering, processor string) int {
	n := 0
	for _, f := range fs {
		if f.Processor != processor || f.Verification != analysis.FilterVerified {
			continue
		}
		for _, c := range f.Conditions {
			n += c.Evaluated
		}
	}
	return n
}
