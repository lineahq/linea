package pg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// scriptedObjects serves Open from a body the test scripts: it reads data,
// then readErr (io.EOF when nil), and counts closes.
type scriptedObjects struct {
	ObjectStore
	data    []byte
	readErr error
	openErr error
	closeFn func() error
	closed  int
}

func (o *scriptedObjects) Open(context.Context, string) (ObjectReader, error) {
	if o.openErr != nil {
		return ObjectReader{}, o.openErr
	}
	return ObjectReader{Body: &scriptedBody{o: o, r: bytes.NewReader(o.data)}, Size: int64(len(o.data))}, nil
}

type scriptedBody struct {
	o *scriptedObjects
	r *bytes.Reader
}

func (b *scriptedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF && b.o.readErr != nil {
		err = b.o.readErr
	}
	return n, err
}

func (b *scriptedBody) Close() error {
	b.o.closed++
	if b.o.closeFn != nil {
		return b.o.closeFn()
	}
	return nil
}

// openSpans returns the ended "s3 Open" spans.
func openSpans(recorder *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "s3 Open" {
			out = append(out, s)
		}
	}
	return out
}

// TestOpenSpanEndsWhenTheBodyIsClosed: the s3 Open span covers the read, so it
// is still open while the body is read, even to its end, and ends at Close
// with the object's size; closing again ends nothing more.
func TestOpenSpanEndsWhenTheBodyIsClosed(t *testing.T) {
	recorder, provider := newTraceRecorder(t)
	data := []byte("hello world")
	next := &scriptedObjects{data: data}
	objects := newTracedObjects(next, provider)
	ctx, parent := parentContext(provider)

	r, err := objects.Open(ctx, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(openSpans(recorder)); n != 0 {
		t.Fatalf("%d Open spans ended before the read", n)
	}
	got, err := io.ReadAll(r.Body)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read = %q, %v", got, err)
	}
	if n := len(openSpans(recorder)); n != 0 {
		t.Fatalf("%d Open spans ended at EOF, before Close", n)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	spans := openSpans(recorder)
	if len(spans) != 1 {
		t.Fatalf("Open spans after Close = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Status().Code != codes.Unset {
		t.Errorf("status = %v after a complete read, want unset", s.Status())
	}
	if v, ok := spanAttr(s, "linea.object.bytes"); !ok || v.AsInt64() != int64(len(data)) {
		t.Errorf("linea.object.bytes = %d (present %v), want %d", v.AsInt64(), ok, len(data))
	}
	endTime := s.EndTime()

	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if next.closed != 2 {
		t.Errorf("the body was closed %d times, want each Close to reach it", next.closed)
	}
	parent.End()
	spans = openSpans(recorder)
	if len(spans) != 1 || !spans[0].EndTime().Equal(endTime) || spans[0].Status().Code != codes.Unset {
		t.Errorf("a second Close changed the span: %d spans, end %v then %v", len(spans), endTime, spans[0].EndTime())
	}
	if txt := everything(s); strings.Contains(txt, "secret") {
		t.Errorf("span holds a key: %s", txt)
	}
	if s.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("the Open span is not a child of the parent")
	}
}

// TestOpenSpanReportsAFailedRead: a read error other than EOF gives the span
// error status when the body is closed, whatever the later reads return, and
// its message never reaches the span; EOF is not an error.
func TestOpenSpanReportsAFailedRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readErr error
		want    codes.Code
	}{
		{"the end of the object", nil, codes.Unset},
		{"a failed read", errors.New("secret connection reset"), codes.Error},
		{"an unexpected end of the object", io.ErrUnexpectedEOF, codes.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, provider := newTraceRecorder(t)
			objects := newTracedObjects(&scriptedObjects{data: []byte("abc"), readErr: tc.readErr}, provider)
			ctx, parent := parentContext(provider)
			r, err := objects.Open(ctx, "key")
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := io.ReadAll(r.Body)
			if tc.readErr != nil && !errors.Is(readErr, tc.readErr) {
				t.Fatalf("read error = %v, want %v passed through", readErr, tc.readErr)
			}
			if tc.readErr == nil && readErr != nil {
				t.Fatalf("read error = %v", readErr)
			}
			// Reading again after the failure does not clear it.
			_, _ = r.Body.Read(make([]byte, 1))
			if n := len(openSpans(recorder)); n != 0 {
				t.Fatalf("%d Open spans ended before Close", n)
			}
			_ = r.Body.Close()
			parent.End()
			spans := openSpans(recorder)
			if len(spans) != 1 {
				t.Fatalf("Open spans = %d, want 1", len(spans))
			}
			if got := spans[0].Status(); got.Code != tc.want || tc.want == codes.Error && got.Description != "error" {
				t.Errorf("status = %+v, want code %v", got, tc.want)
			}
			if txt := everything(spans[0]); strings.Contains(txt, "secret") {
				t.Errorf("span holds an error message: %s", txt)
			}
		})
	}
}

