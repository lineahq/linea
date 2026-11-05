package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func (s *Server) getCollectorGroup(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetCollectorGroup(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) listCollectorGroups(w http.ResponseWriter, r *http.Request) {
	q, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	v, err := s.runs.ListCollectorGroups(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) listCollectorGroupMembers(w http.ResponseWriter, r *http.Request) {
	q, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	v, err := s.runs.ListCollectorGroupMembers(r.Context(), callerOf(r), r.PathValue("id"), q)
	captureReply(w, v, err)
}

func (s *Server) updateCaptureMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var q platform.CaptureMetadataUpdate
	if !decodeCaptureSized(w, r, &q, 16<<10) {
		return
	}
	v, err := s.runs.UpdateCaptureMetadata(r.Context(), callerOf(r), r.PathValue("id"), q)
	captureReply(w, v, err)
}

func captureManagementPaths() map[string]any {
	responses := func(schema string) map[string]any {
		return map[string]any{"200": reply("Fleet group or capture details.", ref(schema)), "400": operationFailure("Invalid request or revision."), "403": operationFailure("Owner access required."), "404": operationFailure("Group or capture not found."), "409": operationFailure("Revision changed; reload."), "503": operationFailure("Fleet or platform unavailable.")}
	}
	page := []any{map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer"}}, map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}}}
	return map[string]any{
		"/api/v1/collector-groups": map[string]any{
			"get":  op("listCollectorGroups", "List fleet's Collector groups.", page, "", responses("CollectorGroupPage")),
			"post": op("createCollectorGroup", "Create a fleet group listing agents or with a selector.", nil, "CollectorGroupSave", fleetFailures(map[string]any{"200": reply("The created group.", ref("CollectorGroup"))})),
		},
		"/api/v1/collector-groups/{id}": map[string]any{
			"get":    op("getCollectorGroup", "Read a fleet group's listed agents or selector.", []any{idParam}, "", responses("CollectorGroup")),
			"put":    op("replaceCollectorGroup", "Replace a fleet group at its current revision.", []any{idParam}, "CollectorGroupSave", fleetFailures(map[string]any{"200": reply("The replaced group.", ref("CollectorGroup"))})),
			"delete": op("deleteCollectorGroup", "Delete a fleet group at its current revision.", []any{idParam, map[string]any{"name": "revision", "in": "query", "required": true, "schema": map[string]any{"type": "integer"}}}, "", fleetFailures(map[string]any{"200": reply("Deleted.", ref("FleetDeleted"))})),
		},
		"/api/v1/collector-groups/{id}/members": map[string]any{"get": op("listCollectorGroupMembers", "Page a fleet group's agents and the connected Collectors each links.", append([]any{idParam}, page...), "", responses("CollectorGroupMembersPage"))},
	}
}
