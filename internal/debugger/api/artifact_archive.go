package api

import (
	"mime"
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// ExportForCIRequest names the saved configuration and scenarios to export
// for CI, each at its head revision.
type ExportForCIRequest struct {
	ConfigurationID string   `json:"configuration_id"`
	ScenarioIDs     []string `json:"scenario_ids"`
}

// ImportRequest carries a CI archive, base64 like artifact content.
type ImportRequest struct {
	Archive []byte `json:"archive_base64"`
}

// maxImportRequestBytes fits the base64 of the largest accepted archive.
const maxImportRequestBytes = platform.MaxCIArchiveBytes/3*4 + 64<<10

func (s *Server) listArtifactRevisions(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		if limit, err = strconv.Atoi(value); err != nil {
			writeArtifactFailure(w, http.StatusBadRequest, "invalid_request", invalidArtifactRequest)
			return
		}
	}
	page, err := s.runs.ListArtifactRevisions(r.Context(), callerOf(r), platform.ListArtifactRevisionsRequest{ArtifactID: r.PathValue("artifact"), Limit: limit, Cursor: r.URL.Query().Get("cursor")})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// exportArtifacts answers with the CI archive as a zip attachment.
func (s *Server) exportArtifacts(w http.ResponseWriter, r *http.Request) {
	var req ExportForCIRequest
	if !decodeArtifact(w, r, &req, maxRequestBytes) {
		return
	}
	exp, err := s.runs.ExportArtifactsForCI(r.Context(), callerOf(r), platform.ExportCIRequest{Configuration: req.ConfigurationID, Scenarios: req.ScenarioIDs})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": exp.Name}))
	w.Header().Set("Content-Length", strconv.Itoa(len(exp.Archive)))
	_, _ = w.Write(exp.Archive)
}

func (s *Server) importArtifacts(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if !decodeArtifact(w, r, &req, maxImportRequestBytes) {
		return
	}
	imp, err := s.runs.ImportArtifacts(r.Context(), callerOf(r), req.Archive)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, imp)
}
