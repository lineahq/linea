package livecaptureextension

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
)

// capabilities lists the protocol features this Collector implements. A
// desired state requiring any other capability is rejected as a whole.
var capabilities = []string{}

// AuthorizedRequest is trusted state from the authenticated control service.
// Authorization is deliberately outside Request, so a session cannot grant itself raw access.
type AuthorizedRequest struct {
	Request       Request `json:"request"`
	AuthorizedRaw bool    `json:"authorized_raw"`
}

// Reconcile applies a complete desired set. Disconnecting never calls this with
// an empty set and never changes an active session's local deadline.
func (m *Manager) Reconcile(ctx context.Context, desired []AuthorizedRequest, disabled bool) error {
	if disabled {
		m.SetEnabled(false)
		return nil
	}
	if len(desired) > 1 {
		return errors.New("at most one session per Collector is supported")
	}
	m.SetEnabled(true)
	m.mu.Lock()
	current := m.current
	m.mu.Unlock()
	if current != nil && (len(desired) == 0 || desired[0].Request.SessionID != current.request.SessionID) {
		if err := m.StopSession(ctx, current.request.SessionID); err != nil {
			return err
		}
	}
	if len(desired) == 0 {
		return nil
	}
	var err error
	if !m.config.Enabled {
		err = errors.New("capture is disabled by the Collector configuration")
	} else if desired[0].Request.ExpiresAt.IsZero() {
		err = errors.New("remote sessions require an absolute expires_at")
	} else {
		_, err = m.StartSession(ctx, desired[0].Request, Access{AllowRaw: desired[0].AuthorizedRaw})
	}
	if err != nil {
		m.reject(desired[0].Request.SessionID, err)
	}
	return err
}

// reject fails a desired session closed and keeps a bounded terminal status
// for platform diagnostics; no payloads.
func (m *Manager) reject(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.request.SessionID == id {
		m.current.requestStop("failed", "desired_state_rejected")
	}
	if id == "" || len(id) > 128 || slices.ContainsFunc(m.history, func(status Status) bool { return status.SessionID == id }) {
		return
	}
	m.history = append(m.history, Status{SessionID: id, State: "failed", Reason: truncate(err.Error(), 512), Stats: Stats{Dropped: map[string]int64{}}})
	if len(m.history) > 64 {
		m.history = m.history[len(m.history)-64:]
	}
}

// applyDesired applies one desired state from the control service. The
// stream stays up whatever it returns; the error is reported back.
func (m *Manager) applyDesired(ctx context.Context, desired *controlpb.DesiredState) error {
	// The kill switch never depends on the rest of the message being valid.
	if desired.GetDisabled() {
		return m.Reconcile(ctx, nil, true)
	}
	for _, required := range desired.GetRequiredCapabilities() {
		if !slices.Contains(capabilities, required) {
			return fmt.Errorf("unsupported required capability %q", truncate(required, 64))
		}
	}
	requests := make([]AuthorizedRequest, 0, len(desired.GetSessions()))
	for _, session := range desired.GetSessions() {
		request, err := requestFromProto(session)
		if err != nil {
			// Fail closed: a desired state that cannot be read stops capture.
			err = errors.Join(err, m.Reconcile(ctx, nil, false))
			m.reject(session.GetSessionId(), err)
			return err
		}
		requests = append(requests, request)
	}
	return m.Reconcile(ctx, requests, false)
}

var signalsFromProto = map[controlpb.Signal]Signal{controlpb.Signal_SIGNAL_TRACES: Traces, controlpb.Signal_SIGNAL_METRICS: Metrics, controlpb.Signal_SIGNAL_LOGS: Logs}
var signalsToProto = map[Signal]controlpb.Signal{Traces: controlpb.Signal_SIGNAL_TRACES, Metrics: controlpb.Signal_SIGNAL_METRICS, Logs: controlpb.Signal_SIGNAL_LOGS}
var statesToProto = map[string]controlpb.SessionState{"active": controlpb.SessionState_SESSION_STATE_ACTIVE, "stopping": controlpb.SessionState_SESSION_STATE_STOPPING, "completed": controlpb.SessionState_SESSION_STATE_COMPLETED, "expired": controlpb.SessionState_SESSION_STATE_EXPIRED, "limit_reached": controlpb.SessionState_SESSION_STATE_LIMIT_REACHED, "failed": controlpb.SessionState_SESSION_STATE_FAILED, "disabled": controlpb.SessionState_SESSION_STATE_DISABLED}

