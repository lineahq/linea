package livecaptureextension

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type telemetry struct {
	active       metric.Int64UpDownCounter
	selected     metric.Int64Counter
	dropped      metric.Int64Counter
	bytes        metric.Int64Counter
	errors       metric.Int64Counter
	terminations metric.Int64Counter
	connected    metric.Int64Gauge
	duration     metric.Float64Histogram
	signalLabels map[Signal]metric.MeasurementOption
	dropLabels   map[Signal]*[dropReasonCount]metric.MeasurementOption
}

func newTelemetry(provider metric.MeterProvider, queues *queueObservations) (telemetry, error) {
	meter := provider.Meter("github.com/lineahq/linea/collector/livecapture/livecaptureextension")
	t := telemetry{signalLabels: make(map[Signal]metric.MeasurementOption), dropLabels: make(map[Signal]*[dropReasonCount]metric.MeasurementOption)}
	for _, signal := range []Signal{Traces, Logs, Metrics} {
		t.signalLabels[signal] = metric.WithAttributes(attribute.String("signal", string(signal)))
		t.dropLabels[signal] = &[dropReasonCount]metric.MeasurementOption{}
		for i, reason := range dropReasons {
			t.dropLabels[signal][i] = dropLabels(signal, reason)
		}
	}
	var errs error
	var err error
	t.active, err = meter.Int64UpDownCounter("otelcol_capture_sessions_active")
	errs = errors.Join(errs, err)
	t.selected, err = meter.Int64Counter("otelcol_capture_records_selected_total")
	errs = errors.Join(errs, err)
	t.dropped, err = meter.Int64Counter("otelcol_capture_records_dropped_total")
	errs = errors.Join(errs, err)
	t.bytes, err = meter.Int64Counter("otelcol_capture_bytes_enqueued_total")
	errs = errors.Join(errs, err)
	t.errors, err = meter.Int64Counter("otelcol_capture_filter_errors_total")
	errs = errors.Join(errs, err)
	t.terminations, err = meter.Int64Counter("otelcol_capture_session_terminations_total")
	errs = errors.Join(errs, err)
	t.connected, err = meter.Int64Gauge("otelcol_capture_control_connected")
	errs = errors.Join(errs, err)
	t.duration, err = meter.Float64Histogram("otelcol_capture_filter_duration_seconds", metric.WithUnit("s"))
	errs = errors.Join(errs, err)
	_, err = meter.Int64ObservableGauge("otelcol_capture_queue_bytes", metric.WithUnit("By"), metric.WithInt64Callback(func(ctx context.Context, observer metric.Int64Observer) error {
		observer.Observe(queues.sum(ctx))
		return nil
	}))
	errs = errors.Join(errs, err)
	return t, errs
}

func labels(signal Signal, point string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("signal", string(signal)), attribute.String("capture_point", point))
}
func dropLabels(signal Signal, reason string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("signal", string(signal)), attribute.String("reason", reason))
}
