package livecaptureextension

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"
)

type dropReason int

const (
	dropQueueFull dropReason = iota
	dropRateBytes
	dropRateRecords
	dropRecordSize
	dropSampling
	dropStopping
	dropDelivery
	dropBudget
	dropReasonCount
)

var dropReasons = [dropReasonCount]string{"queue_full", "rate_bytes", "rate_records", "record_size", "sampling", "stopping", "delivery", "budget"}

type captureKey struct {
	Point
	signal Signal
}
type termination struct{ state, reason string }

// run is one capture from start to its terminal state. Budgets, the rate
// window and the deadline belong to the run, so updating a session's
// configuration never grants a new allowance or extends its life.
type run struct {
	manager  *Manager
	id       string
	deadline atomic.Int64 // Unix nanoseconds; only ever moves earlier.
	expiry   *time.Timer
	// ended closes when admission is over; done when the run is terminal.
	stop, ended, done chan struct{}
	stopOnce          sync.Once
	stopped           atomic.Bool
	termination       atomic.Pointer[termination]
	// gate serializes admission. The window fields are written only under it
	// and read without it by the per-record pre-checks.
	gate                                    sync.Mutex
	windowStart, windowBytes, windowRecords atomic.Int64
	selected, enqueued, bytes, filterErrors atomic.Int64
	drops                                   [dropReasonCount]atomic.Int64
}

// session is one configuration of a run. An update swaps in a new session
// sharing the same run; capture calls already holding the old one finish
// against the same budgets.
type session struct {
	*run
	request     Request
	fingerprint [32]byte
	filter      *compiledFilter
	points      map[captureKey]sessionPoint
}

// sessionPoint is a selected capture point of a session. Raw points have no
// redactor: their copies leave the Collector unmodified.
type sessionPoint struct {
	options  metric.MeasurementOption
	redactor *redactor
}

func (r *run) stats() Stats {
	result := Stats{Selected: r.selected.Load(), EnqueuedRecords: r.enqueued.Load(), EnqueuedBytes: r.bytes.Load(), FilterErrors: r.filterErrors.Load(), Dropped: make(map[string]int64)}
	for i, reason := range dropReasons {
		if n := r.drops[i].Load(); n > 0 {
			result.Dropped[reason] = n
		}
	}
	return result
}
func (r *run) status() Status {
	state, reason := "active", ""
	if end := r.termination.Load(); end != nil {
		state, reason = "stopping", end.reason
	}
	return Status{SessionID: r.id, State: state, Reason: reason, ExpiresAt: time.Unix(0, r.deadline.Load()), Stats: r.stats()}
}
func (r *run) requestStop(state, reason string) {
	r.stopOnce.Do(func() {
		r.termination.Store(&termination{state: state, reason: reason})
		r.stopped.Store(true)
		r.manager.deactivate(r)
		close(r.stop)
	})
}

// deactivate clears the capture fast path if it still belongs to r.
func (m *Manager) deactivate(r *run) {
	for {
		s := m.active.Load()
		if s == nil || s.run != r || m.active.CompareAndSwap(s, nil) {
			return
		}
	}
}

// Manager is the process-local extension. Processors resolve it from the host;
// no global singleton or package-level map owns session state.
type Manager struct {
	config       Config
	settings     extension.Settings
	host         component.Host
	telemetry    telemetry
	redactor     *redactor
	active       atomic.Pointer[session]
	enabled      atomic.Bool
	mu           sync.Mutex
	current      *session
	points       map[captureKey]CapturePoint
	history      []Status // Bounded to the last 64 ended sessions, terminal or draining.
	shuttingDown bool
	wg           sync.WaitGroup
	cancel       context.CancelFunc
	local        *localServer
	// delivery lives as long as the extension. Queued copies of ended
	// sessions share one byte budget; a session reports its end only after
	// they left the queue (see drained).
	delivery delivery
	queues   *queueObservations
	quit     chan struct{}
}

// drainLimit bounds how long an ended session waits for its queued copies:
// one flush, the export timeout and the retry budget (otlpDelivery.config).
const drainLimit = 7 * time.Second

func NewFactory() extension.Factory {
	return extension.NewFactory(component.MustNewType("livecapture"), defaultConfig, func(_ context.Context, settings extension.Settings, config component.Config) (extension.Extension, error) {
		return newManager(settings, *config.(*Config))
	}, component.StabilityLevelAlpha)
}

