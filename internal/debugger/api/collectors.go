package api

import (
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func collectorPageRequest(r *http.Request) (platform.CollectorPageRequest, error) {
	req := platform.CollectorPageRequest{Cursor: r.URL.Query().Get("cursor")}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return req, &platform.Error{Code: platform.CodeInvalidRequest, Message: "invalid Collector page limit"}
		}
		req.Limit = n
	}
	return req, nil
}

func (s *Server) listCollectors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	req, err := collectorPageRequest(r)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	result, err := s.runs.ListCollectors(r.Context(), callerOf(r), req)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getCollector(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := s.runs.GetCollector(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) listCapturePoints(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	req, err := collectorPageRequest(r)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	result, err := s.runs.ListCapturePoints(r.Context(), callerOf(r), r.PathValue("id"), req)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func collectorPaths() map[string]any {
	page := []any{
		map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "description": "Zero or omitted uses default 25; also bounded by the platform item/byte budgets."},
		map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}},
	}
	responses := func(schema string) map[string]any {
		return map[string]any{
			"200": reply("Bounded page of fleet's connected Collectors.", ref(schema)),
			"400": operationFailure("Invalid paging request or cursor."),
			"404": operationFailure("Collector not found."),
			"409": operationFailure("Catalog changed; restart paging."),
			"413": operationFailure("An entry exceeds the query byte budget."),
			"503": operationFailure("Fleet not connected or platform closed."),
		}
	}
	return map[string]any{
		"/api/v1/collectors":                     map[string]any{"get": op("listCollectors", "List the Collectors connected to fleet's livecapture plugin; enabled when linea-server connects to fleet.", page, "", responses("CollectorPage"))},
		"/api/v1/collectors/{id}":                map[string]any{"get": op("getCollector", "Read one connected Collector's identity and capture availability.", []any{idParam}, "", responses("Collector"))},
		"/api/v1/collectors/{id}/capture-points": map[string]any{"get": op("listCapturePoints", "List a Collector's reported capture points and signals.", append([]any{idParam}, page...), "", responses("CapturePointPage"))},
	}
}
