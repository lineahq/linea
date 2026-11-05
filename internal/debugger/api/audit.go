package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// AuditEvent is one operation's audit record. It names
// resources by ID and holds no request content.
type AuditEvent struct {
	Seq           uint64    `json:"seq"`
	Time          time.Time `json:"time"`
	Principal     string    `json:"principal"`
	PrincipalKind string    `json:"principal_kind"`
	Client        string    `json:"client"`
	RequestID     string    `json:"request_id,omitempty"`
	Operation     string    `json:"operation"`
	Permission    string    `json:"permission,omitempty"`
	// Scope is "*" for platform-wide operations, "project:<id>" for one
	// resource's project, and empty for lists filtered by project.
	Scope     string   `json:"scope,omitempty"`
	Resources []string `json:"resources"`
	// Outcome is allowed or denied; Code is an allowed operation's error
	// code, empty on success.
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
}

// AuditPage is a page of audit events, newest first.
type AuditPage struct {
	Events     []AuditEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func (s *Server) listAuditEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := platform.AuditFilter{Principal: q.Get("principal"), Operation: q.Get("operation"), Outcome: platform.AuditOutcome(q.Get("outcome")), Cursor: q.Get("cursor")}
	invalid := func(message string) {
		writeOperationError(w, &platform.Error{Code: platform.CodeInvalidRequest, Message: "invalid request: " + message})
	}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			invalid("limit must be an integer")
			return
		}
		f.Limit = limit
	}
	if v := q.Get("since"); v != "" {
		since, err := time.Parse(time.RFC3339, v)
		if err != nil {
			invalid("since must be an RFC 3339 time")
			return
		}
		f.Since = since
	}
	caller := callerOf(r)
	f.Project = caller.Project
	page, err := s.runs.ListAuditEvents(r.Context(), caller, f)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, auditPageOf(page))
}

func auditPageOf(page *platform.AuditPage) AuditPage {
	out := AuditPage{Events: make([]AuditEvent, 0, len(page.Events)), NextCursor: page.NextCursor}
	for _, e := range page.Events {
		resources := e.Resources
		if resources == nil {
			resources = []string{}
		}
		out.Events = append(out.Events, AuditEvent{Seq: e.Seq, Time: e.Time, Principal: e.Principal, PrincipalKind: string(e.PrincipalKind),
			Client: e.Client, RequestID: e.RequestID, Operation: e.Operation, Permission: string(e.Permission), Scope: e.Scope,
			Resources: resources, Outcome: string(e.Outcome), Code: string(e.Code)})
	}
	return out
}

func auditPaths() map[string]any {
	query := func(name, description string, schema map[string]any) map[string]any {
		return map[string]any{"name": name, "in": "query", "description": description, "schema": schema}
	}
	text := map[string]any{"type": "string"}
	return map[string]any{
		"/api/v1/audit-events": map[string]any{"get": op("listAuditEvents",
			"The audit log, newest first: one event per operation with principal, client, request, operation, permission, scope, resource IDs and outcome; ?project= selects the events that decided in that project. Needs the audit.read permission.",
			[]any{
				query("principal", "Only this principal's events.", text),
				query("operation", "Only this operation's events, such as SubmitRun.", text),
				query("outcome", "allowed or denied.", map[string]any{"type": "string", "enum": []string{"allowed", "denied"}}),
				query("since", "Only events at or after this RFC 3339 time.", map[string]any{"type": "string", "format": "date-time"}),
				query("limit", "Page size, 1 to 500; default 100.", map[string]any{"type": "integer"}),
				query("cursor", "next_cursor of the previous page.", text),
			}, "", map[string]any{
				"200": reply("A page of audit events.", ref("AuditPage")),
				"400": operationFailure("Invalid filter, limit or cursor."),
				"403": operationFailure("The caller lacks audit.read."),
			})},
	}
}
