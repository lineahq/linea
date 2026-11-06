package engine

import (
	"context"

	"go.opentelemetry.io/collector/component"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// explainFiltering explains every filter processor input whose outcome is
// known: one correlated output, or a completed run with no output. Each
// drop names the records the lineage followed into the processor.
func explainFiltering(ctx context.Context, resolved *config.Resolved, stages []analysis.Stage, lineage []analysis.RecordLineage, result *runresult.Result) ([]*analysis.Filtering, error) {
	out := []*analysis.Filtering{}
	explain := features.Filter()
	if explain == nil {
		return out, nil // The runtime lacks the feature; its report says so.
	}
	bySeq := map[int]instrument.Event{}
	for _, e := range result.Events {
		bySeq[e.Seq] = e
	}
	for _, s := range stages {
		if s.Kind != analysis.KindProcessor || s.Type != "filter" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		cfg := resolved.Config.Processors[id]
		for _, in := range s.Inputs {
			if in.Outcome != analysis.OutcomeEmitted && in.Outcome != analysis.OutcomeNoOutput {
				continue
			}
			if len(in.Outputs) > 1 {
				continue
			}
			payload, err := result.Snapshots.Payload(bySeq[in.Seq].Snapshot)
			if err != nil {
				return nil, err
			}
			var output *telemetry.Payload
			outSeq := 0
			if len(in.Outputs) == 1 {
				outSeq = in.Outputs[0]
				p, err := result.Snapshots.Payload(bySeq[outSeq].Snapshot)
				if err != nil {
					return nil, err
				}
				output = &p
			}
			f, err := explain(ctx, s.Component, cfg, in.Seq, payload, outSeq, output, in.Error)
			if err != nil {
				return nil, err
			}
			f.Pipeline = s.Pipeline
			analysis.NameDrops(f, lineage)
			out = append(out, f)
		}
	}
	return out, nil
}
