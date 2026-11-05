package mcp

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// A suite reruns from its recorded inputs with the same outcomes, and its
// JUnit counts its members' assertions.
func TestRerunSuiteAndJUnitTools(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, first := callTool(t, ctx, session, "linea_run_scenarios", suiteArgs())
	if r.IsError || first["done"] != true {
		t.Fatal(first)
	}

	r, again := callTool(t, ctx, session, "linea_rerun_suite", map[string]any{"suite_id": first["suite_id"]})
	assertDevelopmentText(t, r, again)
	if r.IsError || again["suite_id"] == first["suite_id"] || again["done"] != true || again["outcome"] != "fail" {
		t.Fatal(again)
	}
	for i, m := range again["members"].([]any) {
		was := first["members"].([]any)[i].(map[string]any)
		if m := m.(map[string]any); m["label"] != was["label"] || m["outcome"] != was["outcome"] || m["run_id"] == was["run_id"] {
			t.Fatalf("member %d = %v, was %v", i, m, was)
		}
	}

	r, out := callTool(t, ctx, session, "linea_get_suite_junit", map[string]any{"suite_id": again["suite_id"]})
	assertDevelopmentText(t, r, out)
	var junit engine.JUnitSuites
	if r.IsError || out["suite_id"] != again["suite_id"] || xml.Unmarshal([]byte(out["junit"].(string)), &junit) != nil ||
		len(junit.Suites) != 2 || junit.Failures == 0 || !strings.HasPrefix(out["junit"].(string), "<?xml") {
		t.Fatal(out)
	}

	for _, tool := range []string{"linea_rerun_suite", "linea_get_suite_junit"} {
		r, out := callTool(t, ctx, session, tool, map[string]any{"suite_id": "suite-unknown"})
		assertToolError(t, r, out, platform.CodeNotFound, false)
		assertDevelopmentText(t, r, out)
	}
}
