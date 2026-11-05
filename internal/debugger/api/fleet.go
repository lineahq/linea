package api

import (
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Fleet management: fleet's inventory, groups, events, configs,
// assignments and rollouts through the team server's fleet connection. Writes are
// gated by fleet's role check on the team server's credential.

func (s *Server) routeFleet() {
	s.mux.HandleFunc("GET /api/v1/fleet", s.getFleetOverview)
	s.mux.HandleFunc("GET /api/v1/fleet/agents", s.listFleetAgents)
	s.mux.HandleFunc("GET /api/v1/fleet/agents/{id}", s.getFleetAgent)
	s.mux.HandleFunc("POST /api/v1/fleet/agents/{id}/config-retry", s.retryFleetAgentConfig)
	s.mux.HandleFunc("GET /api/v1/fleet/events", s.listFleetEvents)
	s.mux.HandleFunc("POST /api/v1/collector-groups", s.saveCollectorGroup)
	s.mux.HandleFunc("PUT /api/v1/collector-groups/{id}", s.saveCollectorGroup)
	s.mux.HandleFunc("DELETE /api/v1/collector-groups/{id}", s.deleteCollectorGroup)
	s.mux.HandleFunc("GET /api/v1/fleet/configs", s.listFleetConfigs)
	s.mux.HandleFunc("POST /api/v1/fleet/configs", s.createFleetConfig)
	s.mux.HandleFunc("GET /api/v1/fleet/configs/{id}", s.getFleetConfig)
	s.mux.HandleFunc("PATCH /api/v1/fleet/configs/{id}", s.updateFleetConfig)
	s.mux.HandleFunc("DELETE /api/v1/fleet/configs/{id}", s.deleteFleetConfig)
	s.mux.HandleFunc("GET /api/v1/fleet/configs/{id}/versions", s.listFleetConfigVersions)
	s.mux.HandleFunc("POST /api/v1/fleet/configs/{id}/versions", s.createFleetConfigVersion)
	s.mux.HandleFunc("GET /api/v1/fleet/configs/{id}/versions/{version}", s.getFleetConfigVersion)
	s.mux.HandleFunc("GET /api/v1/fleet/assignments", s.listFleetAssignments)
	s.mux.HandleFunc("POST /api/v1/fleet/assignments", s.createFleetAssignment)
	s.mux.HandleFunc("GET /api/v1/fleet/assignments/{id}", s.getFleetAssignment)
	s.mux.HandleFunc("GET /api/v1/fleet/assignments/{id}/preview", s.previewFleetAssignment)
	s.mux.HandleFunc("GET /api/v1/fleet/rollouts", s.listFleetRollouts)
	s.mux.HandleFunc("POST /api/v1/fleet/rollouts", s.startFleetRollout)
	s.mux.HandleFunc("GET /api/v1/fleet/rollouts/{id}", s.getFleetRollout)
	s.mux.HandleFunc("GET /api/v1/fleet/rollouts/{id}/targets", s.listFleetRolloutTargets)
	s.mux.HandleFunc("POST /api/v1/fleet/rollouts/{id}/actions", s.controlFleetRollout)
}

// fleetBody is the bound of a fleet management request; a config version's
// YAML is at most 128 KiB, which JSON escaping can grow.
const fleetBody = 512 << 10

func badQuery(message string) error {
	return &platform.Error{Code: platform.CodeInvalidRequest, Message: message}
}

// revisionQuery reads the revision a DELETE names.
func revisionQuery(r *http.Request) (int64, error) {
	n, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || n < 1 {
		return 0, badQuery("a delete names the current revision: ?revision=N")
	}
	return n, nil
}

// paged runs a page read with the request's limit and cursor.
func paged[T any](w http.ResponseWriter, r *http.Request, read func(platform.CollectorPageRequest) (T, error)) {
	q, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	v, err := read(q)
	captureReply(w, v, err)
}

func (s *Server) getFleetOverview(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetFleetOverview(r.Context(), callerOf(r), r.URL.Query().Get("group_by"))
	captureReply(w, v, err)
}

func (s *Server) listFleetAgents(w http.ResponseWriter, r *http.Request) {
	page, err := collectorPageRequest(r)
	if err != nil {
		captureReply(w, nil, err)
		return
	}
	q := platform.FleetAgentQuery{Limit: page.Limit, Cursor: page.Cursor, Selector: r.URL.Query().Get("selector"), Sort: r.URL.Query().Get("sort")}
	for name, field := range map[string]**bool{"connected": &q.Connected, "healthy": &q.Healthy} {
		if raw := r.URL.Query().Get(name); raw != "" {
			b, err := strconv.ParseBool(raw)
			if err != nil {
				captureReply(w, nil, badQuery("invalid "+name+" filter"))
				return
			}
			*field = &b
		}
	}
	v, err := s.runs.ListFleetAgents(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) getFleetAgent(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetFleetAgent(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) retryFleetAgentConfig(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.RetryFleetAgentConfig(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) listFleetEvents(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetEventPage, error) {
		return s.runs.ListFleetEvents(r.Context(), callerOf(r), q)
	})
}

func (s *Server) saveCollectorGroup(w http.ResponseWriter, r *http.Request) {
	var q platform.CollectorGroupSave
	if !decodeCaptureSized(w, r, &q, 128<<10) {
		return
	}
	if id := r.PathValue("id"); id != "" || q.ID != "" {
		if id == "" || q.ID != "" && q.ID != id {
			captureReply(w, nil, badQuery("create a group with POST and no ID; replace it with PUT to its ID"))
			return
		}
		q.ID = id
	}
	v, err := s.runs.SaveCollectorGroup(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) deleteCollectorGroup(w http.ResponseWriter, r *http.Request) {
	rev, err := revisionQuery(r)
	if err == nil {
		err = s.runs.DeleteCollectorGroup(r.Context(), callerOf(r), r.PathValue("id"), platform.CollectorGroupDelete{Revision: rev})
	}
	captureReply(w, FleetDeleted{Deleted: err == nil}, err)
}

func (s *Server) listFleetConfigs(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetConfigPage, error) {
		return s.runs.ListFleetConfigs(r.Context(), callerOf(r), q)
	})
}

func (s *Server) createFleetConfig(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetConfigCreate
	if !decodeCaptureSized(w, r, &q, 16<<10) {
		return
	}
	v, err := s.runs.CreateFleetConfig(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) getFleetConfig(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetFleetConfig(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) updateFleetConfig(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetConfigUpdate
	if !decodeCaptureSized(w, r, &q, 16<<10) {
		return
	}
	v, err := s.runs.UpdateFleetConfig(r.Context(), callerOf(r), r.PathValue("id"), q)
	captureReply(w, v, err)
}

func (s *Server) deleteFleetConfig(w http.ResponseWriter, r *http.Request) {
	rev, err := revisionQuery(r)
	if err == nil {
		err = s.runs.DeleteFleetConfig(r.Context(), callerOf(r), r.PathValue("id"), platform.FleetConfigDelete{Revision: rev})
	}
	captureReply(w, FleetDeleted{Deleted: err == nil}, err)
}

func (s *Server) listFleetConfigVersions(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetConfigVersionPage, error) {
		return s.runs.ListFleetConfigVersions(r.Context(), callerOf(r), r.PathValue("id"), q)
	})
}

func (s *Server) createFleetConfigVersion(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetConfigVersionCreate
	if !decodeCaptureSized(w, r, &q, fleetBody) {
		return
	}
	v, err := s.runs.CreateFleetConfigVersion(r.Context(), callerOf(r), r.PathValue("id"), q)
	captureReply(w, v, err)
}

func (s *Server) getFleetConfigVersion(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		captureReply(w, nil, &platform.Error{Code: platform.CodeNotFound, Message: "config version not found"})
		return
	}
	v, err := s.runs.GetFleetConfigVersion(r.Context(), callerOf(r), r.PathValue("id"), n)
	captureReply(w, v, err)
}

