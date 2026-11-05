package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
	"github.com/lineahq/linea/internal/debugger/provenance"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

const captureSessionBytes int64 = 8 << 20
const captureMemoryBytes int64 = 64 << 20
const captureRecordsLimit = 10000

// CaptureDataStatus is the state of a capture's received records and
// their budgets. Records, raw ones included, are kept in the workspace until
// the capture is deleted; MemoryOnly means the debugger has no workspace.
type CaptureDataStatus struct {
	ByteLimit       int64  `json:"byte_limit"`
	RecordLimit     int    `json:"record_limit"`
	ContainsRaw     bool   `json:"contains_raw"`
	Enabled         bool   `json:"enabled"`
	State           string `json:"state"`
	MemoryOnly      bool   `json:"memory_only"`
	ReceivedRecords int64  `json:"received_records"`
	ReceivedBytes   int64  `json:"received_bytes"`
	RejectedRecords int64  `json:"rejected_records"`
	Error           string `json:"error,omitempty"`
}

// CaptureStream names the records of one Collector, pipeline, capture
// point and signal.
type CaptureStream = provenance.CaptureStream
type capturedRecord struct {
	Sequence    int           `json:"sequence"`
	Stream      CaptureStream `json:"stream"`
	ReceivedAt  time.Time     `json:"received_at"`
	Sensitivity string        `json:"sensitivity"`
	Proto       []byte        `json:"payload_base64"`
}

