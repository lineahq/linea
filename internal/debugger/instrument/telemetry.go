package instrument

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/xreceiver"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Component kinds as events and component metrics name them; they match
// topology.Kind values.
const (
	ComponentReceiver  = "receiver"
	ComponentProcessor = "processor"
	ComponentConnector = "connector"
	ComponentExtension = "extension"
)

// logLevel is the least level of a component's log entries the run
// records: debug entries are left to the component's own logger, so a
// chatty component cannot spend the evidence budget its telemetry needs.
const logLevel = zapcore.InfoLevel

// maxLogText bounds a recorded message or field value, in bytes.
const maxLogText = 4096

// LogEntry is one entry a component logged.
type LogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
	// Fields are the entry's fields as strings: text as it is, other
	// values as JSON. The Collector's component attributes are left out.
	Fields map[string]string `json:"fields,omitempty"`
}

// ComponentMetric is one data point of a metric a component recorded
// through its own meter provider, read when the run ended. Exactly
// one of IntValue and Value is set for a sum or gauge; a histogram has
// Count and Sum.
type ComponentMetric struct {
	ComponentKind string `json:"component_kind"`
	Component     string `json:"component"`
	Pipeline      string `json:"pipeline,omitempty"`
	Name          string `json:"name"`
	Unit          string `json:"unit,omitempty"`
	// Type is "sum", "gauge" or "histogram".
	Type       string            `json:"type"`
	Attributes map[string]string `json:"attributes,omitempty"`
	IntValue   *int64            `json:"int_value,omitempty"`
	Value      *float64          `json:"value,omitempty"`
	Count      *uint64           `json:"count,omitempty"`
	Sum        *float64          `json:"sum,omitempty"`
}

// observed is the identity of a component instance whose own telemetry is
// recorded.
type observed struct {
	kind, component, pipeline, signal string
}

// componentTelemetry is what a recorder keeps of components' own
// telemetry: a meter provider and reader per component instance.
type componentTelemetry struct {
	mu        sync.Mutex
	providers map[observed]*meter
	order     []observed
}

type meter struct {
	provider *sdkmetric.MeterProvider
	reader   *sdkmetric.ManualReader
}

// observe returns set with a logger that also records the component's
// entries as component.log events and a meter provider of its own, one
// per component instance (a component created for several signals shares
// it, as it shares the Collector's).
func (r *Recorder) observe(o observed, set component.TelemetrySettings) component.TelemetrySettings {
	r.telemetry.mu.Lock()
	m, ok := r.telemetry.providers[o]
	if !ok {
		reader := sdkmetric.NewManualReader()
		m = &meter{provider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), reader: reader}
		if r.telemetry.providers == nil {
			r.telemetry.providers = map[observed]*meter{}
		}
		r.telemetry.providers[o] = m
		r.telemetry.order = append(r.telemetry.order, o)
	}
	r.telemetry.mu.Unlock()

	set.MeterProvider = m.provider
	if set.Logger != nil {
		core := &logCore{rec: r, at: o}
		set.Logger = set.Logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			return zapcore.NewTee(c, core)
		}))
	}
	return set
}

// ComponentMetrics reads every component's meter provider and shuts it
// down: call it once the Collector has stopped. Metrics are ordered by
// component kind, component, pipeline, name and attributes.
func (r *Recorder) ComponentMetrics(ctx context.Context) []ComponentMetric {
	r.telemetry.mu.Lock()
	providers, order := r.telemetry.providers, r.telemetry.order
	r.telemetry.providers, r.telemetry.order = nil, nil
	r.telemetry.mu.Unlock()

	var out []ComponentMetric
	for _, o := range order {
		m := providers[o]
		var rm metricdata.ResourceMetrics
		if err := m.reader.Collect(ctx, &rm); err == nil {
			for _, sm := range rm.ScopeMetrics {
				for _, metric := range sm.Metrics {
					out = append(out, points(o, metric)...)
				}
			}
		}
		_ = m.provider.Shutdown(ctx)
	}
	slices.SortStableFunc(out, func(a, b ComponentMetric) int {
		return cmp.Or(cmp.Compare(a.ComponentKind, b.ComponentKind), cmp.Compare(a.Component, b.Component),
			cmp.Compare(a.Pipeline, b.Pipeline), cmp.Compare(a.Name, b.Name),
			cmp.Compare(attributeKey(a.Attributes), attributeKey(b.Attributes)))
	})
	return out
}

func attributeKey(attrs map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		b.WriteString(k + "=" + attrs[k] + "\x00")
	}
	return b.String()
}

