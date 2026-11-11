package platform

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// The server's own metrics: its run queue, leases, workers, builds, storage
// and audit log, recorded with the OpenTelemetry metric API on
// Options.MeterProvider. The composing application chooses the SDK and the
// exporter; without a provider they go nowhere. Attributes have a fixed,
// small set of values: no run, runtime, worker or principal is named.

// meterName is the instrumentation scope of the server's metrics.
const meterName = "github.com/lineahq/linea/internal/debugger/platform"

// claimWaitBounds are linea.run.claim.wait's bucket bounds in seconds: a
// run waits from milliseconds on an idle server to many minutes behind a
// full queue or a runtime build.
var claimWaitBounds = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600}

var (
	renewed      = metric.WithAttributeSet(attribute.NewSet(attribute.String("outcome", "renewed")))
	renewFailed  = metric.WithAttributeSet(attribute.NewSet(attribute.String("outcome", "failed")))
	expRequeued  = metric.WithAttributeSet(attribute.NewSet(attribute.String("result", "requeued")))
	expLost      = metric.WithAttributeSet(attribute.NewSet(attribute.String("result", "lost")))
	buildQueued  = metric.WithAttributeSet(attribute.NewSet(attribute.String("state", "queued")))
	buildRunning = metric.WithAttributeSet(attribute.NewSet(attribute.String("state", "running")))
)

// serverMetrics are the instruments the scheduler and the audit log record
// on, and the state the observed instruments read.
type serverMetrics struct {
	claimWait     metric.Float64Histogram
	leaseRenewals metric.Int64Counter
	leasesLost    metric.Int64Counter
	expiries      metric.Int64Counter
	auditFailures metric.Int64Counter
	// observed unregisters the observed instruments' callback.
	observed metric.Registration
	// storageOK is when the store last answered the scheduler's lease scan
	// (tendLeases), as Unix nanoseconds.
	storageOK atomic.Int64
}

// newServerMetrics creates the instruments on provider, a no-op one when nil.
// An instrument the SDK refuses is replaced by a no-op one: metrics never
// stop the server.
func (s *Service) newServerMetrics(provider metric.MeterProvider) {
	if provider == nil {
		provider = noop.NewMeterProvider()
	}
	meter := provider.Meter(meterName)
	fallback := noop.Meter{}
	m := &s.metrics
	var err error
	if m.claimWait, err = meter.Float64Histogram("linea.run.claim.wait", metric.WithUnit("s"),
		metric.WithDescription("Time from a run's admission to its first claim, for the runs this instance claimed."),
		metric.WithExplicitBucketBoundaries(claimWaitBounds...)); err != nil {
		m.claimWait, _ = fallback.Float64Histogram("")
	}
	if m.leaseRenewals, err = meter.Int64Counter("linea.run.lease.renewals", metric.WithUnit("{renewal}"),
		metric.WithDescription("Run lease renewals of this instance, one per executor each third of a lease, by outcome.")); err != nil {
		m.leaseRenewals, _ = fallback.Int64Counter("")
	}
	if m.leasesLost, err = meter.Int64Counter("linea.run.lease.lost", metric.WithUnit("{run}"),
		metric.WithDescription("Runs this instance stopped because their lease ended before it was renewed.")); err != nil {
		m.leasesLost, _ = fallback.Int64Counter("")
	}
	if m.expiries, err = meter.Int64Counter("linea.run.lease.expiries", metric.WithUnit("{run}"),
		metric.WithDescription("Runs whose lease ended that this instance queued again or ended as lost after their last attempt, by result.")); err != nil {
		m.expiries, _ = fallback.Int64Counter("")
	}
	if m.auditFailures, err = meter.Int64Counter("linea.audit.append.failures", metric.WithUnit("{event}"),
		metric.WithDescription("Audit events this instance could not record.")); err != nil {
		m.auditFailures, _ = fallback.Int64Counter("")
	}
	queued, err1 := meter.Int64ObservableUpDownCounter("linea.run.queue.size", metric.WithUnit("{run}"),
		metric.WithDescription("Queued runs this instance knows of; with a shared store, the installation's queue."))
	workers, err2 := meter.Int64ObservableUpDownCounter("linea.worker.connections", metric.WithUnit("{worker}"),
		metric.WithDescription("Workers connected to this instance."))
	workerSlots, err3 := meter.Int64ObservableUpDownCounter("linea.worker.slots", metric.WithUnit("{run}"),
		metric.WithDescription("Runs the workers connected to this instance take at once."))
	builders, err4 := meter.Int64ObservableUpDownCounter("linea.builder.connections", metric.WithUnit("{builder}"),
		metric.WithDescription("Builders connected to this instance."))
	builderSlots, err5 := meter.Int64ObservableUpDownCounter("linea.builder.slots", metric.WithUnit("{build}"),
		metric.WithDescription("Builds the builders connected to this instance run at once."))
	builds, err6 := meter.Int64ObservableUpDownCounter("linea.runtime.builds", metric.WithUnit("{build}"),
		metric.WithDescription("Runtime builds of the installation that are queued or running, by state."))
	storage, err7 := meter.Int64ObservableGauge("linea.storage.ready", metric.WithUnit("1"),
		metric.WithDescription("1 while the store answers this instance's scheduler, 0 once it has not for a whole run lease (/readyz then answers 503)."))
	if errors.Join(err1, err2, err3, err4, err5, err6, err7) != nil {
		return
	}
	m.observed, _ = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		v := s.observe(ctx)
		o.ObserveInt64(queued, int64(v.queued))
		o.ObserveInt64(workers, int64(v.workers))
		o.ObserveInt64(workerSlots, int64(v.workerSlots))
		o.ObserveInt64(builders, int64(v.build.Builders))
		o.ObserveInt64(builderSlots, int64(v.build.BuilderSlots))
		if v.buildsRead {
			o.ObserveInt64(builds, int64(v.build.Queued), buildQueued)
			o.ObserveInt64(builds, int64(v.build.Running), buildRunning)
		}
		ready := int64(0)
		if s.StorageReady() {
			ready = 1
		}
		o.ObserveInt64(storage, ready)
		return nil
	}, queued, workers, workerSlots, builders, builderSlots, builds, storage)
}

// observation is what the observed instruments report at one collection.
type observation struct {
	queued, workers, workerSlots int
	build                        buildMetrics
	// buildsRead is false when the build queue could not be read: its
	// counts are left out rather than reported as zero.
	buildsRead bool
}

func (s *Service) observe(ctx context.Context) observation {
	var v observation
	var err error
	v.build, err = s.readBuildMetrics(ctx)
	v.buildsRead = err == nil
	if err != nil {
		s.builders.mu.Lock()
		v.build = buildMetrics{Builders: s.builders.links, BuilderSlots: s.builders.slots}
		s.builders.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, job := range s.jobs {
		r := job.handle
		r.mu.Lock()
		if r.state == StateQueued {
			v.queued++
		}
		r.mu.Unlock()
	}
	for _, e := range s.executors {
		if e.conn != nil && !e.gone {
			v.workers++
			v.workerSlots += e.capacity
		}
	}
	return v
}

// storageAnswered records that the store answered at now.
func (m *serverMetrics) storageAnswered(now time.Time) { m.storageOK.Store(now.UnixNano()) }

// StorageReady reports whether the store answered the scheduler within the
// last lease. The scheduler asks it every third of a lease, so a false here
// is a sustained outage, not one slow query.
func (s *Service) StorageReady() bool {
	return s.now().Sub(time.Unix(0, s.metrics.storageOK.Load())) <= s.lease
}
