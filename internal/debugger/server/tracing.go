package server

import (
	"bufio"
	"net"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope of the server's request spans.
const tracerName = "github.com/lineahq/linea/internal/debugger/server"

// traced gives every request one server span, named by its method and the
// route route finds for it, such as "GET /api/v1/runs/{id}". It records the
// method, route and status code only: no path values, query, headers, user
// agent, client address or caller. An incoming W3C traceparent continues
// its trace. Worker and builder connections, which upgrade and last as long
// as the process they serve, get no span. A nil provider traces nothing.
func traced(next http.Handler, provider trace.TracerProvider, route func(*http.Request) string) http.Handler {
	if provider == nil {
		return next
	}
	tracer := provider.Tracer(tracerName)
	propagator := propagation.TraceContext{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			next.ServeHTTP(w, r)
			return
		}
		name, pattern := spanName(r.Method, route(r))
		attrs := []attribute.KeyValue{attribute.String("http.request.method", r.Method)}
		if pattern != "" {
			attrs = append(attrs, attribute.String("http.route", pattern))
		}
		ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			status := sw.status
			if status == 0 {
				status = http.StatusOK
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= 500 {
				span.SetStatus(codes.Error, "")
			}
			span.End()
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

// spanName is a request span's name and http.route from the matched
// pattern: "GET /api/v1/runs/{id}" names itself and has the route
// "/api/v1/runs/{id}"; a pattern without a method is prefixed with it; an
// unmatched request is named by its method alone.
func spanName(method, pattern string) (name, route string) {
	if pattern == "" {
		return method, ""
	}
	if m, path, ok := strings.Cut(pattern, " "); ok {
		return m + " " + path, path
	}
	return method + " " + pattern, pattern
}

// statusWriter records the status code written, and keeps flushing and
// hijacking available to the handler.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
