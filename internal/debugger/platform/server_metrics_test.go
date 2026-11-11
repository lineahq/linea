package platform

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// collected is one collection's data points by metric name and attribute
// set: sums and gauges by value, histograms by count.
type collected map[string]map[attribute.Distinct]int64

func collect(t *testing.T, reader *sdkmetric.ManualReader) collected {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := collected{}
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			t.Fatalf("scope %q", sm.Scope.Name)
		}
		for _, m := range sm.Metrics {
			points := map[attribute.Distinct]int64{}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					points[p.Attributes.Equivalent()] = p.Value
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					points[p.Attributes.Equivalent()] = p.Value
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					points[p.Attributes.Equivalent()] = int64(p.Count)
				}
			default:
				t.Fatalf("%s: unexpected data %T", m.Name, m.Data)
			}
			out[m.Name] = points
		}
	}
	return out
}

func (c collected) value(t *testing.T, name string, kv ...attribute.KeyValue) int64 {
	t.Helper()
	points, ok := c[name]
	if !ok {
		t.Fatalf("%s not collected", name)
	}
	set := attribute.NewSet(kv...)
	return points[set.Equivalent()]
}

// TestServerMetricsRecordTheSchedulerAndAStorageOutage: a claimed run records
// its wait, each heartbeat its lease renewals, and once the store stops
// answering, renewals fail, an audit event is counted as not recorded, and
// after a whole lease the storage is not ready, in StorageReady and in
// linea.storage.ready.
func TestServerMetricsRecordTheSchedulerAndAStorageOutage(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	clock := newTestClock()
	s, kill := openKillable(t, memory.NewData(), Options{Now: clock.Now, MeterProvider: provider,
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return report(req, "completed"), nil
		}})

	run := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if v := waitRun(t, run); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, run.Err())
	}
	s.heartbeat(clock.Now())
	c := collect(t, reader)
	if n := c.value(t, "linea.run.claim.wait"); n != 1 {
		t.Errorf("claim waits = %d, want 1", n)
	}
	if n := c.value(t, "linea.run.lease.renewals", attribute.String("outcome", "renewed")); n < 1 {
		t.Errorf("renewed leases = %d", n)
	}
	if n := c.value(t, "linea.run.queue.size"); n != 0 {
		t.Errorf("queue size = %d", n)
	}
	if n := c.value(t, "linea.storage.ready"); n != 1 || !s.StorageReady() {
		t.Errorf("storage ready = %d, %v", n, s.StorageReady())
	}

	kill()
	s.heartbeat(clock.Advance(DefaultLease / 3))
	if !s.StorageReady() {
		t.Error("one unanswered scan within a lease made the storage not ready")
	}
	s.recordAudit(artifact.AuditEvent{Time: clock.Now(), Operation: "ListRuns"})
	s.heartbeat(clock.Advance(DefaultLease))
	c = collect(t, reader)
	if n := c.value(t, "linea.run.lease.renewals", attribute.String("outcome", "failed")); n < 2 {
		t.Errorf("failed renewals = %d, want at least 2", n)
	}
	if n := c.value(t, "linea.audit.append.failures"); n != 1 {
		t.Errorf("audit failures = %d, want 1", n)
	}
	if n := c.value(t, "linea.storage.ready"); n != 0 || s.StorageReady() {
		t.Errorf("after a lease without the store, storage ready = %d, %v", n, s.StorageReady())
	}
}

// TestServerMetricsWithoutAProviderRecordNothing: a service without a meter
// provider runs as before.
func TestServerMetricsWithoutAProviderRecordNothing(t *testing.T) {
	s := openPlatform(t, memory.NewData(), Options{Now: time.Now,
		Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
			return report(req, "completed"), nil
		}})
	run := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if v := waitRun(t, run); v.State != StateDone {
		t.Fatalf("run = %s, %v", v.State, run.Err())
	}
	s.heartbeat(time.Now())
	if !s.StorageReady() {
		t.Fatal("storage not ready")
	}
}