// CaptureRecord is one received OTLP request, decoded unless it is
// oversize.
type CaptureRecord struct {
	Sequence    int             `json:"sequence"`
	Stream      CaptureStream   `json:"stream"`
	ReceivedAt  time.Time       `json:"received_at"`
	Sensitivity string          `json:"sensitivity"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Oversize    bool            `json:"oversize,omitempty"`
}

// CaptureRecordsRequest pages a capture's records, optionally of one stream.
type CaptureRecordsRequest struct {
	Stream CaptureStream `json:"stream"`
	Limit  int           `json:"limit,omitempty"`
	Cursor string        `json:"cursor,omitempty"`
}

// CaptureRecordsPage is a page of records up to the watermark, the
// sequence the first page saw last.
type CaptureRecordsPage struct {
	SessionID  string          `json:"session_id"`
	Watermark  int             `json:"watermark"`
	Items      []CaptureRecord `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func streamKey(v CaptureStream) string {
	return v.CollectorID + "\x00" + v.Pipeline + "\x00" + v.Point + "\x00" + v.Signal
}

func matchesStream(want, got CaptureStream) bool {
	return (want.CollectorID == "" || want.CollectorID == got.CollectorID) &&
		(want.Pipeline == "" || want.Pipeline == got.Pipeline) &&
		(want.Point == "" || want.Point == got.Point) &&
		(want.Signal == "" || want.Signal == got.Signal)
}

func (r *collectorRegistry) dataMemoryReservedLocked() int64 {
	var n int64
	for _, c := range r.sessions {
		if c.view.Data.Enabled {
			if c.view.Data.State == "open" || c.view.Data.State == "draining" {
				n += captureSessionBytes
			} else {
				for _, record := range c.records {
					n += int64(len(record.Proto))
				}
			}
		}
	}
	return n
}

type selectedStream struct{ key, sensitivity string }

// selectedStreams lists a peer's catalog streams a selection captures.
func selectedStreams(sel CaptureSelection, peer string, points []CapturePoint) []selectedStream {
	out := []selectedStream{}
	for _, p := range points {
		for _, selected := range sel.CapturePoints {
			if p.Pipeline == selected.Pipeline && p.Point == selected.Point && slices.Contains(sel.Signals, p.Signal) {
				out = append(out, selectedStream{streamKey(CaptureStream{CollectorID: peer, Pipeline: p.Pipeline, Point: p.Point, Signal: p.Signal}), p.Sensitivity})
			}
		}
	}
	return out
}

// initializeDataLocked classifies each target's selected streams from its
// catalog. A target whose catalog the debugger did not read is treated as
// raw.
func (r *collectorRegistry) initializeDataLocked(c *captureRecord, snap *peerSnapshot) {
	c.sensitivities = map[string]string{}
	c.records = []capturedRecord{}
	c.view.Data = CaptureDataStatus{Enabled: r.dataEnabled, State: "unavailable", MemoryOnly: true}
	if !r.dataEnabled {
		return
	}
	c.view.Data.State = "open"
	c.view.Data.ByteLimit = captureSessionBytes
	c.view.Data.RecordLimit = captureRecordsLimit
	c.view.Data.MemoryOnly = r.repository == nil
	for _, t := range c.view.Targets {
		peer, known := snap.collectors[t.CollectorID]
		if !known || peer.Generation != t.Generation {
			for _, p := range c.view.Selection.CapturePoints {
				for _, signal := range c.view.Selection.Signals {
					c.sensitivities[streamKey(CaptureStream{CollectorID: t.CollectorID, Pipeline: p.Pipeline, Point: p.Point, Signal: signal})] = "raw"
				}
			}
			c.view.Data.ContainsRaw = true
			continue
		}
		for _, s := range selectedStreams(c.view.Selection, t.CollectorID, snap.points[t.CollectorID]) {
			c.sensitivities[s.key] = s.sensitivity
			if s.sensitivity == "raw" {
				c.view.Data.ContainsRaw = true
			}
		}
	}
}

func (r *collectorRegistry) checkpointCaptureLocked(ctx context.Context, c *captureRecord, segment []byte) error {
	if !c.view.Data.Enabled || r.repository == nil {
		return nil
	}
	raw, _ := json.Marshal(c.view)
	stored, err := r.repository.SaveCapture(
		ctx,
		c.view.ID,
		c.view.Project,
		c.storeGeneration,
		c.view.Data.State != "open" && c.view.Data.State != "draining",
		c.view.Data.MemoryOnly,
		raw,
		segment,
	)
	if err != nil {
		c.view.Data.Error = "storage_error"
		return artifactError(err)
	}
	c.storeGeneration = stored.Generation
	return nil
}

// offloadLocked releases a frozen capture's records from memory once the
// workspace holds them; every record in memory was stored before it was
// accepted. Later reads load them with recordsLocked.
func (r *collectorRegistry) offloadLocked(c *captureRecord) {
	if c.onDisk || r.repository == nil || c.view.Data.MemoryOnly || c.storeGeneration == 0 || c.view.Data.State != "frozen" {
		return
	}
	for _, rec := range c.records {
		r.memoryBytes -= int64(len(rec.Proto))
	}
	c.records, c.onDisk = nil, true
}

// recordsLocked returns a capture's records, reading an offloaded capture
// from the workspace. The last capture read stays cached, so paging does not
// read the workspace again.
func (r *collectorRegistry) recordsLocked(ctx context.Context, c *captureRecord) ([]capturedRecord, error) {
	if !c.onDisk {
		return c.records, nil
	}
	if r.loaded.id == c.view.ID {
		return r.loaded.records, nil
	}
	segments, err := r.repository.CaptureSegments(ctx, c.view.ID)
	if err != nil {
		return nil, artifactError(err)
	}
	records := []capturedRecord{}
	var bytes int64
	for _, raw := range segments {
		var batch []capturedRecord
		if decodeStored(raw, &batch) != nil {
			return nil, artifactError(artifact.ErrCorrupt)
		}
		for _, rec := range batch {
			if rec.Sequence != len(records)+1 || len(rec.Proto) > 64<<10 || len(records) >= captureRecordsLimit {
				return nil, artifactError(artifact.ErrCorrupt)
			}
			records = append(records, rec)
			bytes += int64(len(rec.Proto))
		}
	}
	if int64(len(records)) != c.view.Data.ReceivedRecords || bytes != c.view.Data.ReceivedBytes {
		return nil, artifactError(artifact.ErrCorrupt)
	}
	r.loaded.id, r.loaded.records = c.view.ID, records
	return records, nil
}

// ingest stores a delivered batch within the capture's budgets and returns
// the records it accepted, which the runner acknowledges. A redelivered
// batch is acknowledged with its first count and not stored again.
func (r *collectorRegistry) ingest(ctx context.Context, c *captureRecord, e *fleetcapture.Envelope) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if accepted, seen := c.batches[e.BatchID]; seen {
		return accepted
	}
	accepted, rejected := r.ingestLocked(ctx, c, e)
	if rejected > 0 {
		c.view.Data.RejectedRecords += rejected
		if accepted == 0 && r.checkpointCaptureLocked(ctx, c, nil) != nil {
			r.requestStopLocked(c, "capture_storage_error")
		}
	}
	if collectorText(e.BatchID, 128, true) {
		c.batches[e.BatchID] = accepted
		c.batchOrder = append(c.batchOrder, e.BatchID)
		if len(c.batchOrder) > batchHistory {
			delete(c.batches, c.batchOrder[0])
			c.batchOrder = c.batchOrder[1:]
		}
	}
	c.notify()
	return accepted
}

