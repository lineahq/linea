package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newTraceRecorder(t *testing.T) (*tracetest.SpanRecorder, trace.TracerProvider) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return recorder, provider
}

// parentContext returns a context holding a span the test then ends, so the
// recorder holds the children and the parent.
func parentContext(provider trace.TracerProvider) (context.Context, trace.Span) {
	return provider.Tracer("test").Start(context.Background(), "parent")
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

// endedChildren returns the ended spans other than the parent.
func endedChildren(recorder *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() != "parent" {
			out = append(out, s)
		}
	}
	return out
}

// everything renders a span's name, status, attributes and events as text.
func everything(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%s", s.Name(), s.Status().Code, s.Status().Description)
	for _, kv := range s.Attributes() {
		fmt.Fprintf(&b, "|%s=%s", kv.Key, kv.Value.Emit())
	}
	for _, e := range s.Events() {
		fmt.Fprintf(&b, "|%s", e.Name)
		for _, kv := range e.Attributes {
			fmt.Fprintf(&b, "|%s=%s", kv.Key, kv.Value.Emit())
		}
	}
	return b.String()
}

// TestStatementSpansNeedAParentAndHoldNoValues: a statement under a span is
// one client span named for its leading keyword with the statement text and no
// argument; one without a parent records nothing; a database error carries its
// SQLSTATE and never its message; no rows is not an error; a batch is one span
// with its size.
func TestStatementSpansNeedAParentAndHoldNoValues(t *testing.T) {
	const sql = "  SELECT id FROM runs WHERE id = $1"
	const secret = "secret-value"
	t.Run("a statement under a parent", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		tr := newPGTracer(provider)
		ctx, parent := parentContext(provider)
		qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: sql, Args: []any{secret}})
		tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{})
		parent.End()
		spans := endedChildren(recorder)
		if len(spans) != 1 {
			t.Fatalf("spans = %d, want 1", len(spans))
		}
		s := spans[0]
		if s.Name() != "SELECT" || s.SpanKind() != trace.SpanKindClient || s.Status().Code != codes.Unset {
			t.Errorf("span = %s kind %v status %v", s.Name(), s.SpanKind(), s.Status())
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Error("the statement span is not a child of the parent")
		}
		for key, want := range map[string]string{
			"db.system.name": "postgresql", "db.operation.name": "SELECT", "db.query.text": sql,
		} {
			if v, ok := spanAttr(s, key); !ok || v.AsString() != want {
				t.Errorf("%s = %q (present %v), want %q", key, v.AsString(), ok, want)
			}
		}
		if strings.Contains(everything(s), secret) {
			t.Errorf("span holds an argument: %s", everything(s))
		}
	})
	t.Run("no parent", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		tr := newPGTracer(provider)
		qctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: sql})
		tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})
		bctx := tr.TraceBatchStart(context.Background(), nil, pgx.TraceBatchStartData{Batch: &pgx.Batch{}})
		tr.TraceBatchEnd(bctx, nil, pgx.TraceBatchEndData{})
		cctx := tr.TraceCopyFromStart(context.Background(), nil, pgx.TraceCopyFromStartData{})
		tr.TraceCopyFromEnd(cctx, nil, pgx.TraceCopyFromEndData{})
		if n := len(recorder.Started()) + len(recorder.Ended()); n != 0 {
			t.Errorf("recorded %d spans without a parent, want none", n)
		}
	})
	t.Run("errors", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			err        error
			wantStatus codes.Code
			wantDesc   string
			wantCode   string
		}{
			{"sqlstate", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23505", Message: "secret detail"}), codes.Error, "23505", "23505"},
			{"other", errors.New("secret detail"), codes.Error, "error", ""},
			{"no rows", fmt.Errorf("scan: %w", pgx.ErrNoRows), codes.Unset, "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				recorder, provider := newTraceRecorder(t)
				tr := newPGTracer(provider)
				ctx, parent := parentContext(provider)
				qctx := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: sql})
				tr.TraceQueryEnd(qctx, nil, pgx.TraceQueryEndData{Err: tc.err})
				parent.End()
				spans := endedChildren(recorder)
				if len(spans) != 1 {
					t.Fatalf("spans = %d, want 1", len(spans))
				}
				s := spans[0]
				if s.Status().Code != tc.wantStatus || s.Status().Description != tc.wantDesc {
					t.Errorf("status = %v %q, want %v %q", s.Status().Code, s.Status().Description, tc.wantStatus, tc.wantDesc)
				}
				v, ok := spanAttr(s, "db.response.status_code")
				if tc.wantCode == "" && ok || tc.wantCode != "" && v.AsString() != tc.wantCode {
					t.Errorf("db.response.status_code = %q (present %v), want %q", v.AsString(), ok, tc.wantCode)
				}
				if strings.Contains(everything(s), "secret detail") {
					t.Errorf("span holds an error message: %s", everything(s))
				}
			})
		}
	})
	t.Run("a batch", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		tr := newPGTracer(provider)
		ctx, parent := parentContext(provider)
		batch := &pgx.Batch{}
		batch.Queue("INSERT INTO runs (id) VALUES ($1)", secret)
		batch.Queue("UPDATE runs SET state = $1", secret)
		bctx := tr.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{Batch: batch})
		tr.TraceBatchQuery(bctx, nil, pgx.TraceBatchQueryData{SQL: "INSERT INTO runs (id) VALUES ($1)", Args: []any{secret}})
		tr.TraceBatchEnd(bctx, nil, pgx.TraceBatchEndData{})
		parent.End()
		spans := endedChildren(recorder)
		if len(spans) != 1 || spans[0].Name() != "BATCH" || spans[0].SpanKind() != trace.SpanKindClient {
			t.Fatalf("spans = %v, want one BATCH client span", spans)
		}
		if v, ok := spanAttr(spans[0], "db.operation.batch.size"); !ok || v.AsInt64() != 2 {
			t.Errorf("db.operation.batch.size = %v (present %v), want 2", v.AsInt64(), ok)
		}
		if strings.Contains(everything(spans[0]), secret) {
			t.Errorf("span holds an argument: %s", everything(spans[0]))
		}
	})
	t.Run("a copy", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		tr := newPGTracer(provider)
		ctx, parent := parentContext(provider)
		cctx := tr.TraceCopyFromStart(ctx, nil, pgx.TraceCopyFromStartData{TableName: pgx.Identifier{"secret_table"}})
		tr.TraceCopyFromEnd(cctx, nil, pgx.TraceCopyFromEndData{})
		parent.End()
		spans := endedChildren(recorder)
		if len(spans) != 1 || spans[0].Name() != "COPY" || strings.Contains(everything(spans[0]), "secret_table") {
			t.Fatalf("spans = %v, want one COPY span without the table", spans)
		}
	})
}

