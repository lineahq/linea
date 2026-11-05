package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/otelcol"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"go.uber.org/zap"

	"github.com/lineahq/linea/collector/provider"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Session drives a run's providers through the provider SDK's lifecycle:
// Prepare before the Collector starts, then OnReady,
// OnInput before each input, OnInputsDone, and OnClose after the Collector
// shut down. The debugger's own providers and those of component modules
// run alike; each gets its own provider.Run, and what they report becomes
// the run's mock declarations.
type Session struct {
	runs   []*run
	closed bool
}

// NewSession prepares the lifecycle of providers for one run. cfg is the
// run's configuration, endpoints its simulated endpoints and logger the
// run's log.
func NewSession(cfg *otelcol.Config, spec Spec, providers []Provider, endpoints *Endpoints, logger *zap.Logger) *Session {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Session{}
	for _, p := range providers {
		s.runs = append(s.runs, &run{p: p, cfg: cfg, settings: spec.Providers[p.Name()], endpoints: endpoints,
			logger: logger.With(zap.String("provider", p.Name())), decls: map[string]*Declaration{}})
	}
	return s
}

// Prepare calls each provider's Prepare, in order.
func (s *Session) Prepare(ctx context.Context) error {
	for _, r := range s.runs {
		if err := r.guard(func() error { return r.p.Prepare(ctx, r) }); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// Ready runs the OnReady hooks.
func (s *Session) Ready(ctx context.Context) error {
	return s.each(func(r *run) error {
		for _, h := range r.hooks().ready {
			if err := r.guard(func() error { return h(ctx) }); err != nil {
				return err
			}
		}
		return nil
	})
}

// Input runs the OnInput hooks for the input about to be injected.
func (s *Session) Input(ctx context.Context, in provider.Input) error {
	return s.each(func(r *run) error {
		for _, h := range r.hooks().input {
			if err := r.guard(func() error { return h(ctx, in) }); err != nil {
				return err
			}
		}
		return nil
	})
}

// InputsDone runs the OnInputsDone hooks.
func (s *Session) InputsDone(ctx context.Context) error {
	return s.each(func(r *run) error {
		for _, h := range r.hooks().done {
			if err := r.guard(func() error { return h(ctx) }); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close runs the OnClose hooks once, in reverse order of the providers,
// and closes the listeners the providers took. A module provider's failing
// hook becomes a warning of its declaration, since the run itself is over;
// the debugger's own providers only release temporary resources there.
func (s *Session) Close() {
	if s.closed {
		return
	}
	s.closed = true
	for _, r := range slices.Backward(s.runs) {
		for _, h := range slices.Backward(r.hooks().close) {
			if err := r.guard(h); err != nil {
				if _, ok := r.p.(*plugin); ok {
					if cs := r.p.Components(); len(cs) > 0 {
						r.Warning(topology.Ref(cs[0].Kind, cs[0].ID), "the provider failed to close: "+err.Error())
					}
				}
				r.logger.Warn("provider close failed", zap.Error(err))
			}
		}
		r.closeListeners()
	}
}

// Declarations describe what each provider did, valid after Close: one per
// component the provider handles, in the order of its Components, then any
// other component it reported on.
func (s *Session) Declarations() []Declaration {
	var out []Declaration
	for _, r := range s.runs {
		out = append(out, r.declarations()...)
	}
	return out
}

func (s *Session) each(f func(*run) error) error {
	for _, r := range s.runs {
		if err := f(r); err != nil {
			return r.fail(err)
		}
	}
	return nil
}

// run is one provider's provider.Run.
type run struct {
	p         Provider
	cfg       *otelcol.Config
	settings  json.RawMessage
	endpoints *Endpoints
	logger    *zap.Logger

	mu        sync.Mutex
	ready     []func(context.Context) error
	input     []func(context.Context, provider.Input) error
	done      []func(context.Context) error
	close     []func() error
	listeners []net.Listener
	served    []EndpointRef
	decls     map[string]*Declaration
	extra     []string // reported components outside Components, in order
}

type hooks struct {
	ready []func(context.Context) error
	input []func(context.Context, provider.Input) error
	done  []func(context.Context) error
	close []func() error
}

func (r *run) hooks() hooks {
	r.mu.Lock()
	defer r.mu.Unlock()
	return hooks{ready: slices.Clone(r.ready), input: slices.Clone(r.input), done: slices.Clone(r.done), close: slices.Clone(r.close)}
}

// fail names the provider in err.
func (r *run) fail(err error) error {
	if m, ok := r.p.(interface{ Package() string }); ok {
		return fmt.Errorf("provider %s (%s): %w", r.p.Name(), m.Package(), err)
	}
	return fmt.Errorf("mock %s: %w", r.p.Name(), err)
}

// guard calls f, turning a panic into an error.
func (r *run) guard(f func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return f()
}

func (r *run) Components() []provider.Component {
	var out []provider.Component
	for _, c := range r.p.Components() {
		out = append(out, r.sdkComponent(c.Kind, c.ID))
	}
	return out
}

// sdkComponent is a component as the SDK hands it to providers.
func (r *run) sdkComponent(kind topology.Kind, id component.ID) provider.Component {
	pc := provider.Component{ID: topology.Ref(kind, id)}
	if cfg := componentConfig(r.cfg, kind, id); cfg != nil {
		conf := confmap.New()
		if err := conf.Marshal(cfg); err == nil {
			pc.Config = conf.ToStringMap()
		}
	}
	return pc
}

func (r *run) Pipelines() []provider.Pipeline {
	var out []provider.Pipeline
	for id, pc := range r.cfg.Service.Pipelines {
		p := provider.Pipeline{ID: id.String(), Signal: id.Signal().String()}
		for _, proc := range pc.Processors {
			p.Processors = append(p.Processors, r.sdkComponent(topology.KindProcessor, proc))
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b provider.Pipeline) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (r *run) Settings(target any) error {
	if len(r.settings) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.settings, target); err != nil {
		return fmt.Errorf("mocks.providers.%s: %w", r.p.Name(), err)
	}
	return nil
}

func (r *run) Listener(ref, path string) (net.Listener, error) {
	c, ok := r.component(ref)
	if !ok {
		return nil, fmt.Errorf("%s is not one of the provider's components in this run", ref)
	}
	if !slices.Contains(endpointPaths(r.p), path) {
		return nil, fmt.Errorf("%s is not one of the provider's endpoints", path)
	}
	if r.endpoints == nil {
		return nil, errors.New("the run simulates no endpoints")
	}
	l, err := net.Listen("tcp", loopback)
	if err != nil {
		return nil, err
	}
	er := EndpointRef{Kind: c.Kind, ID: c.ID, Path: path}
	if err := r.endpoints.Serve(er, r.p.Name(), l.Addr().String()); err != nil {
		_ = l.Close()
		return nil, err
	}
	r.mu.Lock()
	r.listeners = append(r.listeners, l)
	r.served = append(r.served, er)
	r.mu.Unlock()
	return l, nil
}

func (r *run) Record(ref, path, signal string, otlp []byte) error {
	c, ok := r.component(ref)
	if !ok {
		return fmt.Errorf("%s is not one of the provider's components in this run", ref)
	}
	if !slices.Contains(endpointPaths(r.p), path) {
		return fmt.Errorf("%s is not one of the provider's endpoints", path)
	}
	p, err := decodeOTLP(signal, otlp)
	if err != nil {
		return err
	}
	if r.endpoints != nil {
		r.endpoints.Received(EndpointRef{Kind: c.Kind, ID: c.ID, Path: path}, p)
	}
	return nil
}

func (r *run) OnReady(h func(context.Context) error) { r.add(func() { r.ready = append(r.ready, h) }) }
func (r *run) OnInput(h func(context.Context, provider.Input) error) {
	r.add(func() { r.input = append(r.input, h) })
}
func (r *run) OnInputsDone(h func(context.Context) error) {
	r.add(func() { r.done = append(r.done, h) })
}
func (r *run) OnClose(h func() error) { r.add(func() { r.close = append(r.close, h) }) }

func (r *run) add(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *run) Supplied(ref, what string) {
	r.declare(ref, func(d *Declaration) { d.Supplied = append(d.Supplied, what) })
}

func (r *run) Limitation(ref, what string) {
	r.declare(ref, func(d *Declaration) { d.Limitations = append(d.Limitations, what) })
}

func (r *run) Warning(ref, what string) {
	r.declare(ref, func(d *Declaration) { d.Warnings = append(d.Warnings, what) })
}

func (r *run) Logf(format string, args ...any) { r.logger.Info(fmt.Sprintf(format, args...)) }

func (r *run) declare(ref string, f func(*Declaration)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.decls[ref]
	if d == nil {
		d = &Declaration{Provider: r.p.Name(), Ref: ref, Mode: topology.ModeDependencyMocked}
		r.decls[ref] = d
		if _, ok := r.component(ref); !ok {
			r.extra = append(r.extra, ref)
		}
	}
	f(d)
}

func (r *run) component(ref string) (Component, bool) {
	for _, c := range r.p.Components() {
		if topology.Ref(c.Kind, c.ID) == ref {
			return c, true
		}
	}
	return Component{}, false
}

func (r *run) closeListeners() {
	r.mu.Lock()
	ls := r.listeners
	r.listeners = nil
	r.mu.Unlock()
	for _, l := range ls {
		_ = l.Close()
	}
}

func (r *run) declarations() []Declaration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Declaration
	for _, c := range r.p.Components() {
		ref := topology.Ref(c.Kind, c.ID)
		d := Declaration{Provider: r.p.Name(), Ref: ref, Mode: c.Mode}
		if have := r.decls[ref]; have != nil {
			d.Supplied, d.Limitations, d.Warnings = have.Supplied, have.Limitations, have.Warnings
		}
		for _, er := range r.served {
			if er.Ref() == ref && r.endpoints != nil && !r.endpoints.Connected(er) {
				d.Warnings = append(d.Warnings, fmt.Sprintf("endpoint %s: the configuration has no client or address there, so nothing reached the provider", er.Path))
			}
		}
		out = append(out, d)
	}
	for _, ref := range r.extra {
		out = append(out, *r.decls[ref])
	}
	return out
}

// componentConfig is the configuration of a configured component.
func componentConfig(cfg *otelcol.Config, kind topology.Kind, id component.ID) component.Config {
	if cfg == nil {
		return nil
	}
	switch kind {
	case topology.KindReceiver:
		return cfg.Receivers[id]
	case topology.KindProcessor:
		return cfg.Processors[id]
	case topology.KindExporter:
		return cfg.Exporters[id]
	case topology.KindConnector:
		return cfg.Connectors[id]
	case topology.KindExtension:
		return cfg.Extensions[id]
	}
	return nil
}

// decodeOTLP decodes an OTLP protobuf export request of a signal.
func decodeOTLP(signal string, raw []byte) (telemetry.Payload, error) {
	switch signal {
	case "traces":
		req := ptraceotlp.NewExportRequest()
		if err := req.UnmarshalProto(raw); err != nil {
			return telemetry.Payload{}, fmt.Errorf("decode OTLP traces: %w", err)
		}
		return telemetry.FromTraces(req.Traces()), nil
	case "metrics":
		req := pmetricotlp.NewExportRequest()
		if err := req.UnmarshalProto(raw); err != nil {
			return telemetry.Payload{}, fmt.Errorf("decode OTLP metrics: %w", err)
		}
		return telemetry.FromMetrics(req.Metrics()), nil
	case "logs":
		req := plogotlp.NewExportRequest()
		if err := req.UnmarshalProto(raw); err != nil {
			return telemetry.Payload{}, fmt.Errorf("decode OTLP logs: %w", err)
		}
		return telemetry.FromLogs(req.Logs()), nil
	}
	return telemetry.Payload{}, fmt.Errorf("signal %q is not traces, metrics or logs", signal)
}