func (s *Server) listFleetAssignments(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetAssignmentPage, error) {
		return s.runs.ListFleetAssignments(r.Context(), callerOf(r), q)
	})
}

func (s *Server) createFleetAssignment(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetAssignmentCreate
	if !decodeCaptureSized(w, r, &q, 64<<10) {
		return
	}
	v, err := s.runs.CreateFleetAssignment(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) getFleetAssignment(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetFleetAssignment(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) previewFleetAssignment(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetAssignmentPreviewPage, error) {
		return s.runs.PreviewFleetAssignment(r.Context(), callerOf(r), r.PathValue("id"), q)
	})
}

func (s *Server) listFleetRollouts(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetRolloutPage, error) {
		return s.runs.ListFleetRollouts(r.Context(), callerOf(r), q)
	})
}

func (s *Server) startFleetRollout(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetRolloutStart
	if !decodeCaptureSized(w, r, &q, 64<<10) {
		return
	}
	v, err := s.runs.StartFleetRollout(r.Context(), callerOf(r), q)
	captureReply(w, v, err)
}

func (s *Server) getFleetRollout(w http.ResponseWriter, r *http.Request) {
	v, err := s.runs.GetFleetRollout(r.Context(), callerOf(r), r.PathValue("id"))
	captureReply(w, v, err)
}

