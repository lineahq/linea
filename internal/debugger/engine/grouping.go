package engine

import (
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/runresult"
)

// explainGrouping explains every groupbyattrs processor instance. It reads
// the configured keys by marshaling the processor's configuration, so the
// engine imports no Collector component.
func explainGrouping(resolved *config.Resolved, stages []analysis.Stage, lineage []analysis.RecordLineage, result *runresult.Result) ([]*analysis.Grouping, error) {
	out := []*analysis.Grouping{}
	for _, s := range stages {
		if s.Kind != analysis.KindProcessor || s.Type != "groupbyattrs" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		keys, err := groupingKeys(resolved.Config.Processors[id])
		if err != nil {
			return nil, fmt.Errorf("groupbyattrs processor %s: %w", s.Component, err)
		}
		g, err := analysis.Group(analysis.GroupInput{Stage: s, Keys: keys, Events: result.Events, Snapshots: result.Snapshots, Lineage: lineage})
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// groupingKeys reads the processor's keys (groupbyattrsprocessor v0.162.0).
func groupingKeys(cfg component.Config) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no configuration")
	}
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return nil, err
	}
	var keys []string
	if list, ok := conf.Get("keys").([]any); ok {
		for _, k := range list {
			if v, ok := k.(string); ok {
				keys = append(keys, v)
			}
		}
	}
	return keys, nil
}
