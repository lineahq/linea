package analysis

import (
	"fmt"
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var testTrace = pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// newTraces builds one resource per element of spansPerResource with spans
// named after their span ID byte.
func newTraces(spansPerResource ...[]byte) ptrace.Traces {
	td := ptrace.NewTraces()
	for _, ids := range spansPerResource {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", "svc")
		ss := rs.ScopeSpans().AppendEmpty()
		for _, id := range ids {
			s := ss.Spans().AppendEmpty()
			s.SetTraceID(testTrace)
			s.SetSpanID(pcommon.SpanID{0, 0, 0, 0, 0, 0, 0, id})
			s.SetName(fmt.Sprintf("span%d", id))
			s.Attributes().PutInt("n", int64(id))
		}
	}
	return td
}

func firstSpan(td ptrace.Traces) ptrace.Span {
	return td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
}

func TestDiffTracesFieldChanges(t *testing.T) {
	entity := "span:" + testTrace.String() + ":0000000000000001"
	tests := []struct {
		name   string
		mutate func(td ptrace.Traces)
		want   []Change
	}{
		{
			name:   "attribute added",
			mutate: func(td ptrace.Traces) { firstSpan(td).Attributes().PutStr("http.route", "/x") },
			want:   []Change{{Entity: entity, Path: `attributes["http.route"]`, Op: OpAdded, After: "/x", AfterType: "Str"}},
		},
		{
			name:   "attribute removed",
			mutate: func(td ptrace.Traces) { firstSpan(td).Attributes().Remove("n") },
			want:   []Change{{Entity: entity, Path: `attributes["n"]`, Op: OpRemoved, Before: int64(1), BeforeType: "Int"}},
		},
		{
			name:   "attribute modified",
			mutate: func(td ptrace.Traces) { firstSpan(td).Attributes().PutInt("n", 7) },
			want:   []Change{{Entity: entity, Path: `attributes["n"]`, Op: OpModified, Before: int64(1), After: int64(7), BeforeType: "Int", AfterType: "Int"}},
		},
		{
			name:   "attribute type changed",
			mutate: func(td ptrace.Traces) { firstSpan(td).Attributes().PutStr("n", "1") },
			want:   []Change{{Entity: entity, Path: `attributes["n"]`, Op: OpTypeChanged, Before: int64(1), After: "1", BeforeType: "Int", AfterType: "Str"}},
		},
		{
			name:   "map value added",
			mutate: func(td ptrace.Traces) { firstSpan(td).Attributes().PutEmptyMap("m").PutBool("deep", true) },
			want:   []Change{{Entity: entity, Path: `attributes["m"]`, Op: OpAdded, After: map[string]any{"deep": true}, AfterType: "Map"}},
		},
		{
			name:   "name modified",
			mutate: func(td ptrace.Traces) { firstSpan(td).SetName("renamed") },
			want:   []Change{{Entity: entity, Path: "name", Op: OpModified, Before: "span1", After: "renamed"}},
		},
		{
			name:   "resource attribute added",
			mutate: func(td ptrace.Traces) { td.ResourceSpans().At(0).Resource().Attributes().PutStr("env", "staging") },
			want:   []Change{{Entity: "resource[0]", Path: `attributes["env"]`, Op: OpAdded, After: "staging", AfterType: "Str"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := newTraces([]byte{1})
			after := newTraces([]byte{1})
			tt.mutate(after)
			d := DiffTraces(before, after)
			if !reflect.DeepEqual(d.Changes, tt.want) {
				t.Fatalf("changes = %#v\nwant      %#v", d.Changes, tt.want)
			}
			if len(d.Removed) != 0 || len(d.Added) != 0 || len(d.Unresolved) != 0 {
				t.Fatalf("unexpected record changes: %+v", d)
			}
		})
	}
}

func TestDiffTracesNestedMapChange(t *testing.T) {
	before := newTraces([]byte{1})
	firstSpan(before).Attributes().PutEmptyMap("m").PutInt("a", 1)
	after := newTraces([]byte{1})
	firstSpan(after).Attributes().PutEmptyMap("m").PutInt("a", 2)
	d := DiffTraces(before, after)
	if len(d.Changes) != 1 || d.Changes[0].Path != `attributes["m"]["a"]` || d.Changes[0].Op != OpModified {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffTracesRecordsRemovedAndAdded(t *testing.T) {
	d := DiffTraces(newTraces([]byte{1, 2}), newTraces([]byte{1, 3}))
	if len(d.Removed) != 1 || d.Removed[0].Name != "span2" {
		t.Errorf("removed = %+v", d.Removed)
	}
	if len(d.Added) != 1 || d.Added[0].Name != "span3" {
		t.Errorf("added = %+v", d.Added)
	}
	if len(d.Changes) != 0 {
		t.Errorf("changes = %+v", d.Changes)
	}
}

// TestDiffTracesDoesNotGuessRegrouping: when two resources are merged into
// one, no resource-level change is attributed.
func TestDiffTracesDoesNotGuessRegrouping(t *testing.T) {
	before := newTraces([]byte{1}, []byte{2})
	after := newTraces([]byte{1, 2})
	after.ResourceSpans().At(0).Resource().Attributes().PutStr("merged", "yes")
	d := DiffTraces(before, after)
	for _, c := range d.Changes {
		if c.Entity == "resource[0]" || c.Entity == "resource[1]" {
			t.Fatalf("resource change attributed despite merge: %+v", c)
		}
	}
	if len(d.Unresolved) == 0 {
		t.Fatal("merge not reported as unresolved")
	}
}
