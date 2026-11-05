package pg

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

const (
	maxCaptureMetadata = 256 << 10
	maxCaptureSegments = 10000
)

// EnableCaptures is a no-op: the capture journal's tables belong to the
// schema.
func (s *Store) EnableCaptures(ctx context.Context) error { return s.ready(ctx) }

// SaveCapture replaces a capture's metadata and appends segment, if any, when
// its generation is still expected. A memory-only capture keeps its metadata
// only; a closed one releases its storage reservation.
func (s *Store) SaveCapture(ctx context.Context, id, project string, expected uint64, closed, memoryOnly bool, metadata, segment []byte) (*artifact.CaptureRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(id, "capture-") || len(id) > 128 || !artifact.ValidProject(project) || len(metadata) > maxCaptureMetadata ||
		!json.Valid(metadata) || memoryOnly && len(segment) > 0 {
		return nil, artifact.ErrInvalidRequest
	}
	var rec *artifact.CaptureRecord
	err := s.inTx(ctx, "capture", func(tx pgx.Tx) error {
		// Creating a capture and appending a segment check counts and storage
		// under the installation lock; a metadata update touches its row only.
		if expected == 0 || len(segment) > 0 {
			if _, err := lockInstallation(ctx, tx); err != nil {
				return err
			}
		}
		var old artifact.CaptureRecord
		var oldProject string
		var oldGeneration int64
		exists := true
		err := tx.QueryRow(ctx, "SELECT project_id, generation, closed, memory_only, bytes FROM captures WHERE id = $1 FOR UPDATE", id).
			Scan(&oldProject, &oldGeneration, &old.Closed, &old.MemoryOnly, &old.Bytes)
		if errors.Is(err, pgx.ErrNoRows) {
			exists = false
		} else if err != nil {
			return err
		}
		if exists && (uint64(oldGeneration) != expected || old.MemoryOnly != memoryOnly) || !exists && expected != 0 {
			return artifact.ErrConflict
		}
		if exists && oldProject != project {
			return artifact.ErrInvalidRequest
		}
		var segments int64
		if exists {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM capture_segments WHERE capture_id = $1", id).Scan(&segments); err != nil {
				return err
			}
		} else {
			var count int64
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM captures").Scan(&count); err != nil {
				return err
			}
			if count >= artifact.CaptureHistory {
				return artifact.ErrLimit
			}
		}
		if old.Bytes+int64(len(segment)) > artifact.CaptureAllowance || len(segment) > 0 && segments >= maxCaptureSegments {
			return artifact.ErrLimit
		}
		r := artifact.CaptureRecord{ID: id, Project: project, Generation: expected + 1, Closed: closed, MemoryOnly: memoryOnly,
			Metadata: append(json.RawMessage(nil), metadata...), Segments: []artifact.CaptureSegment{}, Bytes: old.Bytes}
		var stored []byte
		var seg artifact.CaptureSegment
		if len(segment) > 0 {
			var err error
			if seg, stored, err = encodeCaptureSegment(segment); err != nil {
				return err
			}
			r.Bytes += int64(len(segment))
		}
		if len(segment) > 0 || expected == 0 {
			additional, err := newBytes(ctx, tx, blob{seg.Digest, seg.Bytes})
			if err != nil {
				return err
			}
			var extra int64
			if !closed && !memoryOnly {
				extra = artifact.CaptureAllowance - r.Bytes
			}
			if err := s.checkSpace(ctx, tx, additional, reserve{excludeCapture: id, extraCapture: extra}); err != nil {
				return err
			}
		}
		if len(segment) > 0 {
			if err := s.putBlob(ctx, seg.Digest, stored); err != nil {
				return err
			}
		}
		if exists {
			_, err = tx.Exec(ctx, "UPDATE captures SET generation = $2, closed = $3, memory_only = $4, metadata = $5, bytes = $6, updated_at = now() WHERE id = $1",
				id, int64(r.Generation), closed, memoryOnly, []byte(r.Metadata), r.Bytes)
		} else {
			_, err = tx.Exec(ctx, "INSERT INTO captures (id, project_id, generation, closed, memory_only, metadata, bytes) VALUES ($1, $2, $3, $4, $5, $6, $7)",
				id, project, int64(r.Generation), closed, memoryOnly, []byte(r.Metadata), r.Bytes)
		}
		if err != nil {
			return err
		}
		if len(segment) > 0 {
			if _, err := tx.Exec(ctx, "INSERT INTO capture_segments (capture_id, seq, digest, bytes, encoding, uncompressed_bytes) VALUES ($1, $2, $3, $4, $5, $6)",
				id, int32(segments), seg.Digest, seg.Bytes, seg.Encoding, seg.UncompressedBytes); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, "SELECT digest, bytes, encoding, uncompressed_bytes FROM capture_segments WHERE capture_id = $1 ORDER BY seq", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c artifact.CaptureSegment
			if err := rows.Scan(&c.Digest, &c.Bytes, &c.Encoding, &c.UncompressedBytes); err != nil {
				return err
			}
			r.Segments = append(r.Segments, c)
		}
		rec = &r
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return rec, nil
}

