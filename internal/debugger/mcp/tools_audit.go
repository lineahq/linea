package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// The audit log, readable with the audit.read permission.

type listAuditInput struct {
	Principal string `json:"principal,omitempty" jsonschema:"Only this principal's events."`
	Operation string `json:"operation,omitempty" jsonschema:"Only this operation's events, such as SubmitRun."`
	Outcome   string `json:"outcome,omitempty" jsonschema:"allowed or denied."`
	Since     string `json:"since,omitempty" jsonschema:"Only events at or after this RFC 3339 time."`
	Limit     int    `json:"limit,omitempty" jsonschema:"Page size, 1 to 500; default 100."`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type auditEvent struct {
	Seq           uint64    `json:"seq"`
	Time          time.Time `json:"time"`
	Principal     string    `json:"principal"`
	PrincipalKind string    `json:"principal_kind"`
	Client        string    `json:"client"`
	RequestID     string    `json:"request_id,omitempty"`
	Operation     string    `json:"operation"`
	Permission    string    `json:"permission,omitempty"`
	Scope         string    `json:"scope,omitempty"`
	Resources     []string  `json:"resources"`
	Outcome       string    `json:"outcome"`
	Code          string    `json:"code,omitempty"`
}

type auditPage struct {
	Events     []auditEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

func (p auditPage) summary() string {
	return fmt.Sprintf("Audit events: %d listed, newest first; more pages %t.", len(p.Events), p.NextCursor != "")
}

func addAuditTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_audit_events", "List the audit log newest first: one event per operation with principal, client, request, operation, permission, scope, resource IDs and whether it was allowed or denied. Needs the audit.read permission.",
		func(ctx context.Context, in listAuditInput) (summarized, error) {
			f := platform.AuditFilter{Principal: in.Principal, Operation: in.Operation, Outcome: platform.AuditOutcome(in.Outcome), Limit: in.Limit, Cursor: in.Cursor}
			if in.Since != "" {
				since, err := time.Parse(time.RFC3339, in.Since)
				if err != nil {
					return nil, &platform.Error{Code: platform.CodeInvalidRequest, Message: "invalid request: since must be an RFC 3339 time"}
				}
				f.Since = since
			}
			page, err := service.ListAuditEvents(ctx, callerOf(ctx), f)
			if err != nil {
				return nil, err
			}
			out := auditPage{Events: make([]auditEvent, 0, len(page.Events)), NextCursor: page.NextCursor}
			for _, e := range page.Events {
				resources := e.Resources
				if resources == nil {
					resources = []string{}
				}
				out.Events = append(out.Events, auditEvent{Seq: e.Seq, Time: e.Time, Principal: e.Principal, PrincipalKind: string(e.PrincipalKind),
					Client: e.Client, RequestID: e.RequestID, Operation: e.Operation, Permission: string(e.Permission), Scope: e.Scope,
					Resources: resources, Outcome: string(e.Outcome), Code: string(e.Code)})
			}
			return out, nil
		})
}
