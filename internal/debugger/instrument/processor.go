package instrument

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/xprocessor"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// aliasProvider is implemented by factories registered under a deprecated
// type alias; the alias must be carried over or configs using it stop
// resolving.
type aliasProvider interface{ DeprecatedAlias() component.Type }

// ingressKey marks a context with the stage.input currently being consumed by
// one processor instance. Components never read it; it only lets the egress
// side recognise emissions made synchronously within that ConsumeX call.
type ingressKey struct{}

type ingressMark struct {
	owner *stage
	seq   int
	call  *call
}

// call is the state of one ConsumeX call on a stage.
type call struct {
	mu sync.Mutex
	// downstream is the last error returned by the next consumer during the
	// call. A processor usually passes it up; it is then not the stage's own
	// error.
	downstream error
}

func (c *call) setDownstream(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.downstream = err
}

// fromDownstream reports whether err is (or wraps) a downstream error.
func (c *call) fromDownstream(err error) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.downstream != nil && errors.Is(err, c.downstream)
}

// stage is the identity of one created processor instance: one component in
// one pipeline.
type stage struct {
	name     string
	pipeline string
	signal   string
	source   Source
	rec      *Recorder
}

func (s *stage) event(kind Kind) Event {
	return Event{Kind: kind, Component: s.name, Pipeline: s.pipeline, Signal: s.signal, Mode: ModeReal, Source: s.source}
}

func (s *stage) ingress(ctx context.Context, p telemetry.Payload) (context.Context, ingressMark) {
	e := s.event(KindIngress)
	e.FromSeq = EmissionOf(ctx)
	m := ingressMark{owner: s, seq: s.rec.RecordTelemetry(e, p), call: &call{}}
	return context.WithValue(ctx, ingressKey{}, m), m
}

// egress records an emission and forwards it with emit, passing a context
// that names the emission so the receiving side can link to it.
func (s *stage) egress(ctx context.Context, p telemetry.Payload, emit func(context.Context) error) error {
	return s.egressTo(ctx, "", p, emit)
}

// egressTo is egress for an output delivered into the named pipeline; an
// empty pipeline is the stage's own.
func (s *stage) egressTo(ctx context.Context, pipeline string, p telemetry.Payload, emit func(context.Context) error) error {
	e := s.event(KindEgress)
	if pipeline != "" {
		e.Pipeline = pipeline
	}
	m, own := ctx.Value(ingressKey{}).(ingressMark)
	own = own && m.owner == s
	if own {
		e.InputSeq = m.seq
	}
	err := emit(WithEmission(ctx, s.rec.RecordTelemetry(e, p)))
	if own && err != nil {
		m.call.setDownstream(err)
	}
	return err
}

// consumed records the error a processor returned for an input, unless it
// is an error the next consumer returned during the call (recorded where it
// originated). The error is returned to the caller unchanged.
func (s *stage) consumed(m ingressMark, err error) error {
	if err != nil && !m.call.fromDownstream(err) {
		e := s.event(KindStageError)
		e.InputSeq = m.seq
		e.Error = err.Error()
		s.rec.Record(e)
	}
	return err
}

func (s *stage) lifecycle(kind Kind, err error) error {
	e := s.event(kind)
	if err != nil {
		e.Error = err.Error()
	}
	s.rec.Record(e)
	return err
}

func newStage(set processor.Settings, signal string, rec *Recorder) *stage {
	return &stage{name: set.ID.String(), pipeline: pipelineOf(set.Logger), signal: signal, source: SourceProcessorWrapper, rec: rec}
}

// Processor returns a factory with the same type, alias, default config and
// per-signal stability as f. Each created processor is the real processor
// from f; the decoration only records events to rec.
//
// Profiles are not decorated; configs using profiles pipelines with an
// instrumented processor fail to build.
func Processor(f processor.Factory, rec *Recorder) processor.Factory {
	var opts []xprocessor.FactoryOption
	if ap, ok := f.(aliasProvider); ok && ap.DeprecatedAlias().String() != "" {
		opts = append(opts, xprocessor.WithDeprecatedTypeAlias(ap.DeprecatedAlias()))
	}
	if sl := f.TracesStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xprocessor.WithTraces(createTraces(f, rec), sl))
	}
	if sl := f.MetricsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xprocessor.WithMetrics(createMetrics(f, rec), sl))
	}
	if sl := f.LogsStability(); sl != component.StabilityLevelUndefined {
		opts = append(opts, xprocessor.WithLogs(createLogs(f, rec), sl))
	}
	return xprocessor.NewFactory(f.Type(), f.CreateDefaultConfig, opts...)
}