func (s *Server) listFleetRolloutTargets(w http.ResponseWriter, r *http.Request) {
	paged(w, r, func(q platform.CollectorPageRequest) (*platform.FleetRolloutTargetPage, error) {
		return s.runs.ListFleetRolloutTargets(r.Context(), callerOf(r), r.PathValue("id"), platform.FleetRolloutTargetQuery{Limit: q.Limit, Cursor: q.Cursor, Outcome: r.URL.Query().Get("outcome")})
	})
}

func (s *Server) controlFleetRollout(w http.ResponseWriter, r *http.Request) {
	var q platform.FleetRolloutAction
	if !decodeCaptureSized(w, r, &q, 1<<10) {
		return
	}
	v, err := s.runs.ControlFleetRollout(r.Context(), callerOf(r), r.PathValue("id"), q)
	captureReply(w, v, err)
}

// FleetDeleted answers a fleet delete.
type FleetDeleted struct {
	Deleted bool `json:"deleted"`
}

// fleetFailures adds the refusals every fleet management path shares.
func fleetFailures(ok map[string]any) map[string]any {
	ok["400"] = operationFailure("Invalid request, or fleet refused the input (including an inline secret in a config version).")
	ok["403"] = operationFailure("Fleet refused the team server's credential this action; writes need a fleet admin credential.")
	ok["404"] = operationFailure("Fleet does not know the resource.")
	ok["409"] = operationFailure("The revision changed, the resource is in use, or a rollout conflicts; reload.")
	ok["503"] = operationFailure("The server has no fleet connection, fleet is unreachable, or fleet runs without config management.")
	return ok
}

