package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/api"
)

// audit pages the server's audit log as text and JSON, and refuses a bad
// filter before it reaches the server.
func TestAuditPagesTheServerLog(t *testing.T) {
	ctx := context.Background()
	server := startServe(t)
	if code, _, stderr := runCLI(t, "runs", "--server", server); code != exitOK {
		t.Fatalf("runs: exit %d, %s", code, stderr)
	}
	var page api.AuditPage
	raw := parityCLI(t, ctx, []string{"audit", "--server", server, "--operation", "ListRuns", "--format", "json"})
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Events) != 1 || page.Events[0].Client != "http" || page.Events[0].Outcome != "allowed" {
		t.Fatalf("json page = %s, %v", raw, err)
	}
	code, out, stderr := runCLI(t, "audit", "--server", server, "--limit", "1")
	if code != exitOK || !strings.HasPrefix(out, "TIME") || !strings.Contains(out, "ListAuditEvents") || !strings.Contains(out, "next page: --cursor ") {
		t.Fatalf("text: exit %d, %s%s", code, out, stderr)
	}
	if code, _, stderr := runCLI(t, "audit", "--server", server, "--since", "yesterday"); code != exitError || !strings.Contains(stderr, "RFC 3339") {
		t.Fatalf("bad since: exit %d, %s", code, stderr)
	}
	if code, _, stderr := runCLI(t, "audit", "--server", server, "--outcome", "maybe"); code != exitError || !strings.Contains(stderr, "outcome") {
		t.Fatalf("bad outcome: exit %d, %s", code, stderr)
	}
}
