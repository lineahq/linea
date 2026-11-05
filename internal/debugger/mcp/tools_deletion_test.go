package mcp

import (
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var deletionToolText = regexp.MustCompile(`^(Run deleted|Suite and its member runs deleted)\.$`)

// A finished run deletes and then reads as not_found; a suite member is a
// conflict naming its suite until the suite deletes with it.
func TestDeletionToolsApplyTheHTTPRules(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	id := runID(t, r, out)
	if r, out = callTool(t, ctx, session, "linea_delete_run", map[string]any{"run_id": id}); r.IsError || out["deleted"] != true || textOf(t, r) != "Run deleted." {
		t.Fatalf("delete run = %s, %+v", textOf(t, r), out)
	}
	for _, name := range []string{"linea_get_run", "linea_delete_run"} {
		if r, out = callTool(t, ctx, session, name, map[string]any{"run_id": id}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
			t.Fatalf("%s after delete = %+v", name, out)
		}
	}

	_, suite := callTool(t, ctx, session, "linea_run_scenarios", suiteArgs())
	suiteID, _ := suite["suite_id"].(string)
	member, _ := suite["members"].([]any)[0].(map[string]any)["run_id"].(string)
	r, out = callTool(t, ctx, session, "linea_delete_run", map[string]any{"run_id": member})
	if message, _ := out["message"].(string); !r.IsError || out["code"] != string(platform.CodeConflict) || !strings.Contains(message, suiteID) {
		t.Fatalf("delete member = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_delete_suite", map[string]any{"suite_id": suiteID}); r.IsError || out["deleted"] != true || textOf(t, r) != "Suite and its member runs deleted." {
		t.Fatalf("delete suite = %s, %+v", textOf(t, r), out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_run", map[string]any{"run_id": member}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("member after suite delete = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_delete_suite", map[string]any{"suite_id": suiteID}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("suite after delete = %+v", out)
	}
}
