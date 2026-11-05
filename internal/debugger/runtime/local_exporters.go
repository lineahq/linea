package runtime

import (
	"context"
	"maps"
	"slices"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/xexporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	tel "github.com/lineahq/linea/internal/debugger/telemetry"
)

// observeFunc receives what reached an exporter.
type observeFunc func(ctx context.Context, id component.ID, p tel.Payload)

// observedExporterFactory wraps a local exporter (debug, nop) so it runs
// unmodified and what reaches it is recorded first, as a substituted
// exporter's is: a configuration whose only exporter is debug still has
// outputs. The exporter never sends data off the host, so it keeps
// running in both the instrumented and the reference run.
func observedExporterFactory(f exporter.Factory, observe observeFunc) exporter.Factory {
	var opts []xexporter.FactoryOption
	if alias, ok := deprecatedAlias(f); ok {
		opts = append(opts, xexporter.WithDeprecatedTypeAlias(alias))
	}
	if sl := f.TracesStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithTraces(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Traces, error) {
			next, err := f.CreateTraces(ctx, set, cfg)
			if err != nil {
				return nil, err
			}
			return observedTraces{Traces: next, id: set.ID, observe: observe}, nil
		}, sl))
	}
	if sl := f.MetricsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithMetrics(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Metrics, error) {
			next, err := f.CreateMetrics(ctx, set, cfg)
			if err != nil {
				return nil, err
			}
			return observedMetrics{Metrics: next, id: set.ID, observe: observe}, nil
		}, sl))
	}
	if sl := f.LogsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xexporter.WithLogs(func(ctx context.Context, set exporter.Settings, cfg component.Config) (exporter.Logs, error) {
			next, err := f.CreateLogs(ctx, set, cfg)
			if err != nil {
				return nil, err
			}
			return observedLogs{Logs: next, id: set.ID, observe: observe}, nil
		}, sl))
	}
	return xexporter.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

// What reached the exporter is observed (and copied by the observer)
// before the exporter sees it.
type observedTraces struct {
	exporter.Traces
	id      component.ID
	observe observeFunc
}

func (e observedTraces) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	e.observe(ctx, e.id, tel.FromTraces(td))
	return e.Traces.ConsumeTraces(ctx, td)
}

type observedMetrics struct {
	exporter.Metrics
	id      component.ID
	observe observeFunc
}

func (e observedMetrics) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	e.observe(ctx, e.id, tel.FromMetrics(md))
	return e.Metrics.ConsumeMetrics(ctx, md)
}

type observedLogs struct {
	exporter.Logs
	id      component.ID
	observe observeFunc
}

func (e observedLogs) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	e.observe(ctx, e.id, tel.FromLogs(ld))
	return e.Logs.ConsumeLogs(ctx, ld)
}

var (
	_ consumer.Traces  = observedTraces{}
	_ consumer.Metrics = observedMetrics{}
	_ consumer.Logs    = observedLogs{}
)

// localSink keeps what reached the reference run's local exporters.
type localSink struct {
	mu  sync.Mutex
	got map[string][]tel.Payload
}

func newLocalSink() *localSink { return &localSink{got: map[string][]tel.Payload{}} }

func (s *localSink) observe(_ context.Context, id component.ID, p tel.Payload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got[id.String()] = append(s.got[id.String()], p.Clone())
}

// names lists the exporters that received anything, sorted.
func (s *localSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.got))
}

func (s *localSink) payloads(name string) []tel.Payload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got[name])
}
