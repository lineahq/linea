package mcp

import (
	"regexp"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var auditToolText = regexp.MustCompile(`^Audit events: [0-9]+ listed, newest first; more pages (true|false)\.$`)

// The audit tool pages the platform's audit log, which records this very
// session's calls as the local owner through MCP.
func TestListAuditEventsToolPagesTheLog(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	callTool(t, ctx, session, "linea_list_runs", map[string]any{})
	r, out := callTool(t, ctx, session, "linea_list_audit_events", map[string]any{"operation": "ListRuns"})
	if r.IsError {
		t.Fatalf("list audit = %+v", out)
	}
	events, _ := out["events"].([]any)
	if len(events) != 1 || textOf(t, r) != "Audit events: 1 listed, newest first; more pages false." {
		t.Fatalf("events = %s, %+v", textOf(t, r), out)
	}
	e, _ := events[0].(map[string]any)
	if e["principal"] != platform.LocalOwner || e["client"] != "mcp" || e["outcome"] != "allowed" || e["permission"] != "run.read" {
		t.Errorf("event = %+v", e)
	}
	if r, out = callTool(t, ctx, session, "linea_list_audit_events", map[string]any{"since": "yesterday"}); !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Errorf("invalid since = %+v", out)
	}
}
