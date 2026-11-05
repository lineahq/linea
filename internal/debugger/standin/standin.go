// Package standin supplies factories for component types the debugger
// distribution does not contain, so a configuration that uses them still
// resolves and runs. A stand-in decodes any settings without
// interpreting them and does the least that keeps the pipeline intact:
// receivers and exporters do nothing (the runtime replays into receivers and
// captures exporters anyway), processors pass data through, connectors
// forward data of their own signal and emit nothing for another signal, and
// extensions are inert. It imports nothing from this repository.
package standin

import (
	"context"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"
)

// stability is declared by every stand-in factory; the Collector requires a
// defined level for each signal a factory supports.
const stability = component.StabilityLevelDevelopment

// Redacted replaces every value of a stand-in's settings in the effective
// configuration: without the component's settings types, nothing tells a
// secret from a plain value.
const Redacted = "[REDACTED]"

// Config holds a stand-in's settings as decoded, untyped. The field is
// unexported, so the Collector's validation walk does not look inside.
type Config struct {
	settings map[string]any
}

// Unmarshal keeps the settings as given; a stand-in accepts any keys.
func (c *Config) Unmarshal(conf *confmap.Conf) error {
	c.settings = conf.ToStringMap()
	return nil
}

// Marshal writes the settings' keys with every value redacted. It has a
// value receiver: the Collector's marshal hook looks for Marshaler on struct
// values.
func (c Config) Marshal(conf *confmap.Conf) error {
	if len(c.settings) == 0 {
		return nil
	}
	return conf.Merge(confmap.NewFromStringMap(redact(c.settings).(map[string]any)))
}

func redact(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = redact(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = redact(e)
		}
		return out
	case nil:
		return nil
	}
	return Redacted
}

// Is reports whether cfg is a stand-in's configuration.
func Is(cfg component.Config) bool {
	_, ok := cfg.(*Config)
	return ok
}

func newConfig() component.Config { return &Config{} }

// Missing lists, per kind, the configured types the factories lack.
type Missing struct {
	Receivers, Processors, Exporters, Connectors, Extensions []component.Type
}

// Empty reports whether nothing is missing.
func (m Missing) Empty() bool {
	return len(m.Receivers)+len(m.Processors)+len(m.Exporters)+len(m.Connectors)+len(m.Extensions) == 0
}

// Find lists the types of the IDs per section that f lacks. ids maps a
// section key ("receivers", "processors", "exporters", "connectors",
// "extensions") to its configured IDs.
func Find(f otelcol.Factories, ids map[string][]component.ID) Missing {
	return Missing{
		Receivers:  missing(f.Receivers, ids["receivers"]),
		Processors: missing(f.Processors, ids["processors"]),
		Exporters:  missing(f.Exporters, ids["exporters"]),
		Connectors: missing(f.Connectors, ids["connectors"]),
		Extensions: missing(f.Extensions, ids["extensions"]),
	}
}

// FromConfig lists the types of a resolved configuration that f lacks.
func FromConfig(f otelcol.Factories, cfg *otelcol.Config) Missing {
	return Find(f, map[string][]component.ID{
		"receivers":  slices.Collect(maps.Keys(cfg.Receivers)),
		"processors": slices.Collect(maps.Keys(cfg.Processors)),
		"exporters":  slices.Collect(maps.Keys(cfg.Exporters)),
		"connectors": slices.Collect(maps.Keys(cfg.Connectors)),
		"extensions": slices.Collect(maps.Keys(cfg.Extensions)),
	})
}

