package runtime

import (
	"context"
	"net"
	"slices"
	"sync"

	"go.opentelemetry.io/collector/client"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"

	"github.com/lineahq/linea/internal/debugger/mock"
)

// referenceFactories returns the real factories with the mock providers'
// substitutes, as the instrumented run uses them, and no instrumentation.
// With peers, OTLP receivers report each loopback connection as coming
// from the client address its input declares.
func referenceFactories(base otelcol.Factories, providers []mock.Provider, endpoints *mock.Endpoints, peers *peerAddrs, local *localSink) (otelcol.Factories, error) {
	out := base
	var receivers []receiver.Factory
	for _, f := range unique(base.Receivers) {
		for _, p := range providers {
			if g, ok := p.Receiver(f); ok {
				f = g
				break
			}
		}
		if peers != nil && f.Type() == otlpReceiverType {
			f = peerReceiverFactory(f, peers)
		}
		receivers = append(receivers, f)
	}
	var processors []processor.Factory
	for _, f := range unique(base.Processors) {
		for _, p := range providers {
			if g, ok := p.Processor(f); ok {
				f = g
				break
			}
		}
		if isCustom(base.ProcessorModules, f.Type()) {
			f = endpoints.Processor(f)
		}
		processors = append(processors, f)
	}
	var connectors []connector.Factory
	for _, f := range unique(base.Connectors) {
		for _, p := range providers {
			if g, ok := p.Connector(f); ok {
				f = g
				break
			}
		}
		if isCustom(base.ConnectorModules, f.Type()) {
			f = endpoints.Connector(f)
		}
		connectors = append(connectors, f)
	}
	var err error
	if out.Receivers, err = otelcol.MakeFactoryMap(receivers...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Processors, err = otelcol.MakeFactoryMap(processors...); err != nil {
		return otelcol.Factories{}, err
	}
	if out.Connectors, err = otelcol.MakeFactoryMap(connectors...); err != nil {
		return otelcol.Factories{}, err
	}
	// Local exporters run as in the instrumented run, observed alike.
	var exporters []exporter.Factory
	for _, f := range unique(base.Exporters) {
		if slices.Contains(localExporters, f.Type()) {
			f = observedExporterFactory(f, local.observe)
		}
		exporters = append(exporters, f)
	}
	if out.Exporters, err = otelcol.MakeFactoryMap(exporters...); err != nil {
		return otelcol.Factories{}, err
	}
	// The reference run starts the same extensions: custom ones only,
	// their client endpoints simulated.
	if out.Extensions, err = runExtensions(base, nil, endpoints); err != nil {
		return otelcol.Factories{}, err
	}
	return out, nil
}

// peerAddrs maps the local address of each loopback connection the
// reference run opens to the client address its input declares.
type peerAddrs struct {
	mu sync.Mutex
	m  map[string]net.Addr
}

func newPeerAddrs() *peerAddrs { return &peerAddrs{m: map[string]net.Addr{}} }

func (p *peerAddrs) set(local, declared net.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.m[local.String()] = declared
}

// rewrite replaces the client address of a connection the reference run
// opened for an input with a declared one.
func (p *peerAddrs) rewrite(ctx context.Context) context.Context {
	info := client.FromContext(ctx)
	if info.Addr == nil {
		return ctx
	}
	p.mu.Lock()
	declared, ok := p.m[info.Addr.String()]
	p.mu.Unlock()
	if !ok {
		return ctx
	}
	info.Addr = declared
	return client.NewContext(ctx, info)
}

// peerReceiverFactory wraps f so that the consumers it delivers to see the
// declared client addresses. The receiver itself runs unchanged: it stands
// behind the equivalent of address translation.
func peerReceiverFactory(f receiver.Factory, peers *peerAddrs) receiver.Factory {
	return receiver.NewFactory(f.Type(), f.CreateDefaultConfig,
		receiver.WithTraces(func(ctx context.Context, set receiver.Settings, cfg component.Config, next consumer.Traces) (receiver.Traces, error) {
			wrapped, err := consumer.NewTraces(func(ctx context.Context, td ptrace.Traces) error {
				return next.ConsumeTraces(peers.rewrite(ctx), td)
			}, consumer.WithCapabilities(next.Capabilities()))
			if err != nil {
				return nil, err
			}
			return f.CreateTraces(ctx, set, cfg, wrapped)
		}, f.TracesStability()),
		receiver.WithMetrics(func(ctx context.Context, set receiver.Settings, cfg component.Config, next consumer.Metrics) (receiver.Metrics, error) {
			wrapped, err := consumer.NewMetrics(func(ctx context.Context, md pmetric.Metrics) error {
				return next.ConsumeMetrics(peers.rewrite(ctx), md)
			}, consumer.WithCapabilities(next.Capabilities()))
			if err != nil {
				return nil, err
			}
			return f.CreateMetrics(ctx, set, cfg, wrapped)
		}, f.MetricsStability()),
		receiver.WithLogs(func(ctx context.Context, set receiver.Settings, cfg component.Config, next consumer.Logs) (receiver.Logs, error) {
			wrapped, err := consumer.NewLogs(func(ctx context.Context, ld plog.Logs) error {
				return next.ConsumeLogs(peers.rewrite(ctx), ld)
			}, consumer.WithCapabilities(next.Capabilities()))
			if err != nil {
				return nil, err
			}
			return f.CreateLogs(ctx, set, cfg, wrapped)
		}, f.LogsStability()),
	)
}
