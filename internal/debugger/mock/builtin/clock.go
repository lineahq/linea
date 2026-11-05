package builtin

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/spanmetricsconnector"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/connector/xconnector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/receiver"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// spanmetricsTypes are the connector's type and its deprecated alias: at
// v0.162.0 the factory type is span_metrics, and configurations written as
// spanmetrics still resolve to it.
var spanmetricsTypes = []component.Type{component.MustNewType("span_metrics"), component.MustNewType("spanmetrics")}

// flushWait bounds how long a simulated flush may take to reach the
// downstream pipeline.
const flushWait = 10 * time.Second

// clockProvider runs spanmetrics connectors on a simulated clock. The
// connector aggregates spans and exports only when its ticker fires
// (metrics_flush_interval, one minute by default); shutting down does not
// flush. A run that lasts a moment would therefore show nothing. The real
// connector instead reads a fake clock from its create context
// (clockwork.FromContext, verified at v0.162.0), which starts at Spec.Clock
// and, when the run ends, is advanced by whole flush intervals until the
// aggregated spans are exported, before the connector's own shutdown.
type clockProvider struct {
	origin time.Time
	ids    []component.ID
	cfgs   map[component.ID]*spanmetricsconnector.Config

	mu      sync.Mutex
	flushes map[component.ID][]time.Time
	errs    map[component.ID]string
}

func newClock(cfg *otelcol.Config, spec mock.Spec) *clockProvider {
	ids := usedConnectors(cfg, spanmetricsTypes...)
	if len(ids) == 0 {
		return nil
	}
	p := &clockProvider{origin: spec.Clock, ids: ids, cfgs: map[component.ID]*spanmetricsconnector.Config{},
		flushes: map[component.ID][]time.Time{}, errs: map[component.ID]string{}}
	for _, id := range ids {
		c, _ := cfg.Connectors[id].(*spanmetricsconnector.Config)
		p.cfgs[id] = c
	}
	return p
}

func (p *clockProvider) Name() string { return "clock" }

func (p *clockProvider) Components() []mock.Component {
	out := make([]mock.Component, 0, len(p.ids))
	for _, id := range p.ids {
		out = append(out, mock.Component{
			Kind: topology.KindConnector, ID: id, Mode: topology.ModeDependencyMocked,
			Note: "the real connector runs on a simulated clock that flushes its metrics when the run ends",
		})
	}
	return out
}

func (p *clockProvider) Receiver(f receiver.Factory) (receiver.Factory, bool)    { return f, false }
func (p *clockProvider) Processor(f processor.Factory) (processor.Factory, bool) { return f, false }

func (p *clockProvider) Connector(f connector.Factory) (connector.Factory, bool) {
	if !slices.Contains(spanmetricsTypes, f.Type()) {
		return f, false
	}
	var opts []xconnector.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xconnector.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	opts = append(opts, xconnector.WithTracesToMetrics(func(ctx context.Context, set connector.Settings, cfg component.Config, next consumer.Metrics) (connector.Traces, error) {
		origin := p.origin
		if origin.IsZero() {
			origin = time.Now()
		}
		clock := clockwork.NewFakeClockAt(origin)
		flushed := make(chan struct{}, 1)
		out := flushSignal{Metrics: next, flushed: flushed}
		c, err := f.CreateTracesToMetrics(clockwork.AddToContext(ctx, clock), set, cfg, out)
		if err != nil {
			return nil, err
		}
		return &clockedConnector{Traces: c, p: p, id: set.ID, cfg: cfg, clock: clock, flushed: flushed}, nil
	}, f.TracesToMetricsStability()))
	return xconnector.NewFactory(f.Type(), f.CreateDefaultConfig, opts...), true
}

func (p *clockProvider) start(context.Context) error { return nil }
func (p *clockProvider) ready(context.Context) error { return nil }
func (p *clockProvider) drive(context.Context) error { return nil }
func (p *clockProvider) close() error                { return nil }
func (p *clockProvider) Gaps() []mock.Gap            { return nil }

