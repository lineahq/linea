package instrument

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/xconnector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pipeline"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Connector returns a factory with the same type, alias, default config and
// per-signal-pair stability as f. Each created connector is the real one
// from f. Its consumption is recorded as a stage.input and each delivery as
// a stage.output whose Pipeline is the pipeline it was delivered into.
//
// The Collector hands a connector a router (connector.XRouterAndConsumer)
// over the first consumer of each pipeline it feeds; routing connectors ask
// it for the consumers of the pipelines a route names. The decoration builds
// the same kind of router with connector.NewXRouter over recording consumers
// that wrap the real router's per-pipeline consumers, so fan-out (and its
// cloning of data for mutating consumers) is the Collector's own.
//
// Profiles are not decorated.
func Connector(f connector.Factory, rec *Recorder) connector.Factory {
	var opts []xconnector.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xconnector.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	undefined := component.StabilityLevelUndefined
	if sl := f.TracesToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Traces, error) {
			s := newConnectorStage(set, "traces", rec)
			return tracesIn(s)(f.CreateTracesToTraces(ctx, observeConnector(set, rec), cfg, tracesOut(s, next)))
		}, sl))
	}
	if sl := f.TracesToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Traces, error) {
			s := newConnectorStage(set, "traces", rec)
			return tracesIn(s)(f.CreateTracesToMetrics(ctx, observeConnector(set, rec), cfg, metricsOut(s, next)))
		}, sl))
	}
	if sl := f.TracesToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Traces, error) {
			s := newConnectorStage(set, "traces", rec)
			return tracesIn(s)(f.CreateTracesToLogs(ctx, observeConnector(set, rec), cfg, logsOut(s, next)))
		}, sl))
	}
	if sl := f.MetricsToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Metrics, error) {
			s := newConnectorStage(set, "metrics", rec)
			return metricsIn(s)(f.CreateMetricsToTraces(ctx, observeConnector(set, rec), cfg, tracesOut(s, next)))
		}, sl))
	}
	if sl := f.MetricsToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Metrics, error) {
			s := newConnectorStage(set, "metrics", rec)
			return metricsIn(s)(f.CreateMetricsToMetrics(ctx, observeConnector(set, rec), cfg, metricsOut(s, next)))
		}, sl))
	}
	if sl := f.MetricsToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Metrics, error) {
			s := newConnectorStage(set, "metrics", rec)
			return metricsIn(s)(f.CreateMetricsToLogs(ctx, observeConnector(set, rec), cfg, logsOut(s, next)))
		}, sl))
	}
	if sl := f.LogsToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Logs, error) {
			s := newConnectorStage(set, "logs", rec)
			return logsIn(s)(f.CreateLogsToTraces(ctx, observeConnector(set, rec), cfg, tracesOut(s, next)))
		}, sl))
	}
	if sl := f.LogsToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Logs, error) {
			s := newConnectorStage(set, "logs", rec)
			return logsIn(s)(f.CreateLogsToMetrics(ctx, observeConnector(set, rec), cfg, metricsOut(s, next)))
		}, sl))
	}
	if sl := f.LogsToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Logs, error) {
			s := newConnectorStage(set, "logs", rec)
			return logsIn(s)(f.CreateLogsToLogs(ctx, observeConnector(set, rec), cfg, logsOut(s, next)))
		}, sl))
	}
	return xconnector.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

// newConnectorStage names a connector instance. The Collector builds one per
// connector and signal pair, shared by every pipeline it joins, so it has no
// pipeline of its own.
func newConnectorStage(set connector.Settings, signal string, rec *Recorder) *stage {
	return &stage{name: set.ID.String(), signal: signal, source: SourceConnectorWrapper, rec: rec}
}

// The ingress side reuses the processor wrappers: connector.X and
// processor.X have the same method set.

