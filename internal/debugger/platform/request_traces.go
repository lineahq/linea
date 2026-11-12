package platform

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Spans of the work a request does on this instance between its storage
// calls: preparing a run, inspecting a configuration on a runtime and
// waiting for runs to end. They are recorded only under a request's span. Like the run spans they carry IDs and counts,
// never a configuration, a runtime's name or an error message.

// Span and attribute names are a contract once published.
const (
	spanRunPrepare = "linea.run.prepare"
	spanRunWait    = "linea.run.wait"
	spanSuiteWait  = "linea.suite.wait"
	spanInspect    = "linea.inspect"

	attrSuiteID   = "linea.suite.id"
	attrSuiteRuns = "linea.suite.runs"
	// attrRemote says the work ran on a pooled worker, not in this process.
	attrRemote = "linea.runtime.remote"
)

// startSpan starts an internal span under the span in ctx. Work done
// outside a request, with no span in ctx, records nothing: it returns ctx
// and its non-recording span.
func (s *Service) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, trace.SpanFromContext(ctx)
	}
	return s.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
}

// waitRun is r.Wait under a linea.run.wait span; a run that already ended
// returns at once and has no span.
func (s *Service) waitRun(ctx context.Context, r *Handle) error {
	select {
	case <-r.done:
		return nil
	default:
	}
	_, span := s.startSpan(ctx, spanRunWait, attribute.String(attrRunID, r.id))
	defer span.End()
	return r.Wait(ctx)
}

// waitSuite waits for every member run in handles (nil for a member
// without a run) under one linea.suite.wait span; a suite whose runs all
// ended has no span.
func (s *Service) waitSuite(ctx context.Context, id string, handles []*Handle) error {
	pending := 0
	for _, h := range handles {
		if h == nil {
			continue
		}
		select {
		case <-h.done:
		default:
			pending++
		}
	}
	if pending == 0 {
		return nil
	}
	_, span := s.startSpan(ctx, spanSuiteWait, attribute.String(attrSuiteID, id), attribute.Int(attrSuiteRuns, pending))
	defer span.End()
	for _, h := range handles {
		if h == nil {
			continue
		}
		if err := h.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}

// requestScope lets the operations one request composes, such as an
// upgrade check's inspections, comparison and reports, share what each
// would otherwise repeat: the catch-up on other instances' writes, which
// the first operation does for all, and the binding of each runtime they
// name. It lives only as long as the request's context.
type requestScope struct {
	mu       sync.Mutex
	caughtUp bool
	bound    map[string]*boundRuntime
}

type requestScopeKey struct{}

// withRequestScope returns ctx with a request scope, keeping one ctx
// already has.
func withRequestScope(ctx context.Context) context.Context {
	if scopeOf(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, requestScopeKey{}, &requestScope{})
}

func scopeOf(ctx context.Context) *requestScope {
	sc, _ := ctx.Value(requestScopeKey{}).(*requestScope)
	return sc
}

// caughtUpOnce reports whether this request's scope has already caught up.
func (sc *requestScope) caughtUpOnce() bool {
	if sc == nil {
		return false
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.caughtUp
}

func (sc *requestScope) markCaughtUp() {
	if sc == nil {
		return
	}
	sc.mu.Lock()
	sc.caughtUp = true
	sc.mu.Unlock()
}

// boundRuntime returns the runtime this request already bound under name.
func (sc *requestScope) boundRuntime(name string) *boundRuntime {
	if sc == nil {
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.bound[name]
}

func (sc *requestScope) keep(name string, b *boundRuntime) {
	if sc == nil || b == nil {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.bound == nil {
		sc.bound = make(map[string]*boundRuntime)
	}
	sc.bound[name] = b
}