func (p *clockProvider) declarations() []mock.Declaration {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []mock.Declaration
	for _, id := range p.ids {
		d := mock.Declaration{Provider: p.Name(), Ref: topology.Ref(topology.KindConnector, id), Mode: topology.ModeDependencyMocked}
		origin := "the run's start (the inputs carry no timestamps)"
		if !p.origin.IsZero() {
			origin = p.origin.UTC().Format(time.RFC3339Nano) + " (the latest record time in the inputs)"
		}
		d.Supplied = append(d.Supplied, "clock start: "+origin)
		for _, t := range p.flushes[id] {
			d.Supplied = append(d.Supplied, "flush at "+t.UTC().Format(time.RFC3339Nano))
		}
		d.Limitations = []string{
			"the connector's clock is simulated: it stands still while the inputs are processed, so all of them fall into one flush interval",
			"when the run ends, the clock advances by metrics_flush_interval until the aggregated spans are exported (twice for cumulative temporality, whose first point of a series is 0); a deployed Collector flushes on its own schedule",
		}
		if e := p.errs[id]; e != "" {
			d.Warnings = append(d.Warnings, e)
		}
		out = append(out, d)
	}
	return out
}

func (p *clockProvider) flushed(id component.ID, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushes[id] = append(p.flushes[id], at)
}

func (p *clockProvider) failed(id component.ID, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs[id] = msg
}

// flushSignal passes metrics on and signals each export once it returned.
type flushSignal struct {
	consumer.Metrics
	flushed chan<- struct{}
}

func (s flushSignal) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	defer func() {
		select {
		case s.flushed <- struct{}{}:
		default:
		}
	}()
	return s.Metrics.ConsumeMetrics(ctx, md)
}

// clockedConnector advances the connector's clock before shutting it down,
// so what it aggregated is exported while the downstream pipelines still
// run (the Collector shuts a connector down before the pipelines it feeds).
type clockedConnector struct {
	connector.Traces
	p       *clockProvider
	id      component.ID
	cfg     component.Config
	clock   *clockwork.FakeClock
	flushed chan struct{}

	started bool
}

func (c *clockedConnector) Start(ctx context.Context, host component.Host) error {
	err := c.Traces.Start(ctx, host)
	c.started = err == nil
	return err
}

func (c *clockedConnector) Shutdown(ctx context.Context) error {
	if c.started {
		c.started = false
		if !c.flush(ctx) {
			// The connector's export goroutine is still blocked downstream,
			// and its Shutdown waits for that goroutine, so it would block
			// the run's shutdown too.
			go func() { _ = c.Traces.Shutdown(context.Background()) }()
			return nil
		}
	}
	return c.Traces.Shutdown(ctx)
}

// flush advances the clock one flush interval per export the aggregated
// spans need, waiting for each export. It reports whether every export
// returned.
func (c *clockedConnector) flush(ctx context.Context) bool {
	interval, ticks := time.Minute, 2
	if cfg, ok := c.cfg.(*spanmetricsconnector.Config); ok {
		if cfg.MetricsFlushInterval > 0 {
			interval = cfg.MetricsFlushInterval
		}
		if cfg.GetAggregationTemporality() == pmetric.AggregationTemporalityDelta {
			ticks = 1
		}
	}
	timer := time.NewTimer(flushWait)
	defer timer.Stop()
	for range ticks {
		c.clock.Advance(interval)
		select {
		case <-c.flushed:
			c.p.flushed(c.id, c.clock.Now())
		case <-timer.C:
			c.p.failed(c.id, fmt.Sprintf("the simulated flush did not reach the downstream pipeline within %v", flushWait))
			return false
		case <-ctx.Done():
			c.p.failed(c.id, "the run ended before the simulated flush completed")
			return false
		}
	}
	return true
}

// usedConnectors returns the connector IDs of the given types that a
// pipeline uses, sorted, once each.
func usedConnectors(cfg *otelcol.Config, types ...component.Type) []component.ID {
	seen := map[component.ID]bool{}
	var out []component.ID
	for _, p := range cfg.Service.Pipelines {
		for _, id := range slices.Concat(p.Receivers, p.Exporters) {
			if _, connector := cfg.Connectors[id]; connector && slices.Contains(types, id.Type()) && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sortIDs(out)
	return out
}

// Prepare runs the provider on the run's lifecycle.
func (p *clockProvider) Prepare(ctx context.Context, run provider.Run) error {
	return mock.PrepareBuiltin(ctx, run, mock.Lifecycle{Start: p.start, Ready: p.ready, Drive: p.drive, Declarations: p.declarations, Close: p.close})
}
