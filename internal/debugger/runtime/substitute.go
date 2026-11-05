package runtime

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/xexporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/xreceiver"
	"go.opentelemetry.io/collector/service/telemetry"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/instrument"
	tel "github.com/lineahq/linea/internal/debugger/telemetry"
)

// aliasProvider is implemented by factories registered under a deprecated
// type alias (for example the OTLP exporter, type otlp_grpc, alias otlp).
type aliasProvider interface{ DeprecatedAlias() component.Type }

func deprecatedAlias(f component.Factory) (component.Type, bool) {
	ap, ok := f.(aliasProvider)
	if !ok || ap.DeprecatedAlias().String() == "" {
		return component.Type{}, false
	}
	return ap.DeprecatedAlias(), true
}

// isolatedTelemetry keeps the real internal-telemetry config schema, so the
// user's service::telemetry section is still unmarshalled and validated, but
// emits nothing: no Prometheus listener and no OTLP export of internal
// telemetry. Collector logs go to logger.
func isolatedTelemetry(logger *zap.Logger) telemetry.Factory {
	return telemetry.NewFactory(
		otelconftelemetry.NewFactory().CreateDefaultConfig,
		telemetry.WithCreateLogger(func(context.Context, telemetry.LoggerSettings, component.Config) (*zap.Logger, component.ShutdownFunc, error) {
			return logger, nil, nil
		}),
	)
}

type receiverKey struct {
	id     component.ID
	signal pipeline.Signal
}

// replay holds the next consumers of substituted receivers so a fixture can be
// injected where the receiver would have delivered decoded network data.
type replay struct {
	mu        sync.Mutex
	consumers map[receiverKey]any
}

func newReplay() *replay { return &replay{consumers: map[receiverKey]any{}} }

func (r *replay) register(id component.ID, signal pipeline.Signal, next any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumers[receiverKey{id, signal}] = next
}

func (r *replay) inject(ctx context.Context, id component.ID, p tel.Payload) error {
	r.mu.Lock()
	next, ok := r.consumers[receiverKey{id, p.Signal}]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("receiver %s has no %s pipeline", id, p.Signal)
	}
	switch p.Signal {
	case pipeline.SignalTraces:
		return next.(consumer.Traces).ConsumeTraces(ctx, p.Traces)
	case pipeline.SignalMetrics:
		return next.(consumer.Metrics).ConsumeMetrics(ctx, p.Metrics)
	case pipeline.SignalLogs:
		return next.(consumer.Logs).ConsumeLogs(ctx, p.Logs)
	}
	return fmt.Errorf("unsupported signal %s", p.Signal)
}

// replayReceiver is a stand-in that opens no listener. Its config is still
// the real receiver's, so validation is unchanged.
type replayReceiver struct {
	component.StartFunc
	component.ShutdownFunc
}

// replayReceiverFactory substitutes f with a receiver that accepts injected
// fixtures instead of network input.
func replayReceiverFactory(f receiver.Factory, r *replay) receiver.Factory {
	var opts []xreceiver.FactoryOption
	if alias, ok := deprecatedAlias(f); ok {
		opts = append(opts, xreceiver.WithDeprecatedTypeAlias(alias))
	}
	if sl := f.TracesStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithTraces(func(_ context.Context, set receiver.Settings, _ component.Config, next consumer.Traces) (receiver.Traces, error) {
			r.register(set.ID, pipeline.SignalTraces, next)
			return replayReceiver{}, nil
		}, sl))
	}
	if sl := f.MetricsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithMetrics(func(_ context.Context, set receiver.Settings, _ component.Config, next consumer.Metrics) (receiver.Metrics, error) {
			r.register(set.ID, pipeline.SignalMetrics, next)
			return replayReceiver{}, nil
		}, sl))
	}
	if sl := f.LogsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xreceiver.WithLogs(func(_ context.Context, set receiver.Settings, _ component.Config, next consumer.Logs) (receiver.Logs, error) {
			r.register(set.ID, pipeline.SignalLogs, next)
			return replayReceiver{}, nil
		}, sl))
	}
	return xreceiver.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

// capture stores everything that reached each substituted exporter: the
// counts of every batch, and its telemetry while the budget keeps it.
type capture struct {
	rec     *instrument.Recorder
	budget  *instrument.Budget
	mu      sync.Mutex
	exports map[string][]captured
}

func newCapture(rec *instrument.Recorder, budget *instrument.Budget) *capture {
	return &capture{rec: rec, budget: budget, exports: map[string][]captured{}}
}

func (c *capture) consume(ctx context.Context, id component.ID, p tel.Payload) {
	c.record(ctx, id, p, instrument.ModeCaptured)
}

// observeLocal records what reached a local exporter, which then runs for
// real (observedExporterFactory).
func (c *capture) observeLocal(ctx context.Context, id component.ID, p tel.Payload) {
	c.record(ctx, id, p, instrument.ModeReal)
}

func (c *capture) record(ctx context.Context, id component.ID, p tel.Payload, mode string) {
	name := id.String()
	c.rec.RecordTelemetry(instrument.Event{
		Kind:      instrument.KindExport,
		Component: name,
		Mode:      mode,
		Source:    instrument.SourceCaptureExporter,
		FromSeq:   instrument.EmissionOf(ctx),
	}, p)
	entry := keep(c.budget, p)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exports[name] = append(c.exports[name], entry)
}

func (c *capture) snapshot() map[string][]captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]captured, len(c.exports))
	for k, v := range c.exports {
		out[k] = append([]captured(nil), v...)
	}
	return out
}

type captureExporter struct {
	component.StartFunc
	component.ShutdownFunc
	id component.ID
	c  *capture
}

func (captureExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (e captureExporter) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	e.c.consume(ctx, e.id, tel.FromTraces(td))
	return nil
}

func (e captureExporter) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	e.c.consume(ctx, e.id, tel.FromMetrics(md))
	return nil
}

func (e captureExporter) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	e.c.consume(ctx, e.id, tel.FromLogs(ld))
	return nil
}

// captureExporterFactory substitutes f with an exporter that records
// telemetry locally and never connects anywhere. Its config is still the real
// exporter's, so validation is unchanged.
func captureExporterFactory(f exporter.Factory, c *capture) exporter.Factory {
	create := func(_ context.Context, set exporter.Settings, _ component.Config) (captureExporter, error) {
		return captureExporter{id: set.ID, c: c}, nil
	}
	var opts []xexporter.FactoryOption
	if alias, ok := deprecatedAlias(f); ok {
		opts = append(opts, xexporter.WithDeprecatedTypeAlias(alias))
	}
	if sl := f.TracesStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithTraces(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Traces, error) {
			return create(ctx, set, cfg)
		}, sl))
	}
	if sl := f.MetricsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithMetrics(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Metrics, error) {
			return create(ctx, set, cfg)
		}, sl))
	}
	if sl := f.LogsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithLogs(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Logs, error) {
			return create(ctx, set, cfg)
		}, sl))
	}
	return xexporter.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}
