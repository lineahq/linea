package engine

import (
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
)

// explainBatching explains the flushes of every batch processor instance.
// It reads the processor's settings by marshaling its configuration, so the
// engine imports no Collector component.
func explainBatching(resolved *config.Resolved, stages []analysis.Stage, lineage []analysis.RecordLineage, result *runresult.Result, inputs []runresult.Input) ([]*analysis.Batching, error) {
	out := []*analysis.Batching{}
	batchers := map[string]bool{}
	for _, s := range stages {
		if s.Kind == analysis.KindProcessor && s.Type == "batch" {
			batchers[s.Component] = true
		}
	}
	var headers map[int]map[string][]string
	dropped := false
	for _, s := range stages {
		if s.Kind != analysis.KindProcessor || s.Type != "batch" {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		settings, timeout, err := batchSettings(resolved.Config.Processors[id])
		if err != nil {
			return nil, fmt.Errorf("batch processor %s: %w", s.Component, err)
		}
		if len(settings.MetadataKeys) > 0 && headers == nil {
			headers, dropped = inputHeaders(result, inputs)
		}
		b, err := analysis.Batch(analysis.BatchInput{
			Processor: s.Component, Pipeline: s.Pipeline, Signal: s.Signal,
			Settings: settings, Timeout: timeout,
			Events: result.Events, Snapshots: result.Snapshots, Lineage: lineage,
			Metrics: result.ComponentMetrics, Completed: result.Completed(),
			InputHeaders: headers, BatchProcessors: batchers,
		})
		if err != nil {
			return nil, err
		}
		if len(settings.MetadataKeys) > 0 && dropped {
			b.Notes = append(b.Notes, "some inputs declare headers their receiver drops: without include_metadata: true it hands no headers to the pipeline, so their requests share the batcher without metadata")
		}
		out = append(out, b)
	}
	return out, nil
}

// inputHeaders maps each injected scenario input's input.injected seq to
// the headers the receiver handed on, and reports whether a receiver
// dropped declared headers. Inputs are injected in order, one event each.
func inputHeaders(result *runresult.Result, inputs []runresult.Input) (map[int]map[string][]string, bool) {
	out := map[int]map[string][]string{}
	dropped := false
	i := 0
	for _, e := range result.Events {
		if e.Kind != instrument.KindInput || e.Source != instrument.SourceReplayReceiver {
			continue
		}
		for i < len(result.Inputs) && i < len(inputs) && !result.Inputs[i].Injected {
			i++
		}
		if i >= len(result.Inputs) || i >= len(inputs) {
			break
		}
		if result.Inputs[i].HeadersDropped {
			dropped = dropped || len(inputs[i].Headers) > 0
		} else if len(inputs[i].Headers) > 0 {
			out[e.Seq] = inputs[i].Headers
		}
		i++
	}
	return out, dropped
}

// batchSettings reads the batch processor's effective settings
// (batchprocessor v0.162.0 field names).
func batchSettings(cfg component.Config) (analysis.BatchSettings, time.Duration, error) {
	var s analysis.BatchSettings
	if cfg == nil {
		return s, 0, fmt.Errorf("no configuration")
	}
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return s, 0, err
	}
	s.SendBatchSize = intOf(conf.Get("send_batch_size"))
	s.SendBatchMaxSize = intOf(conf.Get("send_batch_max_size"))
	timeout, err := durationOf(conf.Get("timeout"))
	if err != nil {
		return s, 0, err
	}
	s.Timeout = timeout.String()
	if keys, ok := conf.Get("metadata_keys").([]any); ok {
		for _, k := range keys {
			if v, ok := k.(string); ok {
				s.MetadataKeys = append(s.MetadataKeys, v)
			}
		}
	}
	s.MetadataCardinalityLimit = intOf(conf.Get("metadata_cardinality_limit"))
	return s, timeout, nil
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint32:
		return int(n)
	case uint64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func durationOf(v any) (time.Duration, error) {
	switch d := v.(type) {
	case nil:
		return 0, nil
	case time.Duration:
		return d, nil
	case string:
		return time.ParseDuration(d)
	case int64:
		return time.Duration(d), nil
	case int:
		return time.Duration(d), nil
	}
	return 0, fmt.Errorf("timeout has type %T", v)
}
