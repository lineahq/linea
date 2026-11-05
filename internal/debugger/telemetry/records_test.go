package telemetry

import (
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pipeline"
)

func TestFingerprintPreservesTypes(t *testing.T) {
	tests := []struct {
		name string
		a, b func(pcommon.Map)
		same bool
	}{
		{"int vs double", func(m pcommon.Map) { m.PutInt("v", 1) }, func(m pcommon.Map) { m.PutDouble("v", 1) }, false},
		{"int vs string", func(m pcommon.Map) { m.PutInt("v", 1) }, func(m pcommon.Map) { m.PutStr("v", "1") }, false},
		{"bytes vs string", func(m pcommon.Map) { m.PutEmptyBytes("v").FromRaw([]byte("a")) }, func(m pcommon.Map) { m.PutStr("v", "a") }, false},
		{"key order", func(m pcommon.Map) { m.PutStr("a", "1"); m.PutStr("b", "2") }, func(m pcommon.Map) { m.PutStr("b", "2"); m.PutStr("a", "1") }, true},
		{"nested", func(m pcommon.Map) { m.PutEmptyMap("m").PutInt("x", 1) }, func(m pcommon.Map) { m.PutEmptyMap("m").PutInt("x", 2) }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := pcommon.NewMap(), pcommon.NewMap()
			tt.a(a)
			tt.b(b)
			if got := Fingerprint(a) == Fingerprint(b); got != tt.same {
				t.Fatalf("same = %v, want %v (%s vs %s)", got, tt.same, Fingerprint(a), Fingerprint(b))
			}
		})
	}
}

func TestRecordsFlattenEveryMetricType(t *testing.T) {
	md := pmetric.NewMetrics()
	ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	ms.AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty().SetIntValue(1)
	ms.AppendEmpty().SetEmptySum().DataPoints().AppendEmpty().SetDoubleValue(2)
	ms.AppendEmpty().SetEmptyHistogram().DataPoints().AppendEmpty().BucketCounts().FromRaw([]uint64{1, 2})
	ms.AppendEmpty().SetEmptyExponentialHistogram().DataPoints().AppendEmpty().SetScale(3)
	ms.AppendEmpty().SetEmptySummary().DataPoints().AppendEmpty().QuantileValues().AppendEmpty().SetValue(4)

	p := FromMetrics(md)
	recs := Records(p)
	if len(recs) != p.Counts().Items {
		t.Fatalf("records = %d, data points = %d", len(recs), p.Counts().Items)
	}
	var types, refs []string
	for _, r := range recs {
		types = append(types, r.Str("metric.type"))
		refs = append(refs, r.Ref(pipeline.SignalMetrics))
	}
	if !slices.Equal(types, []string{"Gauge", "Sum", "Histogram", "ExponentialHistogram", "Summary"}) {
		t.Errorf("types = %v", types)
	}
	if refs[4] != "resource[0].scope[0].metric[4].data_point[0]" {
		t.Errorf("ref = %s", refs[4])
	}
	if v, _ := recs[0].Fields.Get("value"); v.Type() != pcommon.ValueTypeInt {
		t.Errorf("gauge value type = %s", v.Type())
	}
}
