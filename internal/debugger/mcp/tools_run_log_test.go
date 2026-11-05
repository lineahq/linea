package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// linea_get_run_log returns a failed run's log in structured output only.
func TestGetRunLogTool(t *testing.T) {
	const line = "error\tcollector failed with private@example.test\n"
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		_, _ = req.Log.Write([]byte(line))
		return nil, errors.New("collector failed to start")
	}
	ctx, session, service := newTestClient(t, platform.Options{Execute: execute})
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	id := runID(t, r, out)
	_, _ = service.GetRunReport(ctx, caller, id, true) // a failed run: run_failed

	r, out = callTool(t, ctx, session, "linea_get_run_log", map[string]any{"run_id": id})
	assertDevelopmentText(t, r, out, "collector failed")
	if r.IsError || out["run_id"] != id || out["kept"] != true || out["log"] != line || out["truncated"] != false ||
		!strings.Contains(r.Content[0].(*sdk.TextContent).Text, "Run log:") {
		t.Fatal(out)
	}
	r, out = callTool(t, ctx, session, "linea_get_run_log", map[string]any{"run_id": "run-unknown"})
	assertToolError(t, r, out, platform.CodeNotFound, false)
}