func requestFromProto(session *controlpb.Session) (AuthorizedRequest, error) {
	request := Request{SessionID: session.GetSessionId(), Principal: session.GetPrincipal()}
	request.Target.CollectorGroup = session.GetTarget().GetCollectorGroup()
	request.Target.Pod = session.GetTarget().GetPod()
	for _, point := range session.GetCapturePoints() {
		request.CapturePoints = append(request.CapturePoints, Point{Pipeline: point.GetPipeline(), Point: point.GetPoint()})
	}
	for _, signal := range session.GetSignals() {
		converted, ok := signalsFromProto[signal]
		if !ok {
			return AuthorizedRequest{}, fmt.Errorf("unsupported signal %d", signal)
		}
		request.Signals = append(request.Signals, converted)
	}
	filter := session.GetFilter()
	request.Filter.Resource = filter.GetResource()
	request.Filter.Traces.Span = filter.GetSpan()
	request.Filter.Logs.Log = filter.GetLog()
	request.Filter.Metrics.Metric = filter.GetMetric()
	request.Filter.Metrics.Datapoint = filter.GetDatapoint()
	if sampling := session.GetSampling(); sampling != nil {
		request.Sampling = &Sampling{Rate: sampling.GetRate()}
		switch sampling.GetMode() {
		case controlpb.SamplingMode_SAMPLING_MODE_UNSPECIFIED, controlpb.SamplingMode_SAMPLING_MODE_HASH:
			request.Sampling.Mode = "hash"
		case controlpb.SamplingMode_SAMPLING_MODE_RANDOM:
			request.Sampling.Mode = "random"
		default:
			return AuthorizedRequest{}, fmt.Errorf("unsupported sampling mode %d", sampling.GetMode())
		}
	}
	limits := session.GetLimits()
	request.Limits = SessionLimits{BytesPerSecond: limits.GetBytesPerSecond(), RecordsPerSecond: limits.GetRecordsPerSecond(), TotalBytes: limits.GetTotalBytes(), TotalRecords: limits.GetTotalRecords(), RecordBytes: limits.GetRecordBytes()}
	if expires := session.GetExpiresAt(); expires != nil {
		if err := expires.CheckValid(); err != nil {
			return AuthorizedRequest{}, fmt.Errorf("invalid expires_at: %w", err)
		}
		request.ExpiresAt = expires.AsTime()
	}
	return AuthorizedRequest{Request: request, AuthorizedRaw: session.GetAuthorizedRaw()}, nil
}

// Proto converts r to its control protocol form, for control services and
// test fixtures written against the domain types.
func (r AuthorizedRequest) Proto() *controlpb.Session {
	request := r.Request
	session := &controlpb.Session{SessionId: request.SessionID, Principal: request.Principal, Target: &controlpb.Target{CollectorGroup: request.Target.CollectorGroup, Pod: request.Target.Pod}, ExpiresAt: optionalTimestamp(request.ExpiresAt), AuthorizedRaw: r.AuthorizedRaw}
	for _, point := range request.CapturePoints {
		session.CapturePoints = append(session.CapturePoints, &controlpb.Point{Pipeline: point.Pipeline, Point: point.Point})
	}
	for _, signal := range request.Signals {
		session.Signals = append(session.Signals, signalsToProto[signal])
	}
	session.Filter = &controlpb.Filter{Resource: request.Filter.Resource, Span: request.Filter.Traces.Span, Log: request.Filter.Logs.Log, Metric: request.Filter.Metrics.Metric, Datapoint: request.Filter.Metrics.Datapoint}
	if request.Sampling != nil {
		session.Sampling = &controlpb.Sampling{Rate: request.Sampling.Rate}
		switch request.Sampling.Mode {
		case "hash":
			session.Sampling.Mode = controlpb.SamplingMode_SAMPLING_MODE_HASH
		case "random":
			session.Sampling.Mode = controlpb.SamplingMode_SAMPLING_MODE_RANDOM
		}
	}
	l := request.Limits
	session.Limits = &controlpb.Limits{BytesPerSecond: l.BytesPerSecond, RecordsPerSecond: l.RecordsPerSecond, TotalBytes: l.TotalBytes, TotalRecords: l.TotalRecords, RecordBytes: l.RecordBytes}
	return session
}