// Captures returns every capture with its segment references.
func (s *Store) Captures(ctx context.Context) ([]artifact.CaptureRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT id, project_id, generation, closed, memory_only, metadata, bytes FROM captures ORDER BY created_at, id")
	if err != nil {
		return nil, storageErr("captures", err)
	}
	defer rows.Close()
	out := []artifact.CaptureRecord{}
	index := map[string]int{}
	for rows.Next() {
		var r artifact.CaptureRecord
		var generation int64
		var metadata []byte
		if err := rows.Scan(&r.ID, &r.Project, &generation, &r.Closed, &r.MemoryOnly, &metadata, &r.Bytes); err != nil {
			return nil, storageErr("captures", err)
		}
		r.Generation, r.Metadata, r.Segments = uint64(generation), metadata, []artifact.CaptureSegment{}
		index[r.ID] = len(out)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, storageErr("captures", err)
	}
	rows.Close()
	segs, err := s.pool.Query(ctx, "SELECT capture_id, digest, bytes, encoding, uncompressed_bytes FROM capture_segments ORDER BY capture_id, seq")
	if err != nil {
		return nil, storageErr("captures", err)
	}
	defer segs.Close()
	for segs.Next() {
		var id string
		var c artifact.CaptureSegment
		if err := segs.Scan(&id, &c.Digest, &c.Bytes, &c.Encoding, &c.UncompressedBytes); err != nil {
			return nil, storageErr("captures", err)
		}
		// A capture created after the first query has no entry; skip its segments.
		if i, ok := index[id]; ok {
			out[i].Segments = append(out[i].Segments, c)
		}
	}
	return out, storageErr("captures", segs.Err())
}

// CaptureSegments returns a capture's segments decoded, in order.
func (s *Store) CaptureSegments(ctx context.Context, id string) ([][]byte, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM captures WHERE id = $1)", id).Scan(&exists); err != nil {
		return nil, storageErr("capture segments", err)
	}
	if !exists {
		return nil, artifact.ErrNotFound
	}
	rows, err := s.pool.Query(ctx, "SELECT digest, bytes, encoding, uncompressed_bytes FROM capture_segments WHERE capture_id = $1 ORDER BY seq", id)
	if err != nil {
		return nil, storageErr("capture segments", err)
	}
	var segments []artifact.CaptureSegment
	for rows.Next() {
		var c artifact.CaptureSegment
		if err := rows.Scan(&c.Digest, &c.Bytes, &c.Encoding, &c.UncompressedBytes); err != nil {
			rows.Close()
			return nil, storageErr("capture segments", err)
		}
		segments = append(segments, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, storageErr("capture segments", err)
	}
	out := [][]byte{}
	for _, seg := range segments {
		raw, err := s.readCaptureSegment(ctx, seg)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// DeleteCapture removes a capture and its segment references; the objects stay
// until Sweep finds them unreferenced.
func (s *Store) DeleteCapture(ctx context.Context, id string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, "DELETE FROM captures WHERE id = $1", id)
	if err != nil {
		return storageErr("delete capture", err)
	}
	if tag.RowsAffected() == 0 {
		return artifact.ErrNotFound
	}
	return nil
}

// encodeCaptureSegment gzips a segment unless that does not make it smaller.
func encodeCaptureSegment(raw []byte) (artifact.CaptureSegment, []byte, error) {
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
	seg.Digest, seg.Bytes = digestOf(stored), int64(len(stored))
	return seg, stored, nil
}

func (s *Store) readCaptureSegment(ctx context.Context, seg artifact.CaptureSegment) ([]byte, error) {
	raw, err := s.readBlob(ctx, seg.Digest, seg.Bytes)
	if err != nil {
		return nil, err
	}
	if seg.Encoding == "" {
		return raw, nil
	}
	input := bytes.NewReader(raw)
	r, err := gzip.NewReader(input)
	if err != nil {
		return nil, artifact.ErrCorrupt
	}
	r.Multistream(false)
	decoded, err := io.ReadAll(io.LimitReader(r, seg.UncompressedBytes+1))
	closeErr := r.Close()
	if err != nil || closeErr != nil || int64(len(decoded)) != seg.UncompressedBytes || input.Len() != 0 {
		return nil, artifact.ErrCorrupt
	}
	return decoded, nil
}
