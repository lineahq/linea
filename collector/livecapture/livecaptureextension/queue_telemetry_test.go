package livecaptureextension

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestQueueGaugePreservesStockSDKMeasurements(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })
	queues := &queueObservations{}
	if _, err := newTelemetry(provider, queues); err != nil {
		t.Fatal(err)
	}
	wrapper := queueMeterProvider{MeterProvider: provider, queues: queues}
	var registrations []metric.Registration
	for i, signal := range []string{"traces", "logs"} {
		meter := wrapper.Meter("stock-exporter-fixture")
		gauge, err := meter.Int64ObservableGauge("otelcol_exporter_queue_size")
		if err != nil {
			t.Fatal(err)
		}
		registration, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(gauge, int64((i+1)*100), metric.WithAttributes(attribute.String("data_type", signal)))
			return nil
		}, gauge)
		if err != nil {
			t.Fatal(err)
		}
		registrations = append(registrations, registration)
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal(err)
	}
	stockCount, captureCount := 0, 0
	for _, scope := range collected.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			gauge, ok := measurement.Data.(metricdata.Gauge[int64])
			if !ok {
				continue
			}
			switch measurement.Name {
			case "otelcol_exporter_queue_size":
				stockCount += len(gauge.DataPoints)
			case "otelcol_capture_queue_bytes":
				captureCount += len(gauge.DataPoints)
				if len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 300 || gauge.DataPoints[0].Attributes.Len() != 0 {
					t.Fatal("pod-wide gauge not summed without labels")
				}
			}
		}
	}
	if stockCount != 2 || captureCount != 1 {
		t.Fatalf("stock=%d capture=%d", stockCount, captureCount)
	}
	for _, registration := range registrations {
		if err := registration.Unregister(); err != nil {
			t.Fatal(err)
		}
	}
	if queues.sum(ctx) != 0 {
		t.Fatal("retained queue observation after unregister")
	}
}

func TestCachedCaptureLabelsPreserveSDKSeries(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })
	m, _ := testManager(t)
	telemetry, err := newTelemetry(provider, &queueObservations{})
	if err != nil {
		t.Fatal(err)
	}
	m.telemetry = telemetry
	r := traceRequest("cached-labels")
	r.Limits.RecordsPerSecond = 1
	start(t, m, r)
	m.CaptureTraces(ctx, r.CapturePoints[0], testTraces()) // two spans, one rate drop
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &collected); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, scope := range collected.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			switch data := measurement.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					signal, _ := point.Attributes.Value(attribute.Key("signal"))
					if signal.AsString() != "traces" {
						continue
					}
					switch measurement.Name {
					case "otelcol_capture_records_selected_total":
						capturePoint, _ := point.Attributes.Value(attribute.Key("capture_point"))
						seen[measurement.Name] = point.Value == 2 && capturePoint.AsString() == "ingress"
					case "otelcol_capture_bytes_enqueued_total":
						seen[measurement.Name] = point.Value > 0
					case "otelcol_capture_records_dropped_total":
						reason, _ := point.Attributes.Value(attribute.Key("reason"))
						seen[measurement.Name] = point.Value == 1 && reason.AsString() == "rate_records"
					}
				}
			case metricdata.Histogram[float64]:
				if measurement.Name == "otelcol_capture_filter_duration_seconds" && len(data.DataPoints) == 1 {
					point := data.DataPoints[0]
					signal, _ := point.Attributes.Value(attribute.Key("signal"))
					capturePoint, _ := point.Attributes.Value(attribute.Key("capture_point"))
					seen[measurement.Name] = point.Count == 1 && signal.AsString() == "traces" && capturePoint.AsString() == "ingress"
				}
			}
		}
	}
	for _, name := range []string{"otelcol_capture_records_selected_total", "otelcol_capture_bytes_enqueued_total", "otelcol_capture_records_dropped_total", "otelcol_capture_filter_duration_seconds"} {
		if !seen[name] {
			t.Errorf("lost or changed cached metric labels for %s", name)
		}
	}
}
