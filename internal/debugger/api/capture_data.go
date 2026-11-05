package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// CaptureDeleted answers a deleted capture session.
type CaptureDeleted struct {
	Deleted bool `json:"deleted"`
}

func (s *Server) captureRecords(w http.ResponseWriter, r *http.Request) {
	page, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	q := r.URL.Query()
	result, err := s.runs.GetCaptureRecords(r.Context(), callerOf(r), r.PathValue("id"), platform.CaptureRecordsRequest{Limit: page.Limit, Cursor: page.Cursor, Stream: platform.CaptureStream{CollectorID: q.Get("collector_id"), Pipeline: q.Get("pipeline"), Point: q.Get("point"), Signal: q.Get("signal")}})
	captureReply(w, result, err)
}

func (s *Server) captureFixture(w http.ResponseWriter, r *http.Request) {
	var req platform.CaptureDeriveRequest
	if !decodeCaptureSized(w, r, &req, 2<<20) {
		return
	}
	result, err := s.runs.CreateFixtureFromCapture(r.Context(), callerOf(r), r.PathValue("id"), req)
	captureReply(w, result, err)
}

func (s *Server) captureScenario(w http.ResponseWriter, r *http.Request) {
	var req platform.CaptureScenarioRequest
	if !decodeCaptureSized(w, r, &req, 2<<20) {
		return
	}
	result, err := s.runs.CreateScenarioFromCapture(r.Context(), callerOf(r), r.PathValue("id"), req)
	captureReply(w, result, err)
}

func (s *Server) captureExport(w http.ResponseWriter, r *http.Request) {
	var req platform.CaptureExportRequest
	if !decodeCaptureSized(w, r, &req, 2<<20) {
		return
	}
	result, err := s.runs.ExportCaptureSelection(r.Context(), callerOf(r), r.PathValue("id"), req)
	captureReply(w, result, err)
}

func (s *Server) deleteCapture(w http.ResponseWriter, r *http.Request) {
	err := s.runs.DeleteCaptureSession(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, CaptureDeleted{Deleted: err == nil}, err)
}

func captureDataPaths() map[string]any {
	response := func(schema string) map[string]any {
		return map[string]any{"200": reply("Bounded received capture evidence or immutable derived bytes; no completeness claim.", ref(schema)), "400": operationFailure("Invalid selection."), "403": operationFailure("Access denied."), "404": operationFailure("Capture not found or expired."), "409": operationFailure("Stop and wait for ingress closure."), "413": operationFailure("Record/query/materialization or storage limit."), "503": operationFailure("Workspace or capture ingress unavailable.")}
	}
	params := []any{idParam}
	for _, name := range []string{"collector_id", "pipeline", "point", "signal", "cursor"} {
		params = append(params, map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": "string"}})
	}
	params = append(params, map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}})
	return map[string]any{
		"/api/v1/captures/{id}/records":  map[string]any{"get": op("getCaptureRecords", "Read a byte/item-bounded page at a fixed watermark; oversize entries omit payload explicitly.", params, "", response("CaptureRecordsPage"))},
		"/api/v1/captures/{id}/fixtures": map[string]any{"post": op("createFixtureFromCapture", "Freeze one source stream as receiver input; optional save is a deliberate retained copy.", []any{idParam}, "CaptureDeriveRequest", response("CaptureFixture"))},
		"/api/v1/captures/{id}/scenario": map[string]any{"post": op("createScenarioFromCapture", "Freeze selected source streams in scenario YAML with explicit receivers; assertions supplied separately.", []any{idParam}, "CaptureScenarioRequest", response("CaptureScenario"))},
		"/api/v1/captures/{id}/export":   map[string]any{"post": op("exportCaptureSelection", "Export immutable OTLP JSON or scenario YAML without implicit save.", []any{idParam}, "CaptureExportRequest", response("CaptureExport"))},
	}
}