func statusToProto(status Status) *controlpb.SessionStatus {
	return &controlpb.SessionStatus{SessionId: status.SessionID, State: statesToProto[status.State], Reason: status.Reason, ExpiresAt: optionalTimestamp(status.ExpiresAt), Stats: &controlpb.SessionStats{Selected: status.Stats.Selected, EnqueuedRecords: status.Stats.EnqueuedRecords, EnqueuedBytes: status.Stats.EnqueuedBytes, FilterErrors: status.Stats.FilterErrors, Dropped: status.Stats.Dropped}}
}

func optionalTimestamp(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// collectorVersion is the Collector core version this binary was built with.
var collectorVersion = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	version := ""
	for _, dep := range info.Deps {
		switch dep.Path {
		case "go.opentelemetry.io/collector/otelcol":
			return strings.TrimPrefix(dep.Version, "v")
		case "go.opentelemetry.io/collector/exporter/exporterhelper":
			version = strings.TrimPrefix(dep.Version, "v")
		}
	}
	return version
})

func (m *Manager) report(applied uint64, desiredErr string) *controlpb.CollectorReport {
	identity := &controlpb.Identity{Cluster: m.config.Cluster, Namespace: m.config.Namespace, Pod: m.config.Pod, CollectorGroup: m.config.CollectorGroup, CollectorVersion: collectorVersion(), DistributionVersion: m.settings.BuildInfo.Version, Enabled: m.enabled.Load(), Capabilities: capabilities,
		FilterLimits: &controlpb.FilterLimits{MaxConditions: maxFilterConditions, MaxConditionChars: maxConditionChars, MaxRegexChars: maxRegexChars}}
	for _, point := range m.CapturePoints() {
		sensitivity := controlpb.Sensitivity_SENSITIVITY_REDACTED
		if point.Sensitivity == "raw" {
			sensitivity = controlpb.Sensitivity_SENSITIVITY_RAW
		}
		identity.CapturePoints = append(identity.CapturePoints, &controlpb.CapturePoint{Pipeline: point.Pipeline, Point: point.Point.Point, Signal: signalsToProto[point.Signal], Sensitivity: sensitivity})
	}
	report := &controlpb.CollectorReport{Identity: identity, AppliedGeneration: applied, DesiredStateError: desiredErr}
	for _, status := range m.Statuses() {
		report.Sessions = append(report.Sessions, statusToProto(status))
	}
	return report
}

func (m *Manager) watch(ctx context.Context) {
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		connectedAt := time.Now()
		err := m.watchOnce(ctx)
		m.telemetry.connected.Record(context.Background(), 0)
		if ctx.Err() != nil {
			return
		}
		// Never log the raw server message or telemetry on control failures.
		m.settings.Logger.Debug("capture control disconnected; reconnecting")
		_ = err
		if time.Since(connectedAt) >= 2*time.Second {
			delay = 100 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

func (m *Manager) watchOnce(ctx context.Context) error {
	conn, err := m.config.Control.ToClientConn(ctx, m.host.GetExtensions(), m.settings.TelemetrySettings)
	if err != nil {
		return err
	}
	defer conn.Close()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := controlpb.NewCaptureControlClient(conn).WatchCaptures(streamCtx, grpc.MaxCallRecvMsgSize(64<<10), grpc.MaxCallSendMsgSize(1<<20))
	if err != nil {
		return err
	}
	var applied uint64
	var desiredErr string
	send := func() error { return stream.Send(m.report(applied, desiredErr)) }
	if err := send(); err != nil {
		return err
	}
	m.telemetry.connected.Record(context.Background(), 1)
	messages := make(chan *controlpb.DesiredState, 1)
	failures := make(chan error, 1)
	receiverDone := make(chan struct{})
	go func() {
		defer close(receiverDone)
		for {
			message, err := stream.Recv()
			if err != nil {
				select {
				case failures <- err:
				case <-streamCtx.Done():
				}
				return
			}
			select {
			case messages <- message:
			case <-streamCtx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-receiverDone }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failures:
			return err
		case <-ticker.C:
			if err := send(); err != nil {
				return err
			}
		case message := <-messages:
			applied, desiredErr = message.GetGeneration(), ""
			if err := m.applyDesired(streamCtx, message); err != nil {
				desiredErr = truncate(err.Error(), 1024)
			}
			if err := send(); err != nil {
				return err
			}
		}
	}
}