func fleetPaths() map[string]any {
	ok := func(schema string) map[string]any {
		return fleetFailures(map[string]any{"200": reply("Fleet's answer.", ref(schema))})
	}
	query := func(name, typ, desc string) any {
		return map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": typ}, "description": desc}
	}
	page := []any{query("limit", "integer", "Page entries, at most 500."), query("cursor", "string", "Opaque cursor from next_cursor.")}
	withID := func(extra ...any) []any { return append([]any{idParam}, extra...) }
	revision := query("revision", "integer", "The current revision.")
	version := map[string]any{"name": "version", "in": "path", "required": true, "schema": map[string]any{"type": "integer"}}
	return map[string]any{
		"/api/v1/fleet": map[string]any{"get": op("getFleetOverview", "Fleet's version, whether config management is on, and agent counts, grouped by a promoted attribute with group_by. Without a fleet connection enabled is false.",
			[]any{query("group_by", "string", "service.name, service.version, host.name, k8s.namespace.name or k8s.pod.name.")}, "", ok("FleetOverview"))},
		"/api/v1/fleet/agents": map[string]any{"get": op("listFleetAgents", "Page fleet's agents with the Collectors each links to live capture.",
			append(page, query("connected", "boolean", "Only connected or disconnected agents."), query("healthy", "boolean", "Only healthy or unhealthy agents."),
				query("selector", "string", "Fleet selector, such as service.name=cart,k8s.namespace.name in (shop)."), query("sort", "string", "A promoted attribute.")), "", ok("FleetAgentPage"))},
		"/api/v1/fleet/agents/{id}":              map[string]any{"get": op("getFleetAgent", "One agent with its components, live capture links and remote config state.", withID(), "", ok("FleetAgentDetail"))},
		"/api/v1/fleet/agents/{id}/config-retry": map[string]any{"post": op("retryFleetAgentConfig", "Clear the hold fleet places on a desired config the agent failed to apply, so fleet sends it again.", withID(), "", ok("FleetAgentConfig"))},
		"/api/v1/fleet/events":                   map[string]any{"get": op("listFleetEvents", "Page fleet's redacted change events in commit order after the cursor; an expired cursor answers 409, restart without one.", page, "", ok("FleetEventPage"))},
		"/api/v1/fleet/configs": map[string]any{
			"get":  op("listFleetConfigs", "Page fleet's configs.", page, "", ok("FleetConfigPage")),
			"post": op("createFleetConfig", "Create an empty config; its versions follow.", nil, "FleetConfigCreate", ok("FleetConfig")),
		},
		"/api/v1/fleet/configs/{id}": map[string]any{
			"get":    op("getFleetConfig", "Read a config's metadata.", withID(), "", ok("FleetConfig")),
			"patch":  op("updateFleetConfig", "Rename or redescribe a config at its current revision.", withID(), "FleetConfigUpdate", ok("FleetConfig")),
			"delete": op("deleteFleetConfig", "Delete a config and its versions at its current revision; fleet refuses while an assignment uses it.", withID(revision), "", ok("FleetDeleted")),
		},
		"/api/v1/fleet/configs/{id}/versions": map[string]any{
			"get":  op("listFleetConfigVersions", "Page a config's versions, newest first, without bodies.", withID(page...), "", ok("FleetConfigVersionPage")),
			"post": op("createFleetConfigVersion", "Store the config's next immutable version: at most 128 KiB of Collector YAML with secrets as ${env:...} or ${file:...} references.", withID(), "FleetConfigVersionCreate", ok("FleetConfigVersion")),
		},
		"/api/v1/fleet/configs/{id}/versions/{version}": map[string]any{"get": op("getFleetConfigVersion", "Read one version with its body; fleet returns bodies to an admin credential only.", withID(version), "", ok("FleetConfigVersion"))},
		"/api/v1/fleet/assignments": map[string]any{
			"get":  op("listFleetAssignments", "Page config assignments.", page, "", ok("FleetAssignmentPage")),
			"post": op("createFleetAssignment", "Assign a config version to a group or selector at a layer and start the rollout that introduces it.", nil, "FleetAssignmentCreate", ok("FleetAssignment")),
		},
		"/api/v1/fleet/assignments/{id}":         map[string]any{"get": op("getFleetAssignment", "Read a config assignment.", withID(), "", ok("FleetAssignment"))},
		"/api/v1/fleet/assignments/{id}/preview": map[string]any{"get": op("previewFleetAssignment", "Page the agents an assignment targets now and the layers each would receive.", withID(page...), "", ok("FleetAssignmentPreviewPage"))},
		"/api/v1/fleet/rollouts": map[string]any{
			"get":  op("listFleetRollouts", "Page the active and recently finished rollouts.", page, "", ok("FleetRolloutPage")),
			"post": op("startFleetRollout", "Move an assignment to another version, or remove it, in waves.", nil, "FleetRolloutStart", ok("FleetRollout")),
		},
		"/api/v1/fleet/rollouts/{id}":         map[string]any{"get": op("getFleetRollout", "Read a rollout with its waves' progress.", withID(), "", ok("FleetRollout"))},
		"/api/v1/fleet/rollouts/{id}/targets": map[string]any{"get": op("listFleetRolloutTargets", "Page a rollout's targets, optionally of one outcome.", withID(append(page, query("outcome", "string", "pending, released, baking, offline, succeeded, failed, unhealthy, timed_out, unmatched or incompatible."))...), "", ok("FleetRolloutTargetPage"))},
		"/api/v1/fleet/rollouts/{id}/actions": map[string]any{"post": op("controlFleetRollout", "Pause, resume, roll back or cancel a rollout; resuming accepts the failures that paused it. Roll back and cancel both return released agents to the version they had before: roll back ends when they report it, cancel ends at once.", withID(), "FleetRolloutAction", ok("FleetRollout"))},
	}
}
