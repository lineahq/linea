package mock

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/xconnector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/xprocessor"

	"github.com/lineahq/linea/internal/debugger/topology"
)

// Extension returns f creating each extension from its redirected
// configuration (Redirect). The extension itself is f's.
func (e *Endpoints) Extension(f extension.Factory) extension.Factory {
	return extension.NewFactory(f.Type(), f.CreateDefaultConfig,
		func(ctx context.Context, set extension.Settings, cfg component.Config) (extension.Extension, error) {
			c, err := e.Redirect(topology.KindExtension, set.ID, cfg)
			if err != nil {
				return nil, err
			}
			return f.Create(ctx, set, c)
		}, f.Stability())
}

// Processor returns f creating each processor from its redirected
// configuration. Profiles are not offered, as the instrumented processor
// offers none.
func (e *Endpoints) Processor(f processor.Factory) processor.Factory {
	var opts []xprocessor.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xprocessor.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	redirect := func(set processor.Settings, cfg component.Config) (component.Config, error) {
		return e.Redirect(topology.KindProcessor, set.ID, cfg)
	}
	undefined := component.StabilityLevelUndefined
	if sl := f.TracesStability(); sl != undefined {
		opts = append(opts, xprocessor.WithTraces(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateTraces(ctx, set, c, next)
		}, sl))
	}
	if sl := f.MetricsStability(); sl != undefined {
		opts = append(opts, xprocessor.WithMetrics(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateMetrics(ctx, set, c, next)
		}, sl))
	}
	if sl := f.LogsStability(); sl != undefined {
		opts = append(opts, xprocessor.WithLogs(func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateLogs(ctx, set, c, next)
		}, sl))
	}
	return xprocessor.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

// Connector returns f creating each connector from its redirected
// configuration. Profiles are not offered, as the instrumented connector
// offers none.
func (e *Endpoints) Connector(f connector.Factory) connector.Factory {
	var opts []xconnector.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xconnector.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	redirect := func(set connector.Settings, cfg component.Config) (component.Config, error) {
		return e.Redirect(topology.KindConnector, set.ID, cfg)
	}
	undefined := component.StabilityLevelUndefined
	if sl := f.TracesToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Traces, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateTracesToTraces(ctx, set, c, next)
		}, sl))
	}
	if sl := f.TracesToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Traces, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateTracesToMetrics(ctx, set, c, next)
		}, sl))
	}
	if sl := f.TracesToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithTracesToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Traces, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateTracesToLogs(ctx, set, c, next)
		}, sl))
	}
	if sl := f.MetricsToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Metrics, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateMetricsToTraces(ctx, set, c, next)
		}, sl))
	}
	if sl := f.MetricsToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Metrics, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateMetricsToMetrics(ctx, set, c, next)
		}, sl))
	}
	if sl := f.MetricsToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithMetricsToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Metrics, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateMetricsToLogs(ctx, set, c, next)
		}, sl))
	}
	if sl := f.LogsToTracesStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToTraces(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Traces) (connector.Logs, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateLogsToTraces(ctx, set, c, next)
		}, sl))
	}
	if sl := f.LogsToMetricsStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Logs, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateLogsToMetrics(ctx, set, c, next)
		}, sl))
	}
	if sl := f.LogsToLogsStability(); sl != undefined {
		opts = append(opts, xconnector.WithLogsToLogs(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Logs) (connector.Logs, error) {
			c, err := redirect(set, cfg)
			if err != nil {
				return nil, err
			}
			return f.CreateLogsToLogs(ctx, set, c, next)
		}, sl))
	}
	return xconnector.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}
