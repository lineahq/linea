package platform

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/authz"
)

// CaptureMetadata is owner-authored organization, separate from immutable evidence.
type CaptureMetadata struct {
	Revision    uint64    `json:"revision"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CaptureMetadataUpdate renames or describes a capture at its revision.
type CaptureMetadataUpdate struct {
	Revision    uint64 `json:"revision"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func descriptionText(v string) bool {
	return len(v) <= 2048 && utf8.ValidString(v) &&
		!strings.ContainsFunc(
			v,
			func(c rune) bool { return unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t' },
		)
}

func validCaptureMetadata(v CaptureMetadata) bool {
	return v.Revision > 0 && !v.UpdatedAt.IsZero() && collectorText(v.Name, 128, false) &&
		descriptionText(v.Description)
}

// UpdateCaptureMetadata replaces a capture's name and description.
func (s *Service) UpdateCaptureMetadata(ctx context.Context, caller Caller, id string, q CaptureMetadataUpdate) (_ *CaptureSession, err error) {
	defer s.audit(&caller, "UpdateCaptureMetadata", &err)()
	if err := s.begin(ctx, caller, authz.CaptureStart); err != nil {
		return nil, err
	}
	if err := s.captureIn(caller, authz.CaptureStart, id); err != nil {
		return nil, err
	}
	q.Name = strings.TrimSpace(q.Name)
	q.Description = strings.TrimSpace(q.Description)
	if q.Revision == 0 || !collectorText(q.Name, 128, false) || !descriptionText(q.Description) {
		return nil, captureInvalid(
			"capture details need a revision, name up to 128 bytes and description up to 2048 bytes",
		)
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
	if c.view.Metadata.Revision != q.Revision {
		return nil, &Error{Code: CodeConflict, Message: "capture details changed; reload before saving"}
	}
	next := cloneCapture(c.view)
	next.Metadata = CaptureMetadata{
		Revision:    q.Revision + 1,
		Name:        q.Name,
		Description: q.Description,
		UpdatedAt:   r.now().UTC(),
	}
	// Save a metadata-only checkpoint. Never append, rewrite or materialize payloads.
	if r.repository != nil && c.storeGeneration > 0 {
		raw, _ := json.Marshal(next)
		stored, err := r.repository.SaveCapture(
			ctx,
			id,
			c.view.Project,
			c.storeGeneration,
			next.Data.State != "open" && next.Data.State != "draining",
			next.Data.MemoryOnly,
			raw,
			nil,
		)
		if err != nil {
			return nil, artifactError(err)
		}
		c.storeGeneration = stored.Generation
	}
	c.view = next
	r.sessionGeneration++
	c.notify()
	v := cloneCapture(next)
	return &v, nil
}