func (r *collectorRegistry) ingestLocked(ctx context.Context, c *captureRecord, e *fleetcapture.Envelope) (int64, int64) {
	refused := max(min(e.Records, 1<<20), 1)
	known := false
	for _, t := range c.view.Targets {
		known = known || t.CollectorID == e.PeerID && t.Generation == e.Generation
	}
	if !c.view.Data.Enabled || c.view.Data.State == "frozen" || c.view.Data.Error != "" || !known || c.refused[e.PeerID] ||
		e.CaptureID != c.fleetID || e.Encoding != "otlp-protobuf" || !slices.Contains(c.view.Selection.Signals, e.Signal) || len(e.Data) > 1<<20 {
		return 0, refused
	}
	p, err := telemetry.DecodeProto(e.Signal, e.Data)
	if err != nil {
		return 0, refused
	}
	if counts := p.Counts(); counts.Items > 8192 || counts.Resources > 8192 || counts.Scopes > 8192 {
		return 0, int64(counts.Items)
	}
	var rejected, workBytes, pendingBytes int64
	items := []capturedRecord{}
	keys := telemetry.CaptureTagKeys()
	type tags struct {
		pipeline, point string
		valid           bool
	}
	cache := map[int]tags{}
	for record := range telemetry.CaptureRecords(p) {
		t, cached := cache[record.Resource]
		if !cached {
			resource, _ := p.ResourceAt(record.Resource)
			point, okPoint := telemetry.CaptureTag(resource.Attributes(), keys[1])
			pipeline, okPipeline := telemetry.CaptureTag(resource.Attributes(), keys[2])
			t = tags{pipeline, point, okPoint && okPipeline && len(point) <= 128 && len(pipeline) <= 256}
			cache[record.Resource] = t
			// The routing tags are the Collector's, never the source's: stored
			// records carry the resource as the capture point saw it.
			for _, key := range keys {
				resource.Attributes().Remove(key)
			}
		}
		stream := CaptureStream{CollectorID: e.PeerID, Pipeline: t.pipeline, Point: t.point, Signal: e.Signal}
		sensitivity := c.sensitivities[streamKey(stream)]
		if !t.valid || sensitivity == "" {
			rejected++
			continue
		}
		// Bound expansion of shared resource/scope data even for rejected items.
		if workBytes >= captureSessionBytes {
			rejected++
			r.requestStopLocked(c, "capture_processing_limit")
			continue
		}
		one := telemetry.RecordPayload(p, record)
		raw, err := one.Proto()
		if err != nil {
			rejected++
			continue
		}
		workBytes += int64(len(raw))
		if len(raw) > 64<<10 {
			rejected++
			continue
		}
		bytes := int64(len(raw))
		if c.view.Data.ReceivedBytes+pendingBytes+bytes > captureSessionBytes ||
			len(c.records)+len(items) >= captureRecordsLimit ||
			r.memoryBytes+pendingBytes+bytes > captureMemoryBytes {
			rejected++
			r.requestStopLocked(c, "capture_storage_limit")
			continue
		}
		pendingBytes += bytes
		items = append(items, capturedRecord{Stream: stream, ReceivedAt: r.now().UTC(), Sensitivity: sensitivity, Proto: raw})
	}
	if len(items) == 0 {
		return 0, rejected
	}
	for i := range items {
		items[i].Sequence = len(c.records) + i + 1
	}
	old := c.view.Data
	c.view.Data.ReceivedRecords += int64(len(items))
	c.view.Data.ReceivedBytes += pendingBytes
	c.view.Data.RejectedRecords += rejected
	var segment []byte
	if !c.view.Data.MemoryOnly {
		segment, _ = json.Marshal(items)
	}
	if err := r.checkpointCaptureLocked(ctx, c, segment); err != nil {
		c.view.Data = old
		c.view.Data.Error = "storage_error"
		r.requestStopLocked(c, "capture_storage_error")
		return 0, rejected + int64(len(items))
	}
	c.records = append(c.records, items...)
	r.memoryBytes += pendingBytes
	return int64(len(items)), 0
}