func createTraces(f processor.Factory, rec *Recorder) processor.CreateTracesFunc {
	return func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
		s := newStage(set, "traces", rec)
		egress, err := consumer.NewTraces(func(ctx context.Context, td ptrace.Traces) error {
			return s.egress(ctx, telemetry.FromTraces(td), func(ctx context.Context) error { return next.ConsumeTraces(ctx, td) })
		}, consumer.WithCapabilities(next.Capabilities()))
		if err != nil {
			return nil, err
		}
		real, err := f.CreateTraces(ctx, s.observe(set), withDecisionHooks(cfg, s), egress)
		if err != nil {
			return nil, err
		}
		return &tracesProcessor{Traces: real, lifecycle: lifecycle{s}}, nil
	}
}

func createMetrics(f processor.Factory, rec *Recorder) processor.CreateMetricsFunc {
	return func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
		s := newStage(set, "metrics", rec)
		egress, err := consumer.NewMetrics(func(ctx context.Context, md pmetric.Metrics) error {
			return s.egress(ctx, telemetry.FromMetrics(md), func(ctx context.Context) error { return next.ConsumeMetrics(ctx, md) })
		}, consumer.WithCapabilities(next.Capabilities()))
		if err != nil {
			return nil, err
		}
		real, err := f.CreateMetrics(ctx, s.observe(set), cfg, egress)
		if err != nil {
			return nil, err
		}
		return &metricsProcessor{Metrics: real, lifecycle: lifecycle{s}}, nil
	}
}

func createLogs(f processor.Factory, rec *Recorder) processor.CreateLogsFunc {
	return func(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
		s := newStage(set, "logs", rec)
		egress, err := consumer.NewLogs(func(ctx context.Context, ld plog.Logs) error {
			return s.egress(ctx, telemetry.FromLogs(ld), func(ctx context.Context) error { return next.ConsumeLogs(ctx, ld) })
		}, consumer.WithCapabilities(next.Capabilities()))
		if err != nil {
			return nil, err
		}
		real, err := f.CreateLogs(ctx, s.observe(set), cfg, egress)
		if err != nil {
			return nil, err
		}
		return &logsProcessor{Logs: real, lifecycle: lifecycle{s}}, nil
	}
}

// lifecycle records Start and Shutdown of the real component around the
// calls it delegates to.
type lifecycle struct{ stage *stage }

func (l lifecycle) start(ctx context.Context, host component.Host, real component.Component) error {
	return l.stage.lifecycle(KindComponentStarted, real.Start(ctx, host))
}

func (l lifecycle) shutdown(ctx context.Context, real component.Component) error {
	return l.stage.lifecycle(KindComponentStopped, real.Shutdown(ctx))
}

// The wrappers embed the real processor so Capabilities (which drives fanout
// cloning) is the real component's.

type tracesProcessor struct {
	processor.Traces
	lifecycle
}

func (p *tracesProcessor) Start(ctx context.Context, host component.Host) error {
	return p.start(ctx, host, p.Traces)
}

func (p *tracesProcessor) Shutdown(ctx context.Context) error { return p.shutdown(ctx, p.Traces) }

func (p *tracesProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	ctx, m := p.stage.ingress(ctx, telemetry.FromTraces(td))
	return p.stage.consumed(m, p.Traces.ConsumeTraces(ctx, td))
}

type metricsProcessor struct {
	processor.Metrics
	lifecycle
}

func (p *metricsProcessor) Start(ctx context.Context, host component.Host) error {
	return p.start(ctx, host, p.Metrics)
}

func (p *metricsProcessor) Shutdown(ctx context.Context) error { return p.shutdown(ctx, p.Metrics) }

func (p *metricsProcessor) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	ctx, m := p.stage.ingress(ctx, telemetry.FromMetrics(md))
	return p.stage.consumed(m, p.Metrics.ConsumeMetrics(ctx, md))
}

type logsProcessor struct {
	processor.Logs
	lifecycle
}

func (p *logsProcessor) Start(ctx context.Context, host component.Host) error {
	return p.start(ctx, host, p.Logs)
}

func (p *logsProcessor) Shutdown(ctx context.Context) error { return p.shutdown(ctx, p.Logs) }

func (p *logsProcessor) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	ctx, m := p.stage.ingress(ctx, telemetry.FromLogs(ld))
	return p.stage.consumed(m, p.Logs.ConsumeLogs(ctx, ld))
}
