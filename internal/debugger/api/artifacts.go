package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Messages of failures detected by this adapter, in the artifact vocabulary.
const (
	invalidArtifactRequest = "invalid artifact request"
	artifactLimitExceeded  = "workspace storage limit exceeded"
)

// ArtifactError is confined to the new artifact endpoints. Existing v1 errors
// retain their envelope. Conflicts provide current metadata for a deliberate edit.
type ArtifactError struct {
	Code    string                     `json:"code"`
	Error   string                     `json:"error"`
	Current *platform.ArtifactMetadata `json:"current,omitempty"`
}

// SaveRequest saves original source bytes as a new revision. A new artifact
// omits artifact_id; an edit names the generation and head it expects.
type SaveRequest struct {
	WorkspaceID        string                `json:"workspace_id"`
	ArtifactID         string                `json:"artifact_id,omitempty"`
	ExpectedGeneration uint64                `json:"expected_generation"`
	ExpectedHead       string                `json:"expected_head_revision_id,omitempty"`
	Kind               platform.ArtifactKind `json:"kind"`
	Label              string                `json:"label"`
	Content            []byte                `json:"content_base64"`
}

func (r SaveRequest) save() platform.SaveArtifactRequest {
	return platform.SaveArtifactRequest{WorkspaceID: r.WorkspaceID, ArtifactID: r.ArtifactID, ExpectedGeneration: r.ExpectedGeneration,
		ExpectedHead: r.ExpectedHead, Kind: r.Kind, Label: r.Label, Content: r.Content}
}

// writeArtifactError maps platform codes to the artifact endpoints' statuses
// and body codes. Storage messages are already safe to show.
func writeArtifactError(w http.ResponseWriter, err error) {
	var e *platform.Error
	if !errors.As(err, &e) {
		e = &platform.Error{Code: platform.CodeStorageError, Message: err.Error()}
	}
	status, body := http.StatusInternalServerError, ArtifactError{Code: "storage_error", Error: e.Message}
	switch e.Code {
	case platform.CodeInvalidRequest:
		status, body.Code = http.StatusBadRequest, "invalid_request"
	case platform.CodeNotFound:
		status, body.Code = http.StatusNotFound, "not_found"
	case platform.CodeConflict:
		status, body.Code, body.Current = http.StatusConflict, "conflict", e.Current
	case platform.CodeResourceExhausted:
		status, body.Code = http.StatusRequestEntityTooLarge, "resource_exhausted"
	case platform.CodeUnavailable:
		status, body.Code, body.Error = http.StatusServiceUnavailable, "unavailable", "workspace admission is closed"
		if errors.Is(err, platform.ErrWorkspaceRequired) {
			body.Code, body.Error = "workspace_required", e.Message
		}
	}
	writeJSON(w, status, body)
}

func writeArtifactFailure(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ArtifactError{Code: code, Error: message})
}

// decodeArtifact decodes one strict, bounded request document for the
// artifact endpoints.
func decodeArtifact(w http.ResponseWriter, r *http.Request, v any, size int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, size))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			writeArtifactFailure(w, http.StatusRequestEntityTooLarge, "resource_exhausted", artifactLimitExceeded)
			return false
		}
		writeArtifactFailure(w, http.StatusBadRequest, "invalid_request", invalidArtifactRequest+": "+err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeArtifactFailure(w, http.StatusBadRequest, "invalid_request", invalidArtifactRequest+": expected one request document")
		return false
	}
	return true
}

func (s *Server) saveArtifact(w http.ResponseWriter, r *http.Request) {
	var req SaveRequest
	if !decodeArtifact(w, r, &req, maxRequestBytes) {
		return
	}
	saved, err := s.runs.SaveArtifact(r.Context(), callerOf(r), req.save())
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/artifacts/"+saved.Artifact.ID+"/revisions/"+saved.Revision.Ref.RevisionID)
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil {
			writeArtifactFailure(w, http.StatusBadRequest, "invalid_request", invalidArtifactRequest)
			return
		}
	}
	query := r.URL.Query()
	page, err := s.runs.ListArtifacts(r.Context(), callerOf(r), platform.ListArtifactsRequest{Kind: platform.ArtifactKind(query.Get("kind")), Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	metadata, err := s.runs.GetArtifact(r.Context(), callerOf(r), r.PathValue("artifact"))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, metadata)
}

func (s *Server) getArtifactRevision(w http.ResponseWriter, r *http.Request) {
	revision, err := s.runs.GetArtifactRevision(r.Context(), callerOf(r), r.PathValue("artifact"), r.PathValue("revision"))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revision)
}
