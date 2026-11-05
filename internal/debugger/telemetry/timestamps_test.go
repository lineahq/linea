package telemetry

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// TestShiftMovesEverySetTimestamp: span times and event times, data point
// times and exemplars move; unset timestamps stay unset.
func TestShiftMovesEverySetTimestamp(t *testing.T) {
	td := ptrace.NewTraces()
	s := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.SetStartTimestamp(100)
	s.SetEndTimestamp(300)
	s.Events().AppendEmpty().SetTimestamp(200)
	traces := FromTraces(td)

	md := pmetric.NewMetrics()
	dp := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptySum().DataPoints().AppendEmpty()
	dp.SetTimestamp(400)
	dp.Exemplars().AppendEmpty().SetTimestamp(350)
	metrics := FromMetrics(md)

	if got := Latest(traces, metrics); !got.Equal(pcommon.Timestamp(400).AsTime()) {
		t.Fatalf("latest = %v", got)
	}
	traces.Shift(time.Duration(1000))
	metrics.Shift(time.Duration(1000))
	if s.StartTimestamp() != 1100 || s.EndTimestamp() != 1300 || s.Events().At(0).Timestamp() != 1200 {
		t.Fatalf("span times = %d, %d, %d", s.StartTimestamp(), s.EndTimestamp(), s.Events().At(0).Timestamp())
	}
	if dp.StartTimestamp() != 0 || dp.Timestamp() != 1400 || dp.Exemplars().At(0).Timestamp() != 1350 {
		t.Fatalf("point times = %d, %d, %d", dp.StartTimestamp(), dp.Timestamp(), dp.Exemplars().At(0).Timestamp())
	}
	if !Latest(FromTraces(ptrace.NewTraces())).IsZero() {
		t.Fatal("an empty payload has a latest time")
	}
}