func tracesIn(s *stage) func(connector.Traces, error) (connector.Traces, error) {
	return func(real connector.Traces, err error) (connector.Traces, error) {
		if err != nil {
			return nil, err
		}
		return &tracesProcessor{Traces: real, lifecycle: lifecycle{s}}, nil
	}
}

func metricsIn(s *stage) func(connector.Metrics, error) (connector.Metrics, error) {
	return func(real connector.Metrics, err error) (connector.Metrics, error) {
		if err != nil {
			return nil, err
		}
		return &metricsProcessor{Metrics: real, lifecycle: lifecycle{s}}, nil
	}
}

func logsIn(s *stage) func(connector.Logs, error) (connector.Logs, error) {
	return func(real connector.Logs, err error) (connector.Logs, error) {
		if err != nil {
			return nil, err
		}
		return &logsProcessor{Logs: real, lifecycle: lifecycle{s}}, nil
	}
}

// tracesOut wraps the connector's next consumer so that every delivery is
// recorded with the pipeline it enters.
func tracesOut(s *stage, next consumer.Traces) consumer.Traces {
	r, ok := next.(connector.TracesRouterAndConsumer)
	if !ok {
		return deliverTraces(s, "", next)
	}
	cm := map[pipeline.ID]consumer.Traces{}
	for _, id := range r.PipelineIDs() {
		c, err := r.Consumer(id)
		if err != nil {
			return deliverTraces(s, "", next)
		}
		cm[id] = deliverTraces(s, id.String(), c)
	}
	return connector.NewTracesRouter(cm)
}

func metricsOut(s *stage, next consumer.Metrics) consumer.Metrics {
	r, ok := next.(connector.MetricsRouterAndConsumer)
	if !ok {
		return deliverMetrics(s, "", next)
	}
	cm := map[pipeline.ID]consumer.Metrics{}
	for _, id := range r.PipelineIDs() {
		c, err := r.Consumer(id)
		if err != nil {
			return deliverMetrics(s, "", next)
		}
		cm[id] = deliverMetrics(s, id.String(), c)
	}
	return connector.NewMetricsRouter(cm)
}

func logsOut(s *stage, next consumer.Logs) consumer.Logs {
	r, ok := next.(connector.LogsRouterAndConsumer)
	if !ok {
		return deliverLogs(s, "", next)
	}
	cm := map[pipeline.ID]consumer.Logs{}
	for _, id := range r.PipelineIDs() {
		c, err := r.Consumer(id)
		if err != nil {
			return deliverLogs(s, "", next)
		}
		cm[id] = deliverLogs(s, id.String(), c)
	}
	return connector.NewLogsRouter(cm)
}

// The delivering consumers keep the capabilities of the consumer they wrap,
// so the router's fan-out clones exactly as it would without them.

func deliverTraces(s *stage, pipeline string, next consumer.Traces) consumer.Traces {
	c, _ := consumer.NewTraces(func(ctx context.Context, td ptrace.Traces) error {
		return s.egressTo(ctx, pipeline, telemetry.FromTraces(td), func(ctx context.Context) error { return next.ConsumeTraces(ctx, td) })
	}, consumer.WithCapabilities(next.Capabilities()))
	return c
}

func deliverMetrics(s *stage, pipeline string, next consumer.Metrics) consumer.Metrics {
	c, _ := consumer.NewMetrics(func(ctx context.Context, md pmetric.Metrics) error {
		return s.egressTo(ctx, pipeline, telemetry.FromMetrics(md), func(ctx context.Context) error { return next.ConsumeMetrics(ctx, md) })
	}, consumer.WithCapabilities(next.Capabilities()))
	return c
}

func deliverLogs(s *stage, pipeline string, next consumer.Logs) consumer.Logs {
	c, _ := consumer.NewLogs(func(ctx context.Context, ld plog.Logs) error {
		return s.egressTo(ctx, pipeline, telemetry.FromLogs(ld), func(ctx context.Context) error { return next.ConsumeLogs(ctx, ld) })
	}, consumer.WithCapabilities(next.Capabilities()))
	return c
}