// points flattens a metric into one ComponentMetric per data point.
func points(o observed, m metricdata.Metrics) []ComponentMetric {
	base := ComponentMetric{ComponentKind: o.kind, Component: o.component, Pipeline: o.pipeline, Name: m.Name, Unit: m.Unit}
	var out []ComponentMetric
	add := func(typ string, attrs attribute.Set, set func(*ComponentMetric)) {
		p := base
		p.Type = typ
		p.Attributes = attributes(attrs)
		set(&p)
		out = append(out, p)
	}
	switch d := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, dp := range d.DataPoints {
			add("sum", dp.Attributes, func(p *ComponentMetric) { p.IntValue = &dp.Value })
		}
	case metricdata.Sum[float64]:
		for _, dp := range d.DataPoints {
			add("sum", dp.Attributes, func(p *ComponentMetric) { p.Value = &dp.Value })
		}
	case metricdata.Gauge[int64]:
		for _, dp := range d.DataPoints {
			add("gauge", dp.Attributes, func(p *ComponentMetric) { p.IntValue = &dp.Value })
		}
	case metricdata.Gauge[float64]:
		for _, dp := range d.DataPoints {
			add("gauge", dp.Attributes, func(p *ComponentMetric) { p.Value = &dp.Value })
		}
	case metricdata.Histogram[int64]:
		for _, dp := range d.DataPoints {
			sum := float64(dp.Sum)
			add("histogram", dp.Attributes, func(p *ComponentMetric) { p.Count, p.Sum = &dp.Count, &sum })
		}
	case metricdata.Histogram[float64]:
		for _, dp := range d.DataPoints {
			add("histogram", dp.Attributes, func(p *ComponentMetric) { p.Count, p.Sum = &dp.Count, &dp.Sum })
		}
	case metricdata.ExponentialHistogram[int64]:
		for _, dp := range d.DataPoints {
			sum := float64(dp.Sum)
			add("histogram", dp.Attributes, func(p *ComponentMetric) { p.Count, p.Sum = &dp.Count, &sum })
		}
	case metricdata.ExponentialHistogram[float64]:
		for _, dp := range d.DataPoints {
			add("histogram", dp.Attributes, func(p *ComponentMetric) { p.Count, p.Sum = &dp.Count, &dp.Sum })
		}
	}
	return out
}

func attributes(s attribute.Set) map[string]string {
	if s.Len() == 0 {
		return nil
	}
	out := make(map[string]string, s.Len())
	for _, kv := range s.ToSlice() {
		out[string(kv.Key)] = kv.Value.String()
	}
	return out
}

// logCore records a component's log entries as component.log events. It
// is teed beside the component's own core, so the entries still reach the
// Collector's logger.
type logCore struct {
	rec    *Recorder
	at     observed
	fields []zapcore.Field
}

func (c *logCore) Enabled(l zapcore.Level) bool { return l >= logLevel }

func (c *logCore) With(fields []zapcore.Field) zapcore.Core {
	return &logCore{rec: c.rec, at: c.at, fields: append(slices.Clip(c.fields), fields...)}
}

func (c *logCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(e.Level) && e.Message != probeMessage {
		return ce.AddCore(e, c)
	}
	return ce
}

func (c *logCore) Write(e zapcore.Entry, fields []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range c.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}
	entry := &LogEntry{Level: e.Level.String(), Message: clip(e.Message)}
	for k, v := range enc.Fields {
		if strings.HasPrefix(k, "otelcol.") || k == probeKey {
			continue
		}
		if entry.Fields == nil {
			entry.Fields = map[string]string{}
		}
		entry.Fields[k] = clip(text(v))
	}
	c.rec.Record(Event{
		Kind: KindComponentLog, Component: c.at.component, ComponentKind: c.at.kind,
		Pipeline: c.at.pipeline, Signal: c.at.signal, Mode: ModeReal, Source: SourceComponentTelemetry, Log: entry,
	})
	return nil
}

func (c *logCore) Sync() error { return nil }

func text(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	if data, err := json.Marshal(v); err == nil {
		return string(data)
	}
	return fmt.Sprint(v)
}

func clip(s string) string {
	if len(s) <= maxLogText {
		return s
	}
	cut := maxLogText
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// Receiver returns f with each created receiver's own logs and metrics
// recorded; the receiver itself is f's. It is for receivers that
// run for real, such as a mocked filelog.
func Receiver(f receiver.Factory, rec *Recorder) receiver.Factory {
	set := func(s receiver.Settings) receiver.Settings {
		s.TelemetrySettings = rec.observe(observed{kind: ComponentReceiver, component: s.ID.String()}, s.TelemetrySettings)
		return s
	}
	var opts []xreceiver.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xreceiver.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	if sl := f.TracesStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithTraces(func(ctx context.Context, s receiver.Settings, cfg component.Config, next consumer.Traces) (receiver.Traces, error) {
			return f.CreateTraces(ctx, set(s), cfg, next)
		}, sl))
	}
	if sl := f.MetricsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithMetrics(func(ctx context.Context, s receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
			return f.CreateMetrics(ctx, set(s), cfg, next)
		}, sl))
	}
	if sl := f.LogsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithLogs(func(ctx context.Context, s receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
			return f.CreateLogs(ctx, set(s), cfg, next)
		}, sl))
	}
	return xreceiver.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

// Extension returns f with each created extension's own logs and metrics
// recorded. The extension itself is f's, unwrapped: components find
// extensions by their concrete type.
func Extension(f extension.Factory, rec *Recorder) extension.Factory {
	return extension.NewFactory(f.Type(), f.CreateDefaultConfig,
		func(ctx context.Context, s extension.Settings, cfg component.Config) (extension.Extension, error) {
			s.TelemetrySettings = rec.observe(observed{kind: ComponentExtension, component: s.ID.String()}, s.TelemetrySettings)
			return f.Create(ctx, s, cfg)
		}, f.Stability())
}

// observe hands the processor its own logger and meter provider.
func (s *stage) observe(set processor.Settings) processor.Settings {
	set.TelemetrySettings = s.rec.observe(observed{kind: ComponentProcessor, component: s.name, pipeline: s.pipeline, signal: s.signal}, set.TelemetrySettings)
	return set
}

// observeConnector hands the connector its own logger and meter provider,
// one for every signal pair it is created for.
func observeConnector(set connector.Settings, rec *Recorder) connector.Settings {
	set.TelemetrySettings = rec.observe(observed{kind: ComponentConnector, component: set.ID.String()}, set.TelemetrySettings)
	return set
}