func newManager(settings extension.Settings, config Config) (*Manager, error) {
	if settings.Logger == nil {
		settings.Logger = zap.NewNop()
	}
	if settings.MeterProvider == nil {
		settings.MeterProvider = noop.NewMeterProvider()
	}
	if settings.TracerProvider == nil {
		settings.TracerProvider = tracenoop.NewTracerProvider()
	}
	redactor, err := newRedactor(config.Redaction)
	if err != nil {
		return nil, err
	}
	queues := &queueObservations{}
	telemetry, err := newTelemetry(settings.MeterProvider, queues)
	if err != nil {
		return nil, err
	}
	m := &Manager{config: config, settings: settings, telemetry: telemetry, redactor: redactor, points: make(map[captureKey]CapturePoint), queues: queues, quit: make(chan struct{})}
	m.settings.MeterProvider = queueMeterProvider{MeterProvider: settings.MeterProvider, queues: queues}
	m.enabled.Store(config.Enabled)
	m.delivery = newOTLPDelivery(m)
	return m, nil
}

func (m *Manager) Start(ctx context.Context, host component.Host) error {
	m.host = host
	workerCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	if m.config.LocalControl.Enabled {
		server, err := m.startLocal()
		if err != nil {
			cancel()
			return err
		}
		m.local = server
	}
	if m.config.Control.Endpoint != "" {
		m.wg.Add(1)
		go func() { defer m.wg.Done(); m.watch(workerCtx) }()
	}
	return nil
}

func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.shuttingDown {
		close(m.quit)
	}
	m.shuttingDown = true
	if m.current != nil {
		m.current.requestStop("completed", "shutdown")
	}
	m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
	var err error
	if m.local != nil {
		err = m.local.server.Shutdown(ctx)
	}
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return errors.Join(err, ctx.Err())
	}
	// No session admits anymore; queued copies get the rest of the deadline.
	return errors.Join(err, m.delivery.shutdown(ctx))
}

func (m *Manager) Register(point CapturePoint) error {
	if point.Pipeline == "" || len(point.Pipeline) > 256 || point.Point.Point == "" || len(point.Point.Point) > 128 || (point.Sensitivity != "raw" && point.Sensitivity != "redacted") {
		return errors.New("capture point requires pipeline, point, and raw/redacted sensitivity")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := captureKey{Point: point.Point, signal: point.Signal}
	if _, exists := m.points[key]; exists {
		return fmt.Errorf("duplicate capture point %s/%s for %s; use a distinct processor instance with explicit pipeline", point.Pipeline, point.Point.Point, point.Signal)
	}
	m.points[key] = point
	return nil
}

func (m *Manager) Unregister(point CapturePoint) {
	m.mu.Lock()
	delete(m.points, captureKey{Point: point.Point, signal: point.Signal})
	m.mu.Unlock()
}

func clamp(requested, maximum int64) int64 {
	if requested == 0 || requested > maximum {
		return maximum
	}
	return requested
}

func (m *Manager) prepare(request Request, access Access) (Request, [32]byte, *compiledFilter, map[captureKey]sessionPoint, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return Request{}, [32]byte{}, nil, nil, err
	}
	fingerprint := sha256.Sum256(encoded)
	// Own every slice/map: external callers may reuse their desired-state object.
	var owned Request
	if err = json.Unmarshal(encoded, &owned); err != nil {
		return Request{}, fingerprint, nil, nil, err
	}
	request = owned
	fail := func(err error) (Request, [32]byte, *compiledFilter, map[captureKey]sessionPoint, error) {
		return Request{}, fingerprint, nil, nil, err
	}
	if request.SessionID == "" || len(request.SessionID) > 128 || len(request.Principal) > 256 || request.Principal == "" {
		return fail(errors.New("session_id and principal are required and bounded"))
	}
	if request.Target.CollectorGroup == "" {
		request.Target.CollectorGroup = m.config.CollectorGroup
	}
	if request.Target.CollectorGroup != m.config.CollectorGroup || (request.Target.Pod != "" && request.Target.Pod != m.config.Pod) {
		return fail(errors.New("session target does not match this Collector"))
	}
	if len(request.Signals) == 0 || len(request.Signals) > 3 || len(request.CapturePoints) == 0 || len(request.CapturePoints) > 64 {
		return fail(errors.New("one to three signals and one to 64 capture points are required"))
	}
	signals := make(map[Signal]bool)
	for _, signal := range request.Signals {
		if (signal != Traces && signal != Metrics && signal != Logs) || signals[signal] {
			return fail(errors.New("signals must be unique traces, metrics, or logs"))
		}
		signals[signal] = true
	}
	if request.Sampling != nil && (math.IsNaN(request.Sampling.Rate) || request.Sampling.Rate < 0 || request.Sampling.Rate > 1 || (request.Sampling.Mode != "" && request.Sampling.Mode != "hash" && request.Sampling.Mode != "random")) {
		return fail(errors.New("sampling requires rate in [0,1] and mode hash or random"))
	}
	for _, n := range []int64{request.Limits.BytesPerSecond, request.Limits.RecordsPerSecond, request.Limits.TotalBytes, request.Limits.TotalRecords, request.Limits.RecordBytes} {
		if n < 0 {
			return fail(errors.New("session limits cannot be negative"))
		}
	}
	l := m.config.Limits
	request.Limits = SessionLimits{BytesPerSecond: clamp(request.Limits.BytesPerSecond, int64(l.MaxBytesPerSecond)), RecordsPerSecond: clamp(request.Limits.RecordsPerSecond, l.MaxRecordsPerSecond), TotalBytes: clamp(request.Limits.TotalBytes, int64(l.MaxTotalBytes)), TotalRecords: clamp(request.Limits.TotalRecords, l.MaxTotalRecords), RecordBytes: clamp(request.Limits.RecordBytes, int64(l.MaxRecordBytes))}
	now := time.Now()
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = now.Add(l.DefaultSessionDuration)
	}
	if !request.ExpiresAt.After(now) {
		return fail(errors.New("session is already expired"))
	}
	if maximum := now.Add(l.MaxSessionDuration); request.ExpiresAt.After(maximum) {
		request.ExpiresAt = maximum
	}
	filter, err := compileFilter(request.Filter, m.settings.TelemetrySettings)
	if err != nil {
		return fail(err)
	}
	points := make(map[captureKey]sessionPoint)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, point := range request.CapturePoints {
		if len(point.Pipeline) > 256 || len(point.Point) > 128 {
			return fail(errors.New("capture point identity exceeds length limit"))
		}
		found := false
		for signal := range signals {
			key := captureKey{Point: point, signal: signal}
			known, exists := m.points[key]
			if !exists {
				continue
			}
			found = true
			if known.Sensitivity == "raw" && !access.AllowRaw {
				return fail(errors.New("raw capture point requires elevated permission"))
			}
			// Attribute sets are immutable. Build once per session/point instead
			// of sorting and allocating them for every capture call.
			redactor := m.redactor
			if known.Sensitivity == "raw" {
				redactor = nil
			}
			points[key] = sessionPoint{labels(signal, point.Point), redactor}
		}
		if !found {
			return fail(fmt.Errorf("unknown capture point or signal: %s/%s", point.Pipeline, point.Point))
		}
	}
	for signal := range signals {
		if !slices.ContainsFunc(request.CapturePoints, func(point Point) bool { _, ok := points[captureKey{Point: point, signal: signal}]; return ok }) {
			return fail(fmt.Errorf("no capture point supports signal %s", signal))
		}
	}
	return request, fingerprint, filter, points, nil
}

