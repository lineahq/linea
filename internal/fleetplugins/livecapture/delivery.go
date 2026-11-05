package livecapture

import (
	"context"
	"time"

	"github.com/lineahq/linea/pluginsdk"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type channel struct {
	id, owner         string
	expires, lastSeen time.Time
	batches           []*batch
	bytes             int
	receipts          map[string]int64
	closed            bool
}
type batch struct {
	envelope  Envelope
	offered   bool
	done      chan struct{}
	accepted  int64
	finished  bool
	uncertain bool
}
type ChannelInfo struct {
	AuthorityID      string    `json:"authority_id"`
	PluginInstance   string    `json:"plugin_instance"`
	ID               string    `json:"id"`
	ExpiresAt        time.Time `json:"expires_at"`
	HeartbeatSeconds int       `json:"heartbeat_seconds"`
}

func (s *Server) NewChannel(ctx context.Context, owner string, expiry time.Time) (ChannelInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hostOK || !s.dbOK {
		return ChannelInfo{}, ErrUnavailable
	}
	if expected, ok := ctx.Value(authorityContextKey{}).(string); ok && expected != s.epoch {
		return ChannelInfo{}, ErrConflict
	}
	if !expiry.After(time.Now()) || expiry.After(time.Now().Add(5*time.Minute)) {
		return ChannelInfo{}, ErrInvalid
	}
	if len(s.channels) >= 32 {
		return ChannelInfo{}, ErrCapacity
	}
	c := &channel{id: pluginsdk.RandomID(), owner: owner, expires: expiry, lastSeen: time.Now(), receipts: map[string]int64{}}
	s.channels[c.id] = c
	return ChannelInfo{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, ID: c.id, ExpiresAt: c.expires, HeartbeatSeconds: 5}, nil
}
func (s *Server) closeChannelLocked(c *channel) {
	if c.closed {
		return
	}
	c.closed = true
	for _, b := range c.batches {
		if !b.finished {
			b.finished = true
			b.uncertain = true
			close(b.done)
		}
		b.envelope.Data = nil
	}
	c.batches = nil
	c.bytes = 0
	for _, a := range s.operations {
		if a.op.ChannelID == c.id {
			if a.op.Reason == "" {
				a.op.Reason = "consumer_unavailable"
			}
			_ = s.stopLocked(a, "stopping")
		}
	}
}
func (s *Server) Next(owner, id string) (*Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.channels[id]
	if c == nil || c.closed || !time.Now().Before(c.expires) || time.Since(c.lastSeen) > 5*time.Second {
		return nil, ErrNotFound
	}
	if c.owner != owner {
		return nil, ErrForbidden
	}
	c.lastSeen = time.Now()
	for _, b := range c.batches {
		if !b.finished {
			b.offered = true
			copy := b.envelope
			return &copy, nil
		}
	}
	return nil, nil
}
func (s *Server) Ack(owner, id string, ack Acknowledgement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.channels[id]
	if c == nil || c.closed || time.Since(c.lastSeen) > 5*time.Second || !time.Now().Before(c.expires) {
		return ErrNotFound
	}
	if c.owner != owner {
		return ErrForbidden
	}
	if accepted, ok := c.receipts[ack.ID]; ok {
		if accepted == ack.Accepted {
			s.observed.ackRetries.Add(1)
			return nil
		}
		return ErrConflict
	}
	for _, b := range c.batches {
		if b.envelope.ID == ack.ID {
			if !b.offered || b.finished || ack.Accepted < 0 || ack.Accepted > b.envelope.Records {
				return ErrConflict
			}
			if len(c.receipts) >= 1024 {
				return ErrCapacity
			}
			b.accepted = ack.Accepted
			b.finished = true
			c.receipts[ack.ID] = ack.Accepted
			s.observed.acks.Add(1)
			index := signalIndex(b.envelope.Signal)
			s.observed.accepted[index].Add(uint64(ack.Accepted))
			s.observed.rejected[index].Add(uint64(b.envelope.Records - ack.Accepted))
			c.lastSeen = time.Now()
			close(b.done)
			return nil
		}
	}
	return ErrNotFound
}
func (s *Server) DeleteChannel(owner, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.channels[id]
	if c == nil {
		return ErrNotFound
	}
	if c.owner != owner {
		return ErrForbidden
	}
	s.closeChannelLocked(c)
	delete(s.channels, id)
	return nil
}

func attr(attributes pcommon.Map, key string) string {
	v, ok := attributes.Get(key)
	if !ok || v.Type() != pcommon.ValueTypeStr {
		return ""
	}
	return v.Str()
}
func (s *Server) bindingLocked(credentialID, signal string, resources []pcommon.Map) (*activeOperation, Target, error) {
	var result *activeOperation
	var source Target
	if !s.hostOK || !s.dbOK {
		return nil, source, ErrUnavailable
	}
	for _, attributes := range resources {
		wire := attr(attributes, "capture.session.id")
		point := attr(attributes, "capture.point")
		pipeline := attr(attributes, "capture.pipeline")
		var found *activeOperation
		var target Target
		for _, a := range s.operations {
			// A stopping target drains the copies it queued before the stop
			// until it reports a terminal state and is released.
			if (a.op.State != "starting" && a.op.State != "active" && a.op.State != "stopping") || !time.Now().Before(a.op.ExpiresAt) {
				continue
			}
			for _, t := range a.op.Targets {
				if t.WireID == wire && !t.Released {
					found = a
					target = t
				}
			}
		}
		if found == nil {
			return nil, source, ErrForbidden
		}
		p := s.peers[target.PeerID]
		bound := p != nil && (p.operation == found.op.ID || found.op.State == "stopping" && p.operation == "")
		if !bound || !p.alive() || p.info.CredentialID != credentialID || p.info.Generation != target.Generation || attr(attributes, "capture.collector.pod") != p.info.Identity.Pod || attr(attributes, "capture.cluster") != p.info.Identity.Cluster {
			return nil, source, ErrForbidden
		}
		validPoint, validSignal := false, false
		for _, v := range found.request.Points {
			validPoint = validPoint || (v.Pipeline == pipeline && v.Point == point)
		}
		for _, v := range found.request.Signals {
			validSignal = validSignal || v == signal
		}
		if !validPoint || !validSignal || result != nil && (result != found || source.PeerID != target.PeerID) {
			return nil, source, ErrForbidden
		}
		result, source = found, target
	}
	if result == nil {
		return nil, source, ErrInvalid
	}
	return result, source, nil
}
func (s *Server) deliver(ctx context.Context, signal string, resources []pcommon.Map, count int64, data []byte) (accepted int64, resultErr error) {
	started := time.Now()
	defer func() {
		s.observeDelivery(signal, started, resultErr)
		if status.Code(resultErr) == codes.ResourceExhausted {
			s.observed.deliveryCapacity.Add(1)
		}
	}()
	credential, err := s.authenticate(ctx)
	if err != nil {
		return 0, err
	}
	if len(data) > 256<<10 || count <= 0 || len(resources) > 256 {
		return 0, status.Error(codes.ResourceExhausted, "capture batch bound")
	}
	select {
	case s.dataGate <- struct{}{}:
		defer func() { <-s.dataGate }()
	default:
		return 0, status.Error(codes.ResourceExhausted, "capture delivery busy")
	}
	s.mu.Lock()
	a, target, err := s.bindingLocked(credential.id, signal, resources)
	if err != nil {
		s.mu.Unlock()
		return 0, status.Error(codes.PermissionDenied, "capture source/lease refused")
	}
	c := s.channels[a.op.ChannelID]
	if c == nil || c.closed || time.Since(c.lastSeen) > 5*time.Second || !time.Now().Before(c.expires) {
		s.mu.Unlock()
		return 0, status.Error(codes.Unavailable, "capture consumer unavailable")
	}
	if len(c.batches) >= 16 || c.bytes+len(data) > 1<<20 || len(c.receipts) >= 1024 {
		if a.op.Reason == "" {
			a.op.Reason = "delivery_capacity"
		}
		_ = s.stopLocked(a, "stopping")
		s.mu.Unlock()
		return 0, status.Error(codes.ResourceExhausted, "capture consumer capacity")
	}
	b := &batch{envelope: Envelope{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, PluginEpoch: s.epoch, ID: pluginsdk.RandomID(), CaptureID: a.op.ID, PeerID: target.PeerID, Generation: target.Generation, Signal: signal, Records: count, Encoding: "otlp-protobuf", Data: data, ObservedAt: time.Now().UTC()}, done: make(chan struct{})}
	c.batches = append(c.batches, b)
	c.bytes += len(data)
	s.mu.Unlock()
	select {
	case <-b.done:
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !b.finished {
		b.finished = true
		b.uncertain = true
		close(b.done)
		if a.op.Reason == "" {
			a.op.Reason = "receipt_uncertain"
		}
		_ = s.stopLocked(a, "stopping")
	}
	for i, entry := range c.batches {
		if entry == b {
			c.batches = append(c.batches[:i], c.batches[i+1:]...)
			c.bytes -= len(b.envelope.Data)
			break
		}
	}
	b.envelope.Data = nil
	if b.uncertain {
		s.observed.uncertain.Add(1)
		return 0, status.Error(codes.Unavailable, "capture receipt uncertain")
	}
	return b.accepted, nil
}

type traceReceiver struct {
	ptraceotlp.UnimplementedGRPCServer
	server *Server
}

func (r *traceReceiver) Export(ctx context.Context, request ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	resources := []pcommon.Map{}
	for _, v := range request.Traces().ResourceSpans().All() {
		resources = append(resources, v.Resource().Attributes())
	}
	data, err := request.MarshalProto()
	if err != nil {
		return ptraceotlp.NewExportResponse(), status.Error(codes.InvalidArgument, "invalid OTLP")
	}
	accepted, err := r.server.deliver(ctx, "traces", resources, int64(request.Traces().SpanCount()), data)
	response := ptraceotlp.NewExportResponse()
	if err == nil && accepted < int64(request.Traces().SpanCount()) {
		response.PartialSuccess().SetRejectedSpans(int64(request.Traces().SpanCount()) - accepted)
		response.PartialSuccess().SetErrorMessage("consumer partially accepted capture")
	}
	return response, err
}

type logReceiver struct {
	plogotlp.UnimplementedGRPCServer
	server *Server
}

func (r *logReceiver) Export(ctx context.Context, request plogotlp.ExportRequest) (plogotlp.ExportResponse, error) {
	resources := []pcommon.Map{}
	for _, v := range request.Logs().ResourceLogs().All() {
		resources = append(resources, v.Resource().Attributes())
	}
	data, err := request.MarshalProto()
	if err != nil {
		return plogotlp.NewExportResponse(), status.Error(codes.InvalidArgument, "invalid OTLP")
	}
	accepted, err := r.server.deliver(ctx, "logs", resources, int64(request.Logs().LogRecordCount()), data)
	response := plogotlp.NewExportResponse()
	if err == nil && accepted < int64(request.Logs().LogRecordCount()) {
		response.PartialSuccess().SetRejectedLogRecords(int64(request.Logs().LogRecordCount()) - accepted)
		response.PartialSuccess().SetErrorMessage("consumer partially accepted capture")
	}
	return response, err
}

type metricReceiver struct {
	pmetricotlp.UnimplementedGRPCServer
	server *Server
}

func (r *metricReceiver) Export(ctx context.Context, request pmetricotlp.ExportRequest) (pmetricotlp.ExportResponse, error) {
	resources := []pcommon.Map{}
	for _, v := range request.Metrics().ResourceMetrics().All() {
		resources = append(resources, v.Resource().Attributes())
	}
	data, err := request.MarshalProto()
	if err != nil {
		return pmetricotlp.NewExportResponse(), status.Error(codes.InvalidArgument, "invalid OTLP")
	}
	accepted, err := r.server.deliver(ctx, "metrics", resources, int64(request.Metrics().DataPointCount()), data)
	response := pmetricotlp.NewExportResponse()
	if err == nil && accepted < int64(request.Metrics().DataPointCount()) {
		response.PartialSuccess().SetRejectedDataPoints(int64(request.Metrics().DataPointCount()) - accepted)
		response.PartialSuccess().SetErrorMessage("consumer partially accepted capture")
	}
	return response, err
}
