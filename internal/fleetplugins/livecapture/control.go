package livecapture

import (
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lineahq/linea/collector/livecapture/controlpb"
)

type peer struct {
	info       PeerInfo
	credential [32]byte
	linkedAt   time.Time
	stream     grpc.ServerStream
	lastSeen   time.Time
	operation  string
	wake       chan struct{}
	done       chan struct{}
	once       sync.Once
}

func (p *peer) close() { p.once.Do(func() { close(p.done) }) }
func (p *peer) wakeup() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *peer) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

type report struct {
	Identity Identity
	Statuses []collectorStatus
	// Applied is the last desired-state generation the Collector processed on
	// this stream; DesiredStateError says why it was rejected, if it was.
	Applied           uint64
	DesiredStateError string
}
type collectorStatus struct {
	SessionID string
	State     string
	Stats     *TargetStats
}

var signalNames = map[controlpb.Signal]string{controlpb.Signal_SIGNAL_TRACES: "traces", controlpb.Signal_SIGNAL_LOGS: "logs", controlpb.Signal_SIGNAL_METRICS: "metrics"}
var signalValues = map[string]controlpb.Signal{"traces": controlpb.Signal_SIGNAL_TRACES, "logs": controlpb.Signal_SIGNAL_LOGS, "metrics": controlpb.Signal_SIGNAL_METRICS}
var sensitivityNames = map[controlpb.Sensitivity]string{controlpb.Sensitivity_SENSITIVITY_RAW: "raw", controlpb.Sensitivity_SENSITIVITY_REDACTED: "redacted"}
var samplingModes = map[string]controlpb.SamplingMode{"": controlpb.SamplingMode_SAMPLING_MODE_UNSPECIFIED, "hash": controlpb.SamplingMode_SAMPLING_MODE_HASH, "random": controlpb.SamplingMode_SAMPLING_MODE_RANDOM}
var sessionStates = map[controlpb.SessionState]string{
	controlpb.SessionState_SESSION_STATE_ACTIVE: "active", controlpb.SessionState_SESSION_STATE_STOPPING: "stopping", controlpb.SessionState_SESSION_STATE_COMPLETED: "completed",
	controlpb.SessionState_SESSION_STATE_EXPIRED: "expired", controlpb.SessionState_SESSION_STATE_LIMIT_REACHED: "limit_reached", controlpb.SessionState_SESSION_STATE_FAILED: "failed", controlpb.SessionState_SESSION_STATE_DISABLED: "disabled",
}

// decodeReport maps a wire report onto the plugin's model. Unknown signals and
// sensitivities become empty and fail identity validation; unknown session
// states are ignored.
func decodeReport(message *controlpb.CollectorReport) (report, error) {
	var r report
	i := message.GetIdentity()
	if i == nil || len(message.GetSessions()) > 64 || len(message.GetDesiredStateError()) > 4096 {
		return r, ErrInvalid
	}
	r.Identity = Identity{Cluster: i.GetCluster(), Namespace: i.GetNamespace(), Pod: i.GetPod(), Group: i.GetCollectorGroup(), CollectorVersion: i.GetCollectorVersion(), DistributionVersion: i.GetDistributionVersion(), Enabled: i.GetEnabled(), Points: make([]CatalogPoint, 0, len(i.GetCapturePoints()))}
	l := i.GetFilterLimits()
	r.Identity.FilterLimits = FilterLimits{MaxConditions: int(min(l.GetMaxConditions(), 1<<16)), MaxConditionChars: int(min(l.GetMaxConditionChars(), 1<<16)), MaxRegexChars: int(min(l.GetMaxRegexChars(), 1<<16))}.orV1()
	for _, p := range i.GetCapturePoints() {
		r.Identity.Points = append(r.Identity.Points, CatalogPoint{Pipeline: p.GetPipeline(), Point: p.GetPoint(), Signal: signalNames[p.GetSignal()], Sensitivity: sensitivityNames[p.GetSensitivity()]})
	}
	for _, status := range message.GetSessions() {
		stats, err := decodeStats(status.GetStats())
		if err != nil {
			return r, err
		}
		r.Statuses = append(r.Statuses, collectorStatus{SessionID: status.GetSessionId(), State: sessionStates[status.GetState()], Stats: stats})
	}
	r.Applied, r.DesiredStateError = message.GetAppliedGeneration(), message.GetDesiredStateError()
	return r, nil
}