func TestSQLOperation(t *testing.T) {
	for sql, want := range map[string]string{
		"select 1":                             "SELECT",
		"\n\t  insert into t values ($1)":      "INSERT",
		"-- note\nUPDATE t SET a = 1":          "UPDATE",
		"/* note */ delete from t":             "DELETE",
		"(SELECT 1) UNION (SELECT 2)":          "SELECT",
		"WITH x AS (SELECT 1) SELECT * FROM x": "WITH",
		"":                                     "",
		"-- only a comment":                    "",
		"1":                                    "",
	} {
		if got := sqlOperation(sql); got != want {
			t.Errorf("sqlOperation(%q) = %q, want %q", sql, got, want)
		}
	}
}

// TestObjectSpansNeedAParentAndHoldNoKeys: each object storage call under a
// span is one client span named for the method with the bytes it moved and no
// key; a missing object is not an error; without a parent there is no span.
func TestObjectSpansNeedAParentAndHoldNoKeys(t *testing.T) {
	const key = "secret-prefix/sha256/secret-key"
	t.Run("under a parent", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		objects := newTracedObjects(newMemObjects(), provider)
		ctx, parent := parentContext(provider)
		data := []byte("hello world")
		if err := objects.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
		if raw, err := objects.Get(ctx, key, 100); err != nil || len(raw) != len(data) {
			t.Fatalf("Get = %d bytes, %v", len(raw), err)
		}
		if r, err := objects.Open(ctx, key); err != nil {
			t.Fatal(err)
		} else {
			_ = r.Body.Close()
		}
		if _, err := objects.Stat(ctx, key); err != nil {
			t.Fatal(err)
		}
		if err := objects.Copy(ctx, key, key+"-copy", ""); err != nil {
			t.Fatal(err)
		}
		if err := objects.List(ctx, "secret-prefix/", func(ObjectInfo) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.PresignGet(ctx, key, 0); err != nil {
			t.Logf("presign on the fake: %v", err)
		}
		if err := objects.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.Get(ctx, key, 100); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("Get of a deleted object = %v, want ErrObjectNotFound", err)
		}
		parent.End()

		want := []struct {
			name  string
			bytes int64 // -1: no bytes attribute
		}{
			{"s3 Put", int64(len(data))}, {"s3 Get", int64(len(data))}, {"s3 Open", int64(len(data))},
			{"s3 Stat", int64(len(data))}, {"s3 Copy", -1}, {"s3 List", -1}, {"s3 Delete", -1}, {"s3 Get", -1},
		}
		spans := endedChildren(recorder)
		if len(spans) != len(want) {
			t.Fatalf("spans = %d, want %d", len(spans), len(want))
		}
		for i, w := range want {
			s := spans[i]
			if s.Name() != w.name || s.SpanKind() != trace.SpanKindClient {
				t.Errorf("span %d = %s kind %v, want %s client", i, s.Name(), s.SpanKind(), w.name)
			}
			if s.Status().Code != codes.Unset {
				t.Errorf("%s status = %v, want unset (a missing object is not an error)", s.Name(), s.Status())
			}
			if v, ok := spanAttr(s, "rpc.system"); !ok || v.AsString() != "aws-api" {
				t.Errorf("%s rpc.system = %q", s.Name(), v.AsString())
			}
			if v, ok := spanAttr(s, "rpc.method"); !ok || "s3 "+v.AsString() != w.name {
				t.Errorf("%s rpc.method = %q", s.Name(), v.AsString())
			}
			v, ok := spanAttr(s, "linea.object.bytes")
			switch {
			case w.bytes < 0 && ok:
				t.Errorf("%s has bytes %d, want none", s.Name(), v.AsInt64())
			case w.bytes >= 0 && (!ok || v.AsInt64() != w.bytes):
				t.Errorf("%s bytes = %d (present %v), want %d", s.Name(), v.AsInt64(), ok, w.bytes)
			}
			if txt := everything(s); strings.Contains(txt, "secret-") {
				t.Errorf("span holds a key: %s", txt)
			}
		}
	})
	t.Run("an error", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		objects := newTracedObjects(failingObjects{newMemObjects()}, provider)
		ctx, parent := parentContext(provider)
		if err := objects.Put(ctx, key, []byte("x")); err == nil {
			t.Fatal("Put succeeded")
		}
		parent.End()
		spans := endedChildren(recorder)
		if len(spans) != 1 || spans[0].Status().Code != codes.Error || spans[0].Status().Description != "error" {
			t.Fatalf("spans = %v, want one error span", spans)
		}
		if strings.Contains(everything(spans[0]), "secret") {
			t.Errorf("span holds an error message: %s", everything(spans[0]))
		}
	})
	t.Run("no parent", func(t *testing.T) {
		recorder, provider := newTraceRecorder(t)
		mem := newMemObjects()
		objects := newTracedObjects(mem, provider)
		ctx := context.Background()
		if err := objects.Put(ctx, key, []byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.Get(ctx, key, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := objects.Stat(ctx, "missing"); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("Stat = %v", err)
		}
		if err := objects.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		if n := len(recorder.Started()) + len(recorder.Ended()); n != 0 {
			t.Errorf("recorded %d spans without a parent, want none", n)
		}
	})
}

// failingObjects fails Put with an error whose message must not reach a span.
type failingObjects struct{ ObjectStore }

func (failingObjects) Put(context.Context, string, []byte) error {
	return errors.New("secret message")
}
