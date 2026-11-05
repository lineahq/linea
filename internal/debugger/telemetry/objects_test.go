package telemetry

import (
	"slices"
	"testing"
	"unsafe"

	"go.opentelemetry.io/collector/featuregate"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func TestRecordWrapperLayoutIsVerified(t *testing.T) {
	if !layoutVerified() {
		t.Fatal("the pdata record wrappers no longer start with their object pointer: " +
			"re-verify Objects against the new pdata release (docs/internals/record-objects.md)")
	}
}

// marks lists each record's "n" attribute in Records order.
func marks(t *testing.T, p Payload) []int64 {
	t.Helper()
	var out []int64
	for _, r := range Records(p) {
		attrs, ok := r.Fields.Get("attributes")
		if !ok {
			t.Fatal("record without attributes")
		}
		n, ok := attrs.Map().Get("n")
		if !ok {
			t.Fatal("record without n")
		}
		out = append(out, n.Int())
	}
	return out
}

// objectsByMark maps each record's "n" to its object.
func objectsByMark(t *testing.T, p Payload) map[int64]unsafe.Pointer {
	t.Helper()
	objs, ok := Objects(p)
	if !ok {
		t.Fatal("objects unavailable")
	}
	ns := marks(t, p)
	if len(objs) != len(ns) {
		t.Fatalf("objects = %d, records = %d", len(objs), len(ns))
	}
	out := map[int64]unsafe.Pointer{}
	for i, n := range ns {
		if _, dup := out[n]; dup {
			t.Fatalf("mark %d twice", n)
		}
		if slices.Contains(objs[:i], objs[i]) {
			t.Fatalf("record %d shares an object", i)
		}
		out[n] = objs[i]
	}
	return out
}

// TestObjectsFollowRecordsThroughMoves checks that Objects lists one object
// per record in Records order and that a record keeps its object when it is
// moved, sorted or its neighbour removed, while a copy is a new object.
func TestObjectsFollowRecordsThroughMoves(t *testing.T) {
	tests := []struct {
		name  string
		build func() Payload
		move  func(Payload)
		copy  func(Payload) Payload
	}{
		{"logs", func() Payload {
			ld := plog.NewLogs()
			var n int64
			for range 2 {
				rl := ld.ResourceLogs().AppendEmpty()
				for range 2 {
					sl := rl.ScopeLogs().AppendEmpty()
					for range 3 {
						sl.LogRecords().AppendEmpty().Attributes().PutInt("n", n)
						n++
					}
				}
			}
			return FromLogs(ld)
		}, func(p Payload) {
			rls := p.Logs.ResourceLogs()
			first, last := rls.At(0).ScopeLogs().At(0).LogRecords(), rls.At(1).ScopeLogs().At(1).LogRecords()
			first.RemoveIf(func(lr plog.LogRecord) bool { v, _ := lr.Attributes().Get("n"); return v.Int() == 1 })
			first.MoveAndAppendTo(last)
			last.Sort(func(a, b plog.LogRecord) bool {
				x, _ := a.Attributes().Get("n")
				y, _ := b.Attributes().Get("n")
				return x.Int() > y.Int()
			})
		}, func(p Payload) Payload {
			ld := plog.NewLogs()
			p.Logs.CopyTo(ld)
			return FromLogs(ld)
		}},
		{"spans", func() Payload {
			td := ptrace.NewTraces()
			ss := td.ResourceSpans().AppendEmpty().ScopeSpans()
			for i := range int64(6) {
				if i%3 == 0 {
					ss.AppendEmpty()
				}
				ss.At(ss.Len()-1).Spans().AppendEmpty().Attributes().PutInt("n", i)
			}
			return FromTraces(td)
		}, func(p Payload) {
			ss := p.Traces.ResourceSpans().At(0).ScopeSpans()
			ss.At(1).Spans().RemoveIf(func(s ptrace.Span) bool { v, _ := s.Attributes().Get("n"); return v.Int() == 4 })
			ss.At(1).Spans().MoveAndAppendTo(ss.At(0).Spans())
		}, func(p Payload) Payload {
			td := ptrace.NewTraces()
			p.Traces.CopyTo(td)
			return FromTraces(td)
		}},
		{"metrics", func() Payload {
			md := pmetric.NewMetrics()
			ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
			var n int64
			mark := func(attrs pcommon.Map) { attrs.PutInt("n", n); n++ }
			for range 2 {
				g := ms.AppendEmpty().SetEmptyGauge().DataPoints()
				mark(g.AppendEmpty().Attributes())
				mark(g.AppendEmpty().Attributes())
				mark(ms.AppendEmpty().SetEmptySum().DataPoints().AppendEmpty().Attributes())
				mark(ms.AppendEmpty().SetEmptyHistogram().DataPoints().AppendEmpty().Attributes())
				mark(ms.AppendEmpty().SetEmptyExponentialHistogram().DataPoints().AppendEmpty().Attributes())
				mark(ms.AppendEmpty().SetEmptySummary().DataPoints().AppendEmpty().Attributes())
			}
			ms.AppendEmpty().SetEmptyGauge() // no points
			return FromMetrics(md)
		}, func(p Payload) {
			ms := p.Metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
			ms.At(0).Gauge().DataPoints().RemoveIf(func(dp pmetric.NumberDataPoint) bool {
				v, _ := dp.Attributes().Get("n")
				return v.Int() == 0
			})
			ms.At(5).Gauge().DataPoints().MoveAndAppendTo(ms.At(0).Gauge().DataPoints())
			ms.At(9).Summary().DataPoints().MoveAndAppendTo(ms.At(4).Summary().DataPoints())
		}, func(p Payload) Payload {
			md := pmetric.NewMetrics()
			p.Metrics.CopyTo(md)
			return FromMetrics(md)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.build()
			before := objectsByMark(t, p)
			if len(before) != p.Counts().Items {
				t.Fatalf("objects = %d, items = %d", len(before), p.Counts().Items)
			}
			copied := objectsByMark(t, tt.copy(p))
			tt.move(p)
			after := objectsByMark(t, p)
			if len(after) != len(before)-1 {
				t.Fatalf("after the moves %d records, want %d", len(after), len(before)-1)
			}
			for n, o := range after {
				if before[n] != o {
					t.Errorf("record %d changed object when moved", n)
				}
			}
			for n, o := range copied {
				if before[n] == o {
					t.Errorf("copy of record %d kept its object", n)
				}
			}
		})
	}
}

func TestPoolingTurnsObjectsOff(t *testing.T) {
	if poolingFeature() == nil {
		t.Fatalf("pdata no longer registers %s", poolingGate)
	}
	reg := featuregate.GlobalRegistry()
	if err := reg.Set(poolingGate, true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Set(poolingGate, false) })
	ld := plog.NewLogs()
	ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	if objs, ok := Objects(FromLogs(ld)); ok || objs != nil {
		t.Fatalf("objects with pooling = %v, %v", objs, ok)
	}
}