// recoverCaptures reloads retained captures. The debugger does not resume a
// fleet capture after a restart: a session that was still running is marked
// uncertain, and fleet ends it at its expiry.
func (s *Service) recoverCaptures() error {
	r := s.collectors
	if s.repository == nil {
		return nil
	}
	if r.dataEnabled {
		if err := s.repository.EnableCaptures(context.Background()); err != nil {
			return err
		}
	}
	stored, err := s.repository.Captures(context.Background())
	if err != nil {
		return err
	}
	for _, v := range stored {
		var view CaptureSession
		if decodeStored(v.Metadata, &view) != nil || view.ID != v.ID || view.CreatedAt.IsZero() || len(view.Targets) > 16 {
			return artifact.ErrCorrupt
		}
		// The store's project is authoritative; older records recover into default.
		view.Project = v.Project
		if view.Metadata.Revision == 0 {
			view.Metadata = CaptureMetadata{Revision: 1, UpdatedAt: view.CreatedAt}
		}
		if !validCaptureMetadata(view.Metadata) || view.Metadata.UpdatedAt.Before(view.CreatedAt) {
			return artifact.ErrCorrupt
		}
		c := &captureRecord{
			view:            view,
			changed:         make(chan struct{}),
			sensitivities:   map[string]string{},
			refused:         map[string]bool{},
			batches:         map[string]int64{},
			wake:            make(chan struct{}, 1),
			storeGeneration: v.Generation,
		}
		r.lostLocked(c, "platform_restarted; capture_not_resumed")
		c.view.Data.State = "frozen"
		// Records stay in the workspace until read; recordsLocked checks them.
		if v.MemoryOnly {
			c.view.Data.State = "lost_on_restart"
		} else if view.Data.ReceivedRecords < 0 || view.Data.ReceivedRecords > captureRecordsLimit ||
			view.Data.ReceivedBytes < 0 || view.Data.ReceivedBytes > captureSessionBytes {
			return artifact.ErrCorrupt
		} else {
			c.onDisk = true
		}
		r.sessions[view.ID] = c
		r.sessionGeneration++
		if err := r.checkpointCaptureLocked(context.Background(), c, nil); err != nil {
			return err
		}
	}
	return nil
}

type captureCursor struct {
	Epoch, Session, Filter   string
	Watermark, Offset, Limit int
}

