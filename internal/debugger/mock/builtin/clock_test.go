package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/connector/spanmetricsconnector"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/connector/connectortest"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

var clockOrigin = time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)

func oneSpan() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	s := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	s.SetName("POST /orders")
	s.SetTraceID(pcommon.TraceID{1})
	s.SetSpanID(pcommon.SpanID{2})
	s.SetStartTimestamp(pcommon.NewTimestampFromTime(clockOrigin.Add(-time.Second)))
	s.SetEndTimestamp(pcommon.NewTimestampFromTime(clockOrigin))
	return td
}

// calls returns the calls sum points of each export as (value, time).
func calls(t *testing.T, exports []pmetric.Metrics) [][2]int64 {
	t.Helper()
	var out [][2]int64
	for _, md := range exports {
		for _, rm := range md.ResourceMetrics().All() {
			for _, sm := range rm.ScopeMetrics().All() {
				for _, m := range sm.Metrics().All() {
					if strings.HasSuffix(m.Name(), ".calls") {
						for _, dp := range m.Sum().DataPoints().All() {
							out = append(out, [2]int64{dp.IntValue(), int64(dp.Timestamp().AsTime().Sub(clockOrigin))})
						}
					}
				}
			}
		}
	}
	return out
}

// TestClockFlushesAggregatedSpansAtShutdown: the real spanmetrics connector
// exports nothing on shutdown; on the simulated clock the run's end flushes
// what it aggregated, at flush interval multiples of the clock's start. A
// cumulative series first reports 0, so it takes two flushes.
func TestClockFlushesAggregatedSpansAtShutdown(t *testing.T) {
	for _, tt := range []struct {
		temporality string
		want        [][2]int64
	}{
		{"AGGREGATION_TEMPORALITY_CUMULATIVE", [][2]int64{{0, int64(30 * time.Second)}, {1, int64(time.Minute)}}},
		{"AGGREGATION_TEMPORALITY_DELTA", [][2]int64{{1, int64(30 * time.Second)}}},
	} {
		t.Run(tt.temporality, func(t *testing.T) {
			p := &clockProvider{origin: clockOrigin, flushes: map[component.ID][]time.Time{}, errs: map[component.ID]string{}}
			f, ok := p.Connector(spanmetricsconnector.NewFactory())
			if !ok {
				t.Fatal("the provider does not handle spanmetrics")
			}
			cfg := f.CreateDefaultConfig().(*spanmetricsconnector.Config)
			cfg.AggregationTemporality = tt.temporality
			cfg.MetricsFlushInterval = 30 * time.Second
			sink := new(consumertest.MetricsSink)
			set := connectortest.NewNopSettings(f.Type())
			c, err := f.CreateTracesToMetrics(context.Background(), set, cfg, sink)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Start(context.Background(), componenttest.NewNopHost()); err != nil {
				t.Fatal(err)
			}
			if err := c.ConsumeTraces(context.Background(), oneSpan()); err != nil {
				t.Fatal(err)
			}
			if err := c.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := calls(t, sink.AllMetrics())
			if len(got) != len(tt.want) {
				t.Fatalf("calls points = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("calls points = %v, want %v", got, tt.want)
				}
			}
			if n := len(p.flushes[set.ID]); n != len(tt.want) {
				t.Fatalf("declared %d flushes, want %d", n, len(tt.want))
			}
		})
	}
}

// blockingMetrics never returns from an export until released.
type blockingMetrics struct {
	consumertest.MetricsSink
	release chan struct{}
}

func (b *blockingMetrics) ConsumeMetrics(context.Context, pmetric.Metrics) error {
	<-b.release
	return errors.New("released")
}

func (b *blockingMetrics) Capabilities() consumer.Capabilities { return consumer.Capabilities{} }

// TestClockFlushGivesUpWithTheShutdownContext: a downstream pipeline that
// never accepts the flush does not hang the run; the declaration warns.
func TestClockFlushGivesUpWithTheShutdownContext(t *testing.T) {
	p := &clockProvider{origin: clockOrigin, ids: []component.ID{component.MustNewID("spanmetrics")},
		flushes: map[component.ID][]time.Time{}, errs: map[component.ID]string{}}
	f, _ := p.Connector(spanmetricsconnector.NewFactory())
	next := &blockingMetrics{release: make(chan struct{})}
	defer close(next.release)
	set := connectortest.NewNopSettings(f.Type())
	set.ID = component.MustNewID("spanmetrics")
	c, err := f.CreateTracesToMetrics(context.Background(), set, f.CreateDefaultConfig(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = c.Shutdown(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited past its context")
	}
	d := p.declarations()
	if len(d) != 1 || len(d[0].Warnings) != 1 || !strings.Contains(d[0].Warnings[0], "before the simulated flush") {
		t.Fatalf("declarations = %+v", d)
	}
}
