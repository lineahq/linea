package livecaptureextension

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/metric"
)

// queueObservations reads the stock exporter's live queue callbacks. Keeping the
// stock registration as well preserves all standard exporter metrics. This
// adapter only supplies the unlabelled, pod-wide byte gauge required by capture;
// it does no work on the telemetry processing path.
type queueObservations struct {
	mu        sync.Mutex
	next      uint64
	callbacks map[uint64]func(context.Context) int64
}

func (q *queueObservations) sum(ctx context.Context) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var total int64
	for _, callback := range q.callbacks {
		total += callback(ctx)
	}
	return total
}

type queueMeterProvider struct {
	metric.MeterProvider
	queues *queueObservations
}

func (p queueMeterProvider) Meter(name string, options ...metric.MeterOption) metric.Meter {
	return &queueMeter{Meter: p.MeterProvider.Meter(name, options...), queues: p.queues}
}

type queueMeter struct {
	metric.Meter
	queues *queueObservations
	size   *queueGauge
}

type queueGauge struct{ metric.Int64ObservableGauge }

func (m *queueMeter) Int64ObservableGauge(name string, options ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	gauge, err := m.Meter.Int64ObservableGauge(name, options...)
	if name == "otelcol_exporter_queue_size" && err == nil {
		m.size = &queueGauge{Int64ObservableGauge: gauge}
		return m.size, nil
	}
	return gauge, err
}
func (m *queueMeter) RegisterCallback(callback metric.Callback, instruments ...metric.Observable) (metric.Registration, error) {
	underlying := append([]metric.Observable(nil), instruments...)
	for i, instrument := range underlying {
		if instrument == m.size {
			underlying[i] = m.size.Int64ObservableGauge
		}
	}
	registration, err := m.Meter.RegisterCallback(func(ctx context.Context, observer metric.Observer) error {
		return callback(ctx, &queueForwardObserver{Observer: observer})
	}, underlying...)
	if err != nil {
		return nil, err
	}
	if m.size == nil || len(instruments) != 1 || instruments[0] != m.size {
		return registration, nil
	}
	m.queues.mu.Lock()
	if m.queues.callbacks == nil {
		m.queues.callbacks = make(map[uint64]func(context.Context) int64)
	}
	m.queues.next++
	id := m.queues.next
	m.queues.callbacks[id] = func(ctx context.Context) int64 {
		observer := &queueObserver{instrument: m.size}
		_ = callback(ctx, observer)
		return observer.bytes
	}
	m.queues.mu.Unlock()
	return &queueRegistration{Registration: registration, queues: m.queues, id: id}, nil
}

// SDK observers must receive the SDK's original instrument. A unique wrapper
// also distinguishes queue size from other instruments in a noop provider.
type queueForwardObserver struct{ metric.Observer }

func (o *queueForwardObserver) ObserveInt64(instrument metric.Int64Observable, value int64, options ...metric.ObserveOption) {
	if gauge, ok := instrument.(*queueGauge); ok {
		instrument = gauge.Int64ObservableGauge
	}
	o.Observer.ObserveInt64(instrument, value, options...)
}

type queueObserver struct {
	metric.Observer
	instrument metric.Int64Observable
	bytes      int64
}

func (o *queueObserver) ObserveInt64(instrument metric.Int64Observable, value int64, _ ...metric.ObserveOption) {
	if instrument == o.instrument {
		o.bytes += value
	}
}

type queueRegistration struct {
	metric.Registration
	queues *queueObservations
	id     uint64
}

func (r *queueRegistration) Unregister() error {
	r.queues.mu.Lock()
	delete(r.queues.callbacks, r.id)
	r.queues.mu.Unlock()
	return r.Registration.Unregister()
}