// GetCaptureRecords returns a page of a capture's records.
func (s *Service) GetCaptureRecords(ctx context.Context, caller Caller, id string, req CaptureRecordsRequest) (_ *CaptureRecordsPage, err error) {
	defer s.audit(&caller, "GetCaptureRecords", &err)()
	if err := s.begin(ctx, caller, authz.CaptureRead); err != nil {
		return nil, err
	}
	if err := s.captureDataIn(caller, id); err != nil {
		return nil, err
	}
	if !collectorText(req.Stream.CollectorID, 128, false) || !collectorText(req.Stream.Pipeline, 256, false) ||
		!collectorText(req.Stream.Point, 128, false) ||
		(req.Stream.Signal != "" && !slices.Contains([]string{"traces", "logs", "metrics"}, req.Stream.Signal)) {
		return nil, captureInvalid("invalid source stream filter")
	}
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return nil, err
	}
	c := r.sessions[id]
	if c == nil {
		return nil, &Error{Code: CodeNotFound, Message: "capture not found or expired"}
	}
	if !c.view.Data.Enabled {
		return nil, &Error{Code: CodeUnavailable, Message: "capture ingress was not enabled"}
	}
	limit := req.Limit
	if limit == 0 {
		limit = min(25, s.budgets.PageItems)
	}
	if limit < 1 || limit > min(100, s.budgets.PageItems) {
		return nil, captureInvalid("invalid capture record page limit")
	}
	records, err := r.recordsLocked(ctx, c)
	if err != nil {
		return nil, err
	}
	filter, _ := json.Marshal(req.Stream)
	sum := sha256.Sum256(filter)
	cur := captureCursor{
		Epoch:     r.epoch,
		Session:   id,
		Filter:    hex.EncodeToString(sum[:]),
		Watermark: len(records),
		Limit:     limit,
	}
	if req.Cursor != "" {
		if len(req.Cursor) > 2048 {
			return nil, captureInvalid("invalid capture cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(req.Cursor)
		var prev captureCursor
		if len(req.Cursor) > 2048 || err != nil || json.Unmarshal(raw, &prev) != nil || prev.Epoch != cur.Epoch ||
			prev.Session != id ||
			prev.Filter != cur.Filter ||
			prev.Limit != limit ||
			prev.Offset < 0 ||
			prev.Offset > prev.Watermark ||
			prev.Watermark > len(records) {
			return nil, captureInvalid("invalid capture cursor")
		}
		cur = prev
	}
	page := &CaptureRecordsPage{SessionID: id, Watermark: cur.Watermark, Items: []CaptureRecord{}}
	used := 512
	for cur.Offset < cur.Watermark && len(page.Items) < limit {
		rec := records[cur.Offset]
		if !matchesStream(req.Stream, rec.Stream) {
			cur.Offset++
			continue
		}
		p, err := telemetry.DecodeProto(rec.Stream.Signal, rec.Proto)
		if err != nil {
			return nil, &Error{Code: CodeStorageError, Message: "capture record is unavailable"}
		}
		payload, err := p.JSON()
		if err != nil {
			return nil, &Error{Code: CodeStorageError, Message: "capture record cannot be encoded"}
		}
		item := CaptureRecord{
			Sequence:    rec.Sequence,
			Stream:      rec.Stream,
			ReceivedAt:  rec.ReceivedAt,
			Sensitivity: rec.Sensitivity,
			Payload:     payload,
		}
		raw, _ := json.Marshal(item)
		if len(raw)+512 > s.budgets.PageBytes {
			item.Payload = nil
			item.Oversize = true
			raw, _ = json.Marshal(item)
		}
		if used+len(raw) > s.budgets.PageBytes {
			if len(page.Items) == 0 {
				return nil, &Error{Code: CodeResourceExhausted, Message: "capture record metadata exceeds page budget"}
			}
			break
		}
		page.Items = append(page.Items, item)
		used += len(raw) + 1
		cur.Offset++
	}
	if cur.Offset < cur.Watermark {
		raw, _ := json.Marshal(cur)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

// DeleteCaptureSession deletes a terminal capture session and its records.
func (s *Service) DeleteCaptureSession(ctx context.Context, caller Caller, id string) (err error) {
	defer s.audit(&caller, "DeleteCaptureSession", &err)()
	if err := s.begin(ctx, caller, authz.CaptureStart); err != nil {
		return err
	}
	if err := s.captureIn(caller, authz.CaptureStart, id); err != nil {
		return err
	}
	r := s.collectors
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.check(); err != nil {
		return err
	}
	c := r.sessions[id]
	if c == nil {
		return &Error{Code: CodeNotFound, Message: "capture not found or expired"}
	}
	if !c.view.Terminal || c.running || c.view.Data.Enabled && (c.view.Data.State == "open" || c.view.Data.State == "draining") {
		return &Error{Code: CodeConflict, Message: "stop the capture and wait for its delivery to drain before deletion"}
	}
	if r.repository != nil && c.storeGeneration > 0 {
		if err := r.repository.DeleteCapture(ctx, id); err != nil {
			return artifactError(err)
		}
	}
	for _, rec := range c.records {
		r.memoryBytes -= int64(len(rec.Proto))
	}
	if r.loaded.id == id {
		r.loaded.id, r.loaded.records = "", nil
	}
	delete(r.sessions, id)
	r.sessionGeneration++
	return nil
}
