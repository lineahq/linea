package livecaptureextension

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configoptional"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// delivery outlives sessions. ensure prepares the given signals before a
// session that uses them is published; the send methods never block.
type delivery interface {
	ensure(signals []Signal) error
	traces(context.Context, ptrace.Traces) error
	metrics(context.Context, pmetric.Metrics) error
	logs(context.Context, plog.Logs) error
	shutdown(context.Context) error
}

var exporterQueueFull = exporterhelper.ErrQueueIsFull

// otlpDelivery owns one stock OTLP exporter per signal, started on first use
// and kept until the extension shuts down. Each byte queue holds a third of
// max_queue_bytes, so all queued copies of all sessions stay within it.
type otlpDelivery struct {
	manager *Manager
	mu      sync.Mutex
	closed  bool
	// Written once under mu before any session using the signal is
	// published, then only read.
	trace  exporter.Traces
	metric exporter.Metrics
	log    exporter.Logs
}

func newOTLPDelivery(m *Manager) *otlpDelivery { return &otlpDelivery{manager: m} }

func (d *otlpDelivery) traces(ctx context.Context, data ptrace.Traces) error {
	return d.trace.ConsumeTraces(ctx, data)
}
func (d *otlpDelivery) metrics(ctx context.Context, data pmetric.Metrics) error {
	return d.metric.ConsumeMetrics(ctx, data)
}
func (d *otlpDelivery) logs(ctx context.Context, data plog.Logs) error {
	return d.log.ConsumeLogs(ctx, data)
}

func (d *otlpDelivery) ensure(signals []Signal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errors.New("delivery is shut down")
	}
	m := d.manager
	factory := otlpexporter.NewFactory()
	settings := exporter.Settings{ID: component.NewIDWithName(component.MustNewType("otlp"), "livecapture"), TelemetrySettings: m.settings.TelemetrySettings, BuildInfo: m.settings.BuildInfo}
	config := d.config()
	// Exporters live as long as the extension, not the starting request.
	ctx := context.Background()
	for _, signal := range signals {
		var err error
		switch {
		case signal == Traces && d.trace == nil:
			e, createErr := factory.CreateTraces(ctx, settings, &config)
			if err = d.start(e, createErr); err == nil {
				d.trace = e
			}
		case signal == Metrics && d.metric == nil:
			e, createErr := factory.CreateMetrics(ctx, settings, &config)
			if err = d.start(e, createErr); err == nil {
				d.metric = e
			}
		case signal == Logs && d.log == nil:
			e, createErr := factory.CreateLogs(ctx, settings, &config)
			if err = d.start(e, createErr); err == nil {
				d.log = e
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *otlpDelivery) start(c component.Component, err error) error {
	if err != nil {
		return err
	}
	if err = c.Start(context.Background(), d.manager.host); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = c.Shutdown(cleanup)
		cancel()
	}
	return err
}

func (d *otlpDelivery) config() otlpexporter.Config {
	m := d.manager
	config := m.config.Delivery
	queue := exporterhelper.NewDefaultQueueConfig()
	_ = queue.Sizer.UnmarshalText([]byte("bytes"))
	queue.QueueSize = int64(m.config.Limits.MaxQueueBytes) / 3
	queue.NumConsumers = 1
	queue.WaitForResult = false
	queue.BlockOnOverflow = false
	queue.StorageID = nil
	batch := exporterhelper.NewDefaultBatchConfig()
	_ = batch.Sizer.UnmarshalText([]byte("bytes"))
	batch.FlushTimeout = 100 * time.Millisecond
	batch.MaxSize = min(int64(256<<10), queue.QueueSize)
	batch.MinSize = batch.MaxSize
	queue.Batch = configoptional.Some(batch)
	config.QueueConfig = configoptional.Some(queue)
	config.TimeoutConfig.Timeout = time.Second
	config.RetryConfig.Enabled = true
	config.RetryConfig.InitialInterval = 100 * time.Millisecond
	config.RetryConfig.MaxInterval = time.Second
	config.RetryConfig.MaxElapsedTime = 5 * time.Second
	return config
}

func (d *otlpDelivery) shutdown(ctx context.Context) error {
	d.mu.Lock()
	d.closed = true
	var components []component.Component
	for _, c := range []component.Component{d.trace, d.metric, d.log} {
		if c != nil {
			components = append(components, c)
		}
	}
	d.mu.Unlock()
	// The stock queue ignores the context while draining, so wait for each
	// signal concurrently and give up at the deadline.
	results := make(chan error, len(components))
	for _, c := range components {
		go func() { results <- c.Shutdown(ctx) }()
	}
	var err error
	for range components {
		select {
		case result := <-results:
			err = errors.Join(err, result)
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}