// decodeStats bounds a session's reported counters; nil when none were sent.
func decodeStats(v *controlpb.SessionStats) (*TargetStats, error) {
	if v == nil {
		return nil, nil
	}
	if len(v.GetDropped()) > 16 || v.GetSelected() < 0 || v.GetEnqueuedRecords() < 0 || v.GetEnqueuedBytes() < 0 || v.GetFilterErrors() < 0 {
		return nil, ErrInvalid
	}
	stats := &TargetStats{Selected: v.GetSelected(), EnqueuedRecords: v.GetEnqueuedRecords(), EnqueuedBytes: v.GetEnqueuedBytes(), FilterErrors: v.GetFilterErrors()}
	for reason, n := range v.GetDropped() {
		if reason == "" || len(reason) > 32 || n < 0 {
			return nil, ErrInvalid
		}
		if n > 0 {
			if stats.Dropped == nil {
				stats.Dropped = map[string]int64{}
			}
			stats.Dropped[reason] = n
		}
	}
	return stats, nil
}

// captureControl serves the generated CaptureControl service.
type captureControl struct {
	controlpb.UnimplementedCaptureControlServer
	server *Server
}

func (c captureControl) WatchCaptures(stream controlpb.CaptureControl_WatchCapturesServer) error {
	return c.server.Watch(stream)
}
func (s *Server) Watch(stream grpc.ServerStream) error {
	credential, err := s.authenticate(stream.Context())
	if err != nil {
		return err
	}
	first := &controlpb.CollectorReport{}
	initial := make(chan error, 1)
	go func() { initial <- stream.RecvMsg(first) }()
	select {
	case err = <-initial:
		if err != nil {
			return err
		}
	case <-time.After(2 * time.Second):
		return status.Error(codes.DeadlineExceeded, "capture identity deadline")
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	r, err := decodeReport(first)
	if err != nil || !validIdentity(r.Identity, credential) {
		return status.Error(codes.InvalidArgument, "capture identity/catalog refused")
	}
	id := identityKey(credential.id, r.Identity)
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "capture authority draining")
	}
	if len(s.peers) >= s.config.peerLimit() || s.peers[id] != nil {
		s.mu.Unlock()
		return status.Error(codes.ResourceExhausted, "capture connection capacity")
	}
	gen, err := s.repo.NextGeneration(stream.Context(), id)
	if err != nil {
		s.mu.Unlock()
		return status.Error(codes.Unavailable, "capture generation unavailable")
	}
	p := &peer{info: PeerInfo{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, ID: id, Generation: gen, CredentialID: credential.id, Identity: r.Identity}, credential: credential.hash, stream: stream, lastSeen: time.Now(), wake: make(chan struct{}, 1), done: make(chan struct{})}
	s.peers[id] = p
	s.mu.Unlock()
	s.wakeLinker()
	defer func() {
		p.close()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.peers[id] != p {
			return
		}
		delete(s.peers, id)
		for _, a := range s.operations {
			for i, t := range a.op.Targets {
				if t.PeerID == id && t.Generation == gen && !t.Released {
					a.op.Targets[i].State = "unknown"
					if a.op.Reason == "" {
						a.op.Reason = "peer_disconnected"
					}
					if a.op.State == "stopping" {
						_ = s.saveLocked(a)
					}
					_ = s.stopLocked(a, "stopping")
				}
			}
		}
	}()
	// Full empty desired state is always sent first. Reconnect never replays starts.
	p.wakeup()
	failures := make(chan error, 2)
	go func() {
		var rejected uint64
		for {
			message := &controlpb.CollectorReport{}
			if err := stream.RecvMsg(message); err != nil {
				failures <- err
				return
			}
			update, err := decodeReport(message)
			if err != nil || !validIdentity(update.Identity, credential) || identityKey(credential.id, update.Identity) != id {
				failures <- ErrInvalid
				return
			}
			s.mu.Lock()
			p.info.Identity = update.Identity
			p.lastSeen = time.Now()
			s.applyReportLocked(p, update)
			s.mu.Unlock()
			// Reports repeat until the next desired state; count each rejected
			// generation once. A rejected session also reports failed.
			if update.DesiredStateError != "" && update.Applied > rejected {
				rejected = update.Applied
				s.observed.desiredRejected.Add(1)
			}
		}
	}()
	go func() {
		var generation uint64
		for {
			select {
			case <-p.done:
				return
			case <-stream.Context().Done():
				return
			case <-p.wake:
				if s.repo.Ready(stream.Context()) != nil {
					failures <- ErrUnavailable
					return
				}
				s.mu.Lock()
				message, err := s.desiredLocked(p)
				s.mu.Unlock()
				if err != nil {
					failures <- err
					return
				}
				generation++
				message.Generation = generation
				// grpc SendMsg has no caller deadline; retiring this RPC cancels its IO.
				timer := time.AfterFunc(time.Second, p.close)
				err = stream.SendMsg(message)
				timer.Stop()
				if err != nil {
					failures <- err
					return
				}
			}
		}
	}()
	select {
	case <-s.ctx.Done():
		return status.Error(codes.Unavailable, "capture authority stopping")
	case <-p.done:
		return status.Error(codes.Unavailable, "capture connection retired")
	case <-stream.Context().Done():
		return stream.Context().Err()
	case <-failures:
		return status.Error(codes.Unavailable, "capture control disconnected")
	}
}

