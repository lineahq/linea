package livecapture

import (
	"context"
	"time"

	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
	"github.com/lineahq/linea/pluginsdk"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Delivery buffer bounds. The Collector sends and forgets: a batch that does
// not fit, or waits longer than batchAge, is dropped and counted for its
// target, never refused.
const (
	channelBytes   = 2 << 20
	channelBatches = 64
	pendingBytes   = 32 << 20
	batchAge       = 10 * time.Second
)

type channel struct {
	id, owner         string
	expires, lastSeen time.Time
	batches           []*batch
	bytes             int
	closed            bool
}
type batch struct {
	envelope Envelope
	received time.Time
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
		return ChannelInfo{}, ErrStaleAuthority
	}
	now := s.now()
	if !expiry.After(now) || expiry.After(now.Add(5*time.Minute)) {
		return ChannelInfo{}, ErrInvalid
	}
	if len(s.channels) >= 32 {
		return ChannelInfo{}, ErrCapacity
	}
	c := &channel{id: pluginsdk.RandomID(), owner: owner, expires: expiry, lastSeen: now}
	s.channels[c.id] = c
	return ChannelInfo{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, ID: c.id, ExpiresAt: c.expires, HeartbeatSeconds: 5}, nil
}
func (s *Server) closeChannelLocked(c *channel) {
	if c.closed {
		return
	}
	c.closed = true
	for _, b := range c.batches {
		s.dropLocked(b.envelope, false)
	}
	s.pending -= c.bytes
	c.batches = nil
	c.bytes = 0
	for _, a := range s.operations {
		if a.op.ChannelID == c.id {
			if a.op.Reason == "" {
				a.op.Reason = "consumer_unavailable"
			}
			s.stopLocked(a, "stopping")
		}
	}
}

// liveChannelLocked returns the owner's channel while its consumer polls.
func (s *Server) liveChannelLocked(owner, id string, now time.Time) (*channel, error) {
	c := s.channels[id]
	if c == nil || c.closed || !now.Before(c.expires) || now.Sub(c.lastSeen) > 5*time.Second {
		return nil, ErrNotFound
	}
	if c.owner != owner {
		return nil, ErrForbidden
	}
	return c, nil
}

// ageLocked drops the batches that waited longer than batchAge. Batches are
// in arrival order, so the expired ones are a prefix.
func (s *Server) ageLocked(c *channel, now time.Time) {
	n := 0
	for n < len(c.batches) && now.Sub(c.batches[n].received) > batchAge {
		s.dropLocked(c.batches[n].envelope, false)
		n++
	}
	if n > 0 {
		c.batches = s.removeLocked(c, 0, n)
	}
}

// removeLocked frees batches [from, to) of c and returns the rest.
func (s *Server) removeLocked(c *channel, from, to int) []*batch {
	for _, b := range c.batches[from:to] {
		c.bytes -= len(b.envelope.Data)
		s.pending -= len(b.envelope.Data)
		b.envelope.Data = nil
	}
	rest := append(c.batches[:from], c.batches[to:]...)
	clear(c.batches[len(rest):])
	return rest
}

// dropLocked counts a batch the consumer will never see, for its target and
// in the plugin metrics. received is true when the batch is counted on
// arrival and was never counted as received before.
func (s *Server) dropLocked(e Envelope, received bool) {
	index := signalIndex(e.Signal)
	s.observed.droppedRecords[index].Add(uint64(e.Records))
	s.observed.droppedBatches.Add(1)
	a := s.operations[e.CaptureID]
	if a == nil {
		return
	}
	for i, t := range a.op.Targets {
		if t.PeerID == e.PeerID && t.Generation == e.Generation {
			d := &a.op.Targets[i].Delivery
			if received {
				d.ReceivedRecords += e.Records
			}
			d.DroppedRecords += e.Records
			d.DroppedBatches++
			return
		}
	}
}

func (s *Server) Next(owner, id string) (*Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c, err := s.liveChannelLocked(owner, id, now)
	if err != nil {
		return nil, err
	}
	c.lastSeen = now
	s.ageLocked(c, now)
	if len(c.batches) == 0 {
		return nil, nil
	}
	copy := c.batches[0].envelope
	return &copy, nil
}

// Ack frees a batch the consumer has stored. An ACK for a batch that is not
// pending, such as a repeated one, succeeds without effect.
func (s *Server) Ack(owner, id string, ack Acknowledgement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	c, err := s.liveChannelLocked(owner, id, now)
	if err != nil {
		return err
	}
	c.lastSeen = now
	for i, b := range c.batches {
		if b.envelope.ID == ack.ID {
			s.observed.acks.Add(1)
			s.observed.delivered[signalIndex(b.envelope.Signal)].Add(uint64(b.envelope.Records))
			c.batches = s.removeLocked(c, i, i+1)
			return nil
		}
	}
	return nil
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

// deliver answers every authenticated, bound and bounded export at once. A
// batch the buffer cannot hold, or whose consumer is gone, is dropped and
// counted for its target; the Collector never learns of it and never waits.
func (s *Server) deliver(ctx context.Context, signal string, resources []pcommon.Map, count int64, data []byte) (resultErr error) {
	started := time.Now()
	defer func() { s.observeDelivery(signal, started, resultErr) }()
	credential, err := s.authenticate(ctx)
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			s.authFailures.Record(translog.ListenerOTLP)
		}
		return err
	}
	if len(data) > 256<<10 || count <= 0 || len(resources) > 256 {
		return status.Error(codes.ResourceExhausted, "capture batch bound")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, target, err := s.bindingLocked(credential.id, signal, resources)
	if err != nil {
		return status.Error(codes.PermissionDenied, "capture source/lease refused")
	}
	now := s.now()
	e := Envelope{AuthorityID: s.repo.AuthorityID(), PluginInstance: s.instance, PluginEpoch: s.epoch, ID: pluginsdk.RandomID(), CaptureID: a.op.ID, PeerID: target.PeerID, Generation: target.Generation, Signal: signal, Records: count, Encoding: "otlp-protobuf", Data: data, ObservedAt: now.UTC()}
	c := s.channels[a.op.ChannelID]
	if c == nil || c.closed || now.Sub(c.lastSeen) > 5*time.Second || !now.Before(c.expires) {
		s.dropLocked(e, true)
		return nil
	}
	s.ageLocked(c, now)
	if len(c.batches) >= channelBatches || c.bytes+len(data) > channelBytes || s.pending+len(data) > pendingBytes {
		s.dropLocked(e, true)
		return nil
	}
	for i, t := range a.op.Targets {
		if t.PeerID == target.PeerID && t.Generation == target.Generation {
			a.op.Targets[i].Delivery.ReceivedRecords += count
		}
	}
	c.batches = append(c.batches, &batch{envelope: e, received: now})
	c.bytes += len(data)
	s.pending += len(data)
	return nil
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
	return ptraceotlp.NewExportResponse(), r.server.deliver(ctx, "traces", resources, int64(request.Traces().SpanCount()), data)
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
	return plogotlp.NewExportResponse(), r.server.deliver(ctx, "logs", resources, int64(request.Logs().LogRecordCount()), data)
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
	return pmetricotlp.NewExportResponse(), r.server.deliver(ctx, "metrics", resources, int64(request.Metrics().DataPointCount()), data)
}
