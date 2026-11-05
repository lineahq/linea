package memory

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

const (
	maxCaptureMetadata = 256 << 10
	maxCaptureSegments = 10000
)

type captureCatalog struct {
	Schema    int                               `json:"schema"`
	Workspace string                            `json:"workspace"`
	Records   map[string]artifact.CaptureRecord `json:"records"`
}

// captureReserved is the allowance open, non-memory-only captures hold.
func captureReserved(c *captureCatalog) int64 {
	var n int64
	if c != nil {
		for _, r := range c.Records {
			if !r.Closed && !r.MemoryOnly {
				n += artifact.CaptureAllowance - r.Bytes
			}
		}
	}
	return n
}

// encodeSegment compresses a segment when that makes it smaller; the digest
// and byte count describe the stored representation.
func encodeSegment(raw []byte) (artifact.CaptureSegment, []byte, error) {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := w.Write(raw); err != nil {
		return artifact.CaptureSegment{}, nil, fmt.Errorf("%w: compress capture: %w", artifact.ErrStorage, err)
	}
	if err := w.Close(); err != nil {
		return artifact.CaptureSegment{}, nil, fmt.Errorf("%w: compress capture: %w", artifact.ErrStorage, err)
	}
	stored := raw
	seg := artifact.CaptureSegment{}
	if buf.Len() < len(raw) {
		stored = buf.Bytes()
		seg.Encoding, seg.UncompressedBytes = "gzip", int64(len(raw))
	}
	seg.Digest, seg.Bytes = digest(stored), int64(len(stored))
	return seg, stored, nil
}

func (d *Data) readSegment(seg artifact.CaptureSegment) ([]byte, error) {
	raw, ok := d.blobs[seg.Digest]
	if !ok {
		return nil, artifact.ErrCorrupt
	}
	if seg.Encoding == "" {
		return cloneBytes(raw), nil
	}
	r, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, artifact.ErrCorrupt
	}
	decoded, err := io.ReadAll(r)
	if err != nil {
		return nil, artifact.ErrCorrupt
	}
	return decoded, nil
}

// EnableCaptures creates the capture journal; it is idempotent.
func (s *Store) EnableCaptures(ctx context.Context) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.captures != nil {
		d.sweep()
		return nil
	}
	c := captureCatalog{Schema: captureSchema, Workspace: d.cat.Workspace.ID, Records: map[string]artifact.CaptureRecord{}}
	size := encodedSize(c)
	next := d.cat
	next.CaptureSchema = captureSchema
	catSize := encodedSize(next)
	used, err := s.usage()
	if err != nil {
		return err
	}
	if catSize > s.limits.CatalogBytes || s.checkSpace(used, catSize+size, s.reserved(d.runs)) != nil {
		return artifact.ErrLimit
	}
	d.captures, d.capSize = &c, size
	d.cat, d.catSize = next, catSize
	d.sweep()
	return nil
}

// SaveCapture replaces a capture's metadata and appends segment, if any, when
// its generation is still expected. A memory-only capture keeps its metadata
// only; a closed one releases its reservation.
func (s *Store) SaveCapture(ctx context.Context, id, project string, expected uint64, closed, memoryOnly bool, metadata, segment []byte) (*artifact.CaptureRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.captures == nil || !strings.HasPrefix(id, "capture-") || len(id) > 128 || !artifact.ValidProject(project) ||
		len(metadata) > maxCaptureMetadata || !json.Valid(metadata) || memoryOnly && len(segment) > 0 {
		return nil, artifact.ErrInvalidRequest
	}
	old, ok := d.captures.Records[id]
	if ok && (old.Generation != expected || old.MemoryOnly != memoryOnly) || !ok && expected != 0 {
		return nil, artifact.ErrConflict
	}
	if ok && old.Project != project {
		return nil, artifact.ErrInvalidRequest
	}
	if !ok && len(d.captures.Records) >= artifact.CaptureHistory || old.Bytes+int64(len(segment)) > artifact.CaptureAllowance ||
		len(segment) > 0 && len(old.Segments) >= maxCaptureSegments {
		return nil, artifact.ErrLimit
	}
	r := artifact.CaptureRecord{
		ID:         id,
		Project:    project,
		Generation: expected + 1,
		Closed:     closed,
		MemoryOnly: memoryOnly,
		Metadata:   append(json.RawMessage(nil), metadata...),
		Segments:   append([]artifact.CaptureSegment{}, old.Segments...),
		Bytes:      old.Bytes,
	}
	var stored []byte
	if len(segment) > 0 {
		seg, encoded, err := encodeSegment(segment)
		if err != nil {
			return nil, err
		}
		stored = encoded
		r.Segments = append(r.Segments, seg)
		r.Bytes += int64(len(segment))
	}
	next := *d.captures
	next.Records = make(map[string]artifact.CaptureRecord, len(d.captures.Records)+1)
	for key, value := range d.captures.Records {
		next.Records[key] = value
	}
	next.Records[id] = r
	size := encodedSize(next)
	// Reserve catalog space for growth of active status observations too.
	future := size
	for _, record := range next.Records {
		if !record.Closed {
			future += maxCaptureMetadata - int64(len(record.Metadata))
		}
	}
	if size > s.limits.CatalogBytes || future > s.limits.CatalogBytes {
		return nil, artifact.ErrLimit
	}
	used, err := s.usage()
	if err != nil {
		return nil, err
	}
	if err := s.checkSpace(used, size+int64(len(stored)), s.runReserved(d.runs)+captureReserved(&next)); err != nil {
		return nil, err
	}
	if len(segment) > 0 {
		d.storeBlob(r.Segments[len(r.Segments)-1].Digest, stored)
	}
	d.captures, d.capSize = &next, size
	out := r
	out.Segments = append([]artifact.CaptureSegment{}, r.Segments...)
	return &out, nil
}

// Captures returns every capture in the journal.
func (s *Store) Captures(ctx context.Context) ([]artifact.CaptureRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	out := []artifact.CaptureRecord{}
	if d.captures != nil {
		for _, r := range d.captures.Records {
			r.Metadata = append(json.RawMessage(nil), r.Metadata...)
			r.Segments = append([]artifact.CaptureSegment{}, r.Segments...)
			out = append(out, r)
		}
	}
	return out, nil
}

// CaptureSegments returns a capture's segments decoded, in order.
func (s *Store) CaptureSegments(ctx context.Context, id string) ([][]byte, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	if d.captures == nil {
		return nil, artifact.ErrNotFound
	}
	r, ok := d.captures.Records[id]
	if !ok {
		return nil, artifact.ErrNotFound
	}
	out := [][]byte{}
	for _, seg := range r.Segments {
		raw, err := d.readSegment(seg)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// DeleteCapture removes a capture and its segments.
func (s *Store) DeleteCapture(ctx context.Context, id string) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	if d.captures == nil {
		return artifact.ErrNotFound
	}
	if _, ok := d.captures.Records[id]; !ok {
		return artifact.ErrNotFound
	}
	next := *d.captures
	next.Records = map[string]artifact.CaptureRecord{}
	for key, r := range d.captures.Records {
		if key != id {
			next.Records[key] = r
		}
	}
	d.captures, d.capSize = &next, encodedSize(next)
	d.sweep()
	return nil
}