// TestOpenSpanEndsAtOnceWhenOpenFails: with no body to close, a failed Open
// ends its span itself; a missing object is not an error.
func TestOpenSpanEndsAtOnceWhenOpenFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want codes.Code
	}{
		{"a missing object", ErrObjectNotFound, codes.Unset},
		{"a failure", errors.New("secret failure"), codes.Error},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder, provider := newTraceRecorder(t)
			objects := newTracedObjects(&scriptedObjects{openErr: tc.err}, provider)
			ctx, parent := parentContext(provider)
			r, err := objects.Open(ctx, "key")
			if !errors.Is(err, tc.err) || r.Body != nil {
				t.Fatalf("Open = %+v, %v", r, err)
			}
			spans := openSpans(recorder)
			if len(spans) != 1 || spans[0].Status().Code != tc.want {
				t.Fatalf("Open spans = %v, want one with status %v", spans, tc.want)
			}
			if _, ok := spanAttr(spans[0], "linea.object.bytes"); ok {
				t.Error("a failed Open recorded bytes")
			}
			if txt := everything(spans[0]); strings.Contains(txt, "secret") {
				t.Errorf("span holds an error message: %s", txt)
			}
			parent.End()
		})
	}
}

// TestOpenWithoutAParentRecordsNothingAndWrapsNothing: no span starts, and the
// body is the store's own.
func TestOpenWithoutAParentRecordsNothingAndWrapsNothing(t *testing.T) {
	recorder, provider := newTraceRecorder(t)
	next := &scriptedObjects{data: []byte("abc")}
	objects := newTracedObjects(next, provider)
	r, err := objects.Open(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := r.Body.(*tracedBody); wrapped {
		t.Error("a call without a parent wrapped its body")
	}
	if got, err := io.ReadAll(r.Body); err != nil || string(got) != "abc" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if next.closed != 1 {
		t.Errorf("closed %d times, want 1", next.closed)
	}
	if n := len(recorder.Started()) + len(recorder.Ended()); n != 0 {
		t.Errorf("recorded %d spans without a parent, want none", n)
	}
	if trace.SpanContextFromContext(context.Background()).IsValid() {
		t.Fatal("the background context has a span")
	}
}

// TestTracedBodyClosePassesTheBodysError: Close returns the body's error and
// still ends the span.
func TestTracedBodyClosePassesTheBodysError(t *testing.T) {
	recorder, provider := newTraceRecorder(t)
	closeErr := errors.New("close failed")
	objects := newTracedObjects(&scriptedObjects{data: []byte("abc"), closeFn: func() error { return closeErr }}, provider)
	ctx, parent := parentContext(provider)
	r, err := objects.Open(ctx, "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close = %v, want the body's error", err)
	}
	parent.End()
	if n := len(openSpans(recorder)); n != 1 {
		t.Errorf("Open spans = %d, want 1", n)
	}
}