// StartSession starts a session, or atomically replaces the configuration of
// the running session with the same ID. Capture never pauses for an update.
func (m *Manager) StartSession(ctx context.Context, request Request, access Access) (Status, error) {
	prepared, fingerprint, filter, points, err := m.prepare(request, access)
	if err != nil {
		return Status{}, err
	}
	if !m.enabled.Load() {
		return Status{}, errors.New("capture is disabled")
	}
	// Starting exporters can take a while; never hold m.mu meanwhile, so the
	// kill switch and status reads stay immediate.
	if err := m.delivery.ensure(prepared.Signals); err != nil {
		return Status{}, fmt.Errorf("start internal exporter: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown || !m.enabled.Load() {
		return Status{}, errors.New("capture is disabled")
	}
	if old := m.current; old != nil {
		if old.request.SessionID != prepared.SessionID {
			return Status{}, fmt.Errorf("the Collector already captures session %s until %s", old.request.SessionID, time.Unix(0, old.deadline.Load()).Format(time.RFC3339))
		}
		if old.stopped.Load() {
			return Status{}, errors.New("session ID already terminated; create a new session")
		}
		if old.fingerprint == fingerprint {
			return old.status(), nil
		}
		r := old.run
		if deadline := time.Unix(0, r.deadline.Load()); !prepared.ExpiresAt.Before(deadline) {
			prepared.ExpiresAt = deadline
		} else {
			r.deadline.Store(prepared.ExpiresAt.UnixNano())
			r.expiry.Reset(time.Until(prepared.ExpiresAt))
		}
		s := &session{run: r, request: prepared, fingerprint: fingerprint, filter: filter, points: points}
		m.current = s
		m.active.Store(s)
		m.settings.Logger.Info("capture session updated", zap.String("session_id", prepared.SessionID), zap.String("principal", prepared.Principal), zap.Any("capture_points", prepared.CapturePoints), zap.Any("filter", prepared.Filter), zap.Any("sampling", prepared.Sampling), zap.Any("limits", prepared.Limits), zap.Time("expires_at", prepared.ExpiresAt))
		return s.status(), nil
	}
	// A repeated desired set cannot resurrect a terminal session.
	if slices.ContainsFunc(m.history, func(status Status) bool { return status.SessionID == prepared.SessionID }) {
		return Status{}, errors.New("session ID already terminated; create a new session")
	}
	if !prepared.ExpiresAt.After(time.Now()) {
		return Status{}, errors.New("session expired while starting")
	}
	r := &run{manager: m, id: prepared.SessionID, stop: make(chan struct{}), ended: make(chan struct{}), done: make(chan struct{})}
	r.deadline.Store(prepared.ExpiresAt.UnixNano())
	s := &session{run: r, request: prepared, fingerprint: fingerprint, filter: filter, points: points}
	m.current = s
	m.active.Store(s)
	r.expiry = time.AfterFunc(time.Until(prepared.ExpiresAt), func() { r.requestStop("expired", "ttl") })
	m.wg.Add(1)
	go m.runSession(r)
	m.telemetry.active.Add(context.Background(), 1)
	m.settings.Logger.Info("capture session started", zap.String("session_id", prepared.SessionID), zap.String("principal", prepared.Principal), zap.Any("target", prepared.Target), zap.Any("capture_points", prepared.CapturePoints), zap.Any("filter", prepared.Filter), zap.Any("sampling", prepared.Sampling), zap.Any("limits", prepared.Limits), zap.Time("expires_at", prepared.ExpiresAt))
	return s.status(), nil
}

func (m *Manager) runSession(r *run) {
	defer m.wg.Done()
	<-r.stop
	r.expiry.Stop()
	r.gate.Lock()
	r.gate.Unlock() // Wait for any admission already past its stopped check.
	m.telemetry.active.Add(context.Background(), -1)
	end := r.termination.Load()
	m.mu.Lock()
	principal := ""
	if m.current != nil && m.current.run == r {
		principal = m.current.request.Principal
		m.current = nil
	}
	// An update racing with requestStop may have republished this run.
	m.deactivate(r)
	// Stopping while its queued copies drain; the history entry also keeps
	// the session ID from being reused. A new session may start meanwhile.
	m.recordLocked(r.status())
	close(r.ended)
	m.mu.Unlock()
	m.drained()
	status := r.status()
	status.State = end.state
	m.mu.Lock()
	m.recordLocked(status)
	close(r.done)
	m.mu.Unlock()
	m.telemetry.terminations.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", end.reason)))
	m.settings.Logger.Info("capture session ended", zap.String("session_id", r.id), zap.String("principal", principal), zap.String("state", end.state), zap.String("reason", end.reason), zap.Any("stats", status.Stats))
}

// recordLocked keeps a session's latest status in the bounded history.
func (m *Manager) recordLocked(status Status) {
	for i := range m.history {
		if m.history[i].SessionID == status.SessionID {
			m.history[i] = status
			return
		}
	}
	m.history = append(m.history, status)
	if len(m.history) > 64 {
		m.history = m.history[len(m.history)-64:]
	}
}

// drained waits until the delivery queue holds no copies. A queued copy
// counts until its export finished or failed, so the server sees the ended
// session stopping, keeps its lease and accepts the copies admitted before
// the stop. A session started meanwhile can only extend the wait, which
// drainLimit bounds. Shutdown drains on its own.
func (m *Manager) drained() {
	limit := time.NewTimer(drainLimit)
	defer limit.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for m.queues.sum(context.Background()) > 0 {
		select {
		case <-tick.C:
		case <-limit.C:
			return
		case <-m.quit:
			return
		}
	}
}

func (m *Manager) StopSession(ctx context.Context, id string) error {
	m.mu.Lock()
	s := m.current
	if s == nil || s.request.SessionID != id {
		m.mu.Unlock()
		return nil
	}
	s.requestStop("completed", "user")
	m.mu.Unlock()
	select {
	case <-s.ended:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) SetEnabled(enabled bool) {
	enabled = enabled && m.config.Enabled
	m.mu.Lock()
	m.enabled.Store(enabled)
	if !enabled && m.current != nil {
		m.current.requestStop("disabled", "kill_switch")
	}
	m.mu.Unlock()
}

func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Status, 0, len(m.history)+1)
	positions := make(map[string]int)
	for _, status := range m.history {
		status.Stats.Dropped = maps.Clone(status.Stats.Dropped)
		if index, exists := positions[status.SessionID]; exists {
			result[index] = status
		} else {
			positions[status.SessionID] = len(result)
			result = append(result, status)
		}
	}
	if m.current != nil {
		status := m.current.status()
		if index, exists := positions[status.SessionID]; exists {
			result[index] = status
		} else {
			result = append(result, status)
		}
	}
	return result
}

func (m *Manager) CapturePoints() []CapturePoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]CapturePoint, 0, len(m.points))
	for _, point := range m.points {
		result = append(result, point)
	}
	slices.SortFunc(result, func(a, b CapturePoint) int {
		return strings.Compare(a.Pipeline+"/"+a.Point.Point+"/"+string(a.Signal), b.Pipeline+"/"+b.Point.Point+"/"+string(b.Signal))
	})
	return result
}
