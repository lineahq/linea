package telemetry

import (
	"bytes"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// TestSortMetricsIgnoresEmissionOrder: the same metrics and data points
// emitted in different orders, as the count connector does, encode
// identically once sorted, and the input payload is left unchanged.
func TestSortMetricsIgnoresEmissionOrder(t *testing.T) {
	build := func(reversed bool) Payload {
		md := pmetric.NewMetrics()
		ms := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
		names := []string{"b.count", "a.count"}
		services := []string{"payments", "checkout"}
		if reversed {
			names[0], names[1] = names[1], names[0]
			services[0], services[1] = services[1], services[0]
		}
		for _, n := range names {
			m := ms.AppendEmpty()
			m.SetName(n)
			dps := m.SetEmptySum().DataPoints()
			for _, s := range services {
				dp := dps.AppendEmpty()
				dp.Attributes().PutStr("service.name", s)
				dp.SetIntValue(int64(len(s)))
			}
		}
		g := ms.AppendEmpty()
		g.SetName("a.count")
		g.SetEmptyGauge().DataPoints().AppendEmpty().SetDoubleValue(1.5)
		return FromMetrics(md)
	}
	in := build(false)
	before, err := in.JSON()
	if err != nil {
		t.Fatal(err)
	}
	a, err := SortMetrics(in).JSON()
	if err != nil {
		t.Fatal(err)
	}
	b, err := SortMetrics(build(true)).JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("sorted payloads differ:\n%s\n%s", a, b)
	}
	if after, _ := in.JSON(); !bytes.Equal(after, before) {
		t.Fatal("SortMetrics changed its input")
	}
	if m := SortMetrics(in).Metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0); m.Name() != "a.count" || m.Type() != pmetric.MetricTypeGauge {
		t.Errorf("first metric = %s %s, want the a.count gauge", m.Name(), m.Type())
	}
}