func missing[F any](have map[component.Type]F, ids []component.ID) []component.Type {
	var out []component.Type
	for _, id := range ids {
		if _, ok := have[id.Type()]; !ok && !slices.Contains(out, id.Type()) {
			out = append(out, id.Type())
		}
	}
	slices.SortFunc(out, func(a, b component.Type) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// Add returns f with a stand-in factory for every missing type. The maps of
// f are copied, never changed.
func Add(f otelcol.Factories, m Missing) otelcol.Factories {
	if m.Empty() {
		return f
	}
	out := f
	out.Receivers = maps.Clone(f.Receivers)
	for _, t := range m.Receivers {
		out.Receivers[t] = Receiver(t)
	}
	out.Processors = maps.Clone(f.Processors)
	for _, t := range m.Processors {
		out.Processors[t] = Processor(t)
	}
	out.Exporters = maps.Clone(f.Exporters)
	for _, t := range m.Exporters {
		out.Exporters[t] = Exporter(t)
	}
	out.Connectors = maps.Clone(f.Connectors)
	for _, t := range m.Connectors {
		out.Connectors[t] = Connector(t)
	}
	out.Extensions = maps.Clone(f.Extensions)
	for _, t := range m.Extensions {
		out.Extensions[t] = Extension(t)
	}
	return out
}

type lifecycle struct {
	component.StartFunc
	component.ShutdownFunc
}

// Receiver is a stand-in receiver for every signal: it receives nothing.
func Receiver(t component.Type) receiver.Factory {
	return receiver.NewFactory(t, newConfig,
		receiver.WithTraces(func(context.Context, receiver.Settings, component.Config, consumer.Traces) (receiver.Traces, error) {
			return lifecycle{}, nil
		}, stability),
		receiver.WithMetrics(func(context.Context, receiver.Settings, component.Config, consumer.Metrics) (receiver.Metrics, error) {
			return lifecycle{}, nil
		}, stability),
		receiver.WithLogs(func(context.Context, receiver.Settings, component.Config, consumer.Logs) (receiver.Logs, error) {
			return lifecycle{}, nil
		}, stability))
}

type passTraces struct {
	lifecycle
	consumer.Traces
}

type passMetrics struct {
	lifecycle
	consumer.Metrics
}

type passLogs struct {
	lifecycle
	consumer.Logs
}

// Processor is a stand-in processor for every signal: it passes data on
// unchanged.
func Processor(t component.Type) processor.Factory {
	return processor.NewFactory(t, newConfig,
		processor.WithTraces(func(_ context.Context, _ processor.Settings, _ component.Config, next consumer.Traces) (processor.Traces, error) {
			return passTraces{Traces: next}, nil
		}, stability),
		processor.WithMetrics(func(_ context.Context, _ processor.Settings, _ component.Config, next consumer.Metrics) (processor.Metrics, error) {
			return passMetrics{Metrics: next}, nil
		}, stability),
		processor.WithLogs(func(_ context.Context, _ processor.Settings, _ component.Config, next consumer.Logs) (processor.Logs, error) {
			return passLogs{Logs: next}, nil
		}, stability))
}

// sink accepts data and emits nothing: a connector between two signals
// cannot know what the real one would derive.
type sink struct {
	lifecycle
}

func (sink) Capabilities() consumer.Capabilities                   { return consumer.Capabilities{} }
func (sink) ConsumeTraces(context.Context, ptrace.Traces) error    { return nil }
func (sink) ConsumeMetrics(context.Context, pmetric.Metrics) error { return nil }
func (sink) ConsumeLogs(context.Context, plog.Logs) error          { return nil }

// Connector is a stand-in connector for every pair of signals: it forwards
// data to a pipeline of its own signal and emits nothing into another.
func Connector(t component.Type) connector.Factory {
	return connector.NewFactory(t, newConfig,
		connector.WithTracesToTraces(func(_ context.Context, _ connector.Settings, _ component.Config, next consumer.Traces) (connector.Traces, error) {
			return passTraces{Traces: next}, nil
		}, stability),
		connector.WithMetricsToMetrics(func(_ context.Context, _ connector.Settings, _ component.Config, next consumer.Metrics) (connector.Metrics, error) {
			return passMetrics{Metrics: next}, nil
		}, stability),
		connector.WithLogsToLogs(func(_ context.Context, _ connector.Settings, _ component.Config, next consumer.Logs) (connector.Logs, error) {
			return passLogs{Logs: next}, nil
		}, stability),
		connector.WithTracesToMetrics(func(context.Context, connector.Settings, component.Config, consumer.Metrics) (connector.Traces, error) {
			return sink{}, nil
		}, stability),
		connector.WithTracesToLogs(func(context.Context, connector.Settings, component.Config, consumer.Logs) (connector.Traces, error) {
			return sink{}, nil
		}, stability),
		connector.WithMetricsToTraces(func(context.Context, connector.Settings, component.Config, consumer.Traces) (connector.Metrics, error) {
			return sink{}, nil
		}, stability),
		connector.WithMetricsToLogs(func(context.Context, connector.Settings, component.Config, consumer.Logs) (connector.Metrics, error) {
			return sink{}, nil
		}, stability),
		connector.WithLogsToTraces(func(context.Context, connector.Settings, component.Config, consumer.Traces) (connector.Logs, error) {
			return sink{}, nil
		}, stability),
		connector.WithLogsToMetrics(func(context.Context, connector.Settings, component.Config, consumer.Metrics) (connector.Logs, error) {
			return sink{}, nil
		}, stability))
}

// Exporter is a stand-in exporter for every signal: it accepts data and
// sends nothing.
func Exporter(t component.Type) exporter.Factory {
	return exporter.NewFactory(t, newConfig,
		exporter.WithTraces(func(context.Context, exporter.Settings, component.Config) (exporter.Traces, error) {
			return sink{}, nil
		}, stability),
		exporter.WithMetrics(func(context.Context, exporter.Settings, component.Config) (exporter.Metrics, error) {
			return sink{}, nil
		}, stability),
		exporter.WithLogs(func(context.Context, exporter.Settings, component.Config) (exporter.Logs, error) {
			return sink{}, nil
		}, stability))
}

// Extension is a stand-in extension: inert, like every extension in a run.
func Extension(t component.Type) extension.Factory {
	return extension.NewFactory(t, newConfig,
		func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
			return lifecycle{}, nil
		}, stability)
}
