package engine

import (
	"encoding"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/runresult"
)

// explainBatching explains the flushes of every batching processor
// instance (batch and queue_batch). It reads the processor's settings by
// marshaling its configuration, so the engine imports no Collector
// component.
func explainBatching(resolved *config.Resolved, stages []analysis.Stage, lineage []analysis.RecordLineage, result *runresult.Result, inputs []runresult.Input) ([]*analysis.Batching, error) {
	out := []*analysis.Batching{}
	batchers := map[string]bool{}
	for _, s := range stages {
		if s.Kind == analysis.KindProcessor && analysis.IsBatcher(s.Type) {
			batchers[s.Component] = true
		}
	}
	var headers map[int]map[string][]string
	dropped := false
	for _, s := range stages {
		if s.Kind != analysis.KindProcessor || !analysis.IsBatcher(s.Type) {
			continue
		}
		var id component.ID
		if err := id.UnmarshalText([]byte(s.Component)); err != nil {
			return nil, err
		}
		read := batchSettings
		if s.Type == analysis.TypeQueueBatch {
			read = queueBatchSettings
		}
		settings, timeout, err := read(resolved.Config.Processors[id])
		if err != nil {
			return nil, fmt.Errorf("%s processor %s: %w", s.Type, s.Component, err)
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

// queueBatchSettings reads the queue_batch processor's effective settings
// (queuebatchprocessor v0.165.0, exporterhelper's queuebatch.Config) in the
// batch processor's terms: batch::min_size, max_size, flush_timeout,
// sizer and partition. Without a batch section the processor queues and
// sends every request as it is.
func queueBatchSettings(cfg component.Config) (analysis.BatchSettings, time.Duration, error) {
	s := analysis.BatchSettings{Type: analysis.TypeQueueBatch}
	if cfg == nil {
		return s, 0, fmt.Errorf("no configuration")
	}
	conf := confmap.New()
	if err := conf.Marshal(cfg); err != nil {
		return s, 0, err
	}
	batch, err := conf.Sub("batch")
	if err != nil {
		return s, 0, err
	}
	if !conf.IsSet("batch") || len(batch.AllKeys()) == 0 {
		s.Unbatched = true
		s.Timeout = time.Duration(0).String()
		return s, 0, nil
	}
	s.SendBatchSize = intOf(batch.Get("min_size"))
	s.SendBatchMaxSize = intOf(batch.Get("max_size"))
	timeout, err := durationOf(batch.Get("flush_timeout"))
	if err != nil {
		return s, 0, err
	}
	s.Timeout = timeout.String()
	s.Sizer = textOf(batch.Get("sizer"))
	if s.Sizer == "" {
		s.Sizer = batchSizer(cfg)
	}
	if keys, ok := batch.Get("partition::metadata_keys").([]any); ok {
		for _, k := range keys {
			if v, ok := k.(string); ok {
				s.MetadataKeys = append(s.MetadataKeys, v)
			}
		}
	}
	s.CacheSize = intOf(batch.Get("partition::cache_size"))
	return s, timeout, nil
}

// textOf is a marshaled text value, such as a sizer type, as a string.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case encoding.TextMarshaler:
		b, err := t.MarshalText()
		if err == nil {
			return string(b)
		}
	case fmt.Stringer:
		return t.String()
	}
	return ""
}

// batchSizer reads the sizer of the batch section at path (by mapstructure
// names, default "batch") from the configuration struct: the sizer type
// marshals its text only through a pointer, so confmap writes an empty map
// for it. It returns "" when the struct has no such section.
func batchSizer(cfg component.Config, path ...string) string {
	if len(path) == 0 {
		path = []string{"batch"}
	}
	v := reflect.ValueOf(cfg)
	for _, tag := range path {
		if v = optionalValue(fieldByTag(v, tag)); !v.IsValid() {
			return ""
		}
	}
	sizer := fieldByTag(v, "sizer")
	if !sizer.IsValid() {
		return ""
	}
	sp := reflect.New(sizer.Type())
	sp.Elem().Set(sizer)
	if s, ok := sp.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	return ""
}

// fieldByTag is the field of struct v (through pointers and interfaces)
// whose mapstructure tag names it; invalid when there is none.
func fieldByTag(v reflect.Value, tag string) reflect.Value {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	for i := range v.NumField() {
		name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("mapstructure"), ",")
		if name == tag {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

// optionalValue is the value a configoptional.Optional field holds, or v
// itself when it is not an Optional; invalid when the Optional is unset.
func optionalValue(v reflect.Value) reflect.Value {
	if !v.IsValid() {
		return v
	}
	opt := reflect.New(v.Type())
	opt.Elem().Set(v)
	get := opt.MethodByName("Get")
	if !get.IsValid() || get.Type().NumIn() != 0 || get.Type().NumOut() != 1 {
		return v
	}
	p := get.Call(nil)[0]
	if p.Kind() != reflect.Pointer || p.IsNil() {
		return reflect.Value{}
	}
	return p.Elem()
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
