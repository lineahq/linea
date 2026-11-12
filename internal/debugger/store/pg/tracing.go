package pg

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName names the instrumentation scope of the store spans.
const tracerName = "github.com/lineahq/linea/internal/debugger/store/pg"

// Span attributes. The statement text holds placeholders only, never values;
// no span carries arguments, connection details, bucket names or keys.
const (
	attrDBSystem     = attribute.Key("db.system.name")
	attrDBOperation  = attribute.Key("db.operation.name")
	attrDBQueryText  = attribute.Key("db.query.text")
	attrDBBatchSize  = attribute.Key("db.operation.batch.size")
	attrDBStatusCode = attribute.Key("db.response.status_code")
	attrRPCSystem    = attribute.Key("rpc.system")
	attrRPCMethod    = attribute.Key("rpc.method")
	attrObjectBytes  = attribute.Key("linea.object.bytes")
)

// spanKey holds the span a tracer started in the context it returns to pgx,
// so the matching End call ends it. A call made without a parent span stores
// nothing, and its End does nothing.
type spanKey struct{}

// pgTracer records a client span for each statement, batch and copy that runs
// under a span. It implements pgx.QueryTracer, pgx.BatchTracer and
// pgx.CopyFromTracer; pgx finds the batch and copy interfaces by asserting
// the one ConnConfig.Tracer value.
type pgTracer struct {
	tracer trace.Tracer
}

var (
	_ pgx.QueryTracer    = (*pgTracer)(nil)
	_ pgx.BatchTracer    = (*pgTracer)(nil)
	_ pgx.CopyFromTracer = (*pgTracer)(nil)
)

func newPGTracer(p trace.TracerProvider) *pgTracer {
	return &pgTracer{tracer: p.Tracer(tracerName)}
}

// start begins a client span under the span in ctx. With none it returns ctx
// unchanged: the change feed, lease renewals and upkeep poll constantly and
// would otherwise each become a root span.
func (t *pgTracer) start(ctx context.Context, name string, attrs ...attribute.KeyValue) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return context.WithValue(ctx, spanKey{}, span)
}

// end finishes the span start stored in ctx, if any. The error status holds
// the SQLSTATE of a database error and never its message.
func (t *pgTracer) end(ctx context.Context, err error) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		description := "error"
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code != "" {
			description = pgErr.Code
			span.SetAttributes(attrDBStatusCode.String(pgErr.Code))
		}
		span.SetStatus(codes.Error, description)
	}
	span.End()
}

func (t *pgTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	op := sqlOperation(data.SQL)
	if op == "" {
		op = "postgresql"
	}
	return t.start(ctx, op,
		attrDBSystem.String("postgresql"), attrDBOperation.String(op), attrDBQueryText.String(data.SQL))
}

func (t *pgTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	t.end(ctx, data.Err)
}

func (t *pgTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	attrs := []attribute.KeyValue{attrDBSystem.String("postgresql"), attrDBOperation.String("BATCH")}
	if data.Batch != nil {
		attrs = append(attrs, attrDBBatchSize.Int(data.Batch.Len()))
	}
	return t.start(ctx, "BATCH", attrs...)
}

// TraceBatchQuery records nothing: the batch span covers the whole round trip.
func (t *pgTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (t *pgTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	t.end(ctx, data.Err)
}

func (t *pgTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	return t.start(ctx, "COPY", attrDBSystem.String("postgresql"), attrDBOperation.String("COPY"))
}

func (t *pgTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.end(ctx, data.Err)
}

// sqlOperation is the statement's leading keyword, upper-cased, skipping
// whitespace, comments and opening parentheses. It is empty when there is none.
func sqlOperation(sql string) string {
	i := 0
	for i < len(sql) {
		switch c := sql[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(':
			i++
		case strings.HasPrefix(sql[i:], "--"):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				return ""
			}
			i += end + 1
		case strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += 2 + end + 2
		default:
			start := i
			for i < len(sql) && (sql[i] >= 'a' && sql[i] <= 'z' || sql[i] >= 'A' && sql[i] <= 'Z') {
				i++
			}
			return strings.ToUpper(sql[start:i])
		}
	}
	return ""
}

// tracedObjects records a client span for each object storage call that runs
// under a span. A span never carries the bucket, key, prefix or endpoint, and a
// missing object is not an error.
type tracedObjects struct {
	next   ObjectStore
	tracer trace.Tracer
}

var _ ObjectStore = tracedObjects{}

func newTracedObjects(next ObjectStore, p trace.TracerProvider) ObjectStore {
	return tracedObjects{next: next, tracer: p.Tracer(tracerName)}
}

// begin starts the span of one call and returns the context to pass on. With
// no parent span it returns a nil span and ctx unchanged.
func (o tracedObjects) begin(ctx context.Context, method string) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, nil
	}
	return o.tracer.Start(ctx, "s3 "+method, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrRPCSystem.String("aws-api"), attrRPCMethod.String(method)))
}

// finishObjectSpan ends span with the call's error and the bytes moved (a
// negative count when unknown). A nil span is a call made without a parent.
func finishObjectSpan(span trace.Span, err error, bytes int64) {
	if span == nil {
		return
	}
	if bytes >= 0 {
		span.SetAttributes(attrObjectBytes.Int64(bytes))
	}
	if err != nil && !errors.Is(err, ErrObjectNotFound) {
		span.SetStatus(codes.Error, "error")
	}
	span.End()
}

func (o tracedObjects) Put(ctx context.Context, key string, data []byte) error {
	ctx, span := o.begin(ctx, "Put")
	err := o.next.Put(ctx, key, data)
	finishObjectSpan(span, err, int64(len(data)))
	return err
}

func (o tracedObjects) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	ctx, span := o.begin(ctx, "Get")
	raw, err := o.next.Get(ctx, key, limit)
	size := int64(-1)
	if err == nil {
		size = int64(len(raw))
	}
	finishObjectSpan(span, err, size)
	return raw, err
}

// Open's span covers opening only, not reading the body.
func (o tracedObjects) Open(ctx context.Context, key string) (ObjectReader, error) {
	ctx, span := o.begin(ctx, "Open")
	r, err := o.next.Open(ctx, key)
	size := int64(-1)
	if err == nil {
		size = r.Size
	}
	finishObjectSpan(span, err, size)
	return r, err
}

func (o tracedObjects) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	ctx, span := o.begin(ctx, "Stat")
	info, err := o.next.Stat(ctx, key)
	size := int64(-1)
	if err == nil {
		size = info.Size
	}
	finishObjectSpan(span, err, size)
	return info, err
}

func (o tracedObjects) Copy(ctx context.Context, src, dst, ifMatch string) error {
	ctx, span := o.begin(ctx, "Copy")
	err := o.next.Copy(ctx, src, dst, ifMatch)
	finishObjectSpan(span, err, -1)
	return err
}

func (o tracedObjects) Delete(ctx context.Context, key string) error {
	ctx, span := o.begin(ctx, "Delete")
	err := o.next.Delete(ctx, key)
	finishObjectSpan(span, err, -1)
	return err
}

func (o tracedObjects) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	ctx, span := o.begin(ctx, "List")
	err := o.next.List(ctx, prefix, fn)
	finishObjectSpan(span, err, -1)
	return err
}

// PresignGet and PresignPut make no call, so they have no span.
func (o tracedObjects) PresignGet(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	return o.next.PresignGet(ctx, key, lifetime)
}

func (o tracedObjects) PresignPut(ctx context.Context, key string, lifetime time.Duration) (string, error) {
	return o.next.PresignPut(ctx, key, lifetime)
}