// desiredLocked builds the complete desired state for p; the sender stamps
// its stream generation.
func (s *Server) desiredLocked(p *peer) (*controlpb.DesiredState, error) {
	message := &controlpb.DesiredState{}
	if s.hostOK && s.dbOK && p.operation != "" {
		if a := s.operations[p.operation]; a != nil && (a.op.State == "starting" || a.op.State == "active") && time.Now().Before(a.op.ExpiresAt) {
			for _, t := range a.op.Targets {
				if t.PeerID == p.info.ID && t.Generation == p.info.Generation {
					session, err := wireSession(a, t.WireID, p.info.Identity)
					if err != nil {
						return nil, err
					}
					message.Sessions = append(message.Sessions, session)
				}
			}
		}
	}
	if proto.Size(message) > 64<<10 {
		return nil, ErrInvalid
	}
	return message, nil
}
func wireSession(a *activeOperation, id string, target Identity) (*controlpb.Session, error) {
	r := a.request
	limits := r.Limits
	if limits.TotalRecords == 0 {
		limits.TotalRecords = 10000
	}
	if limits.TotalBytes == 0 {
		limits.TotalBytes = 16 << 20
	}
	session := &controlpb.Session{
		SessionId: id, Principal: a.op.Owner,
		Target:    &controlpb.Target{CollectorGroup: target.Group, Pod: target.Pod},
		Filter:    &controlpb.Filter{Resource: r.Filter.Resource, Span: r.Filter.Traces.Span, Log: r.Filter.Logs.Log, Metric: r.Filter.Metrics.Metric, Datapoint: r.Filter.Metrics.Datapoint},
		Limits:    &controlpb.Limits{BytesPerSecond: limits.BytesPerSecond, RecordsPerSecond: limits.RecordsPerSecond, TotalBytes: limits.TotalBytes, TotalRecords: limits.TotalRecords, RecordBytes: limits.RecordBytes},
		ExpiresAt: timestamppb.New(a.op.ExpiresAt), AuthorizedRaw: a.raw,
	}
	for _, point := range r.Points {
		session.CapturePoints = append(session.CapturePoints, &controlpb.Point{Pipeline: point.Pipeline, Point: point.Point})
	}
	for _, signal := range r.Signals {
		value, ok := signalValues[signal]
		if !ok {
			return nil, ErrInvalid
		}
		session.Signals = append(session.Signals, value)
	}
	if r.Sampling != nil {
		mode, ok := samplingModes[r.Sampling.Mode]
		if !ok {
			return nil, ErrInvalid
		}
		session.Sampling = &controlpb.Sampling{Rate: r.Sampling.Rate, Mode: mode}
	}
	return session, nil
}
func (s *Server) applyReportLocked(p *peer, r report) {
	for _, a := range s.operations {
		changed := false
		rejected := false
		for i, t := range a.op.Targets {
			if t.PeerID != p.info.ID || t.Generation != p.info.Generation || t.Released {
				continue
			}
			for _, observed := range r.Statuses {
				if observed.SessionID != t.WireID {
					continue
				}
				if observed.State != "active" && observed.State != "stopping" && !terminal(observed.State) {
					continue
				}
				// Counters alone do not change the revision; they are saved
				// with the next state change and read live until then.
				if observed.Stats != nil {
					a.op.Targets[i].Stats = observed.Stats
				}
				if a.op.Targets[i].State != observed.State {
					a.op.Targets[i].State = observed.State
					changed = true
				}
				if terminal(observed.State) {
					a.op.Targets[i].Released = true
					changed = true
					if observed.State == "failed" || observed.State == "disabled" {
						rejected = true
					}
					if p.operation == a.op.ID {
						p.operation = ""
						p.wakeup()
					}
				}
			}
		}
		if !changed {
			continue
		}
		if rejected {
			if a.op.Reason == "" {
				a.op.Reason = "collector_rejected"
			}
			_ = s.stopLocked(a, "stopping")
		}
		allActive, allDone := true, true
		for _, t := range a.op.Targets {
			allActive = allActive && t.State == "active"
			allDone = allDone && t.Released
		}
		if allActive && a.op.State == "starting" {
			a.op.State = "active"
		}
		if allDone {
			if a.op.State == "stopping" {
				a.op.State = "stopped"
			} else {
				a.op.State = "completed"
			}
			for _, t := range a.op.Targets {
				if t.State == "expired" {
					a.op.State = "expired"
					if a.op.Reason == "" {
						a.op.Reason = "absolute_expiry"
					}
				}
				if t.State == "failed" || t.State == "disabled" {
					a.op.State = "failed"
					if a.op.Reason == "" {
						a.op.Reason = "collector_rejected"
					}
				}
			}
		}
		if s.saveLocked(a) != nil {
			return
		}
		if allDone {
			delete(s.operations, a.op.ID)
		}
	}
}
