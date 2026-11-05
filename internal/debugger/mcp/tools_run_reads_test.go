package mcp

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

var runReadToolText = regexp.MustCompile(`^(Run inputs: (quick|scenario) run; [0-9]+ root sources?, [0-9]+ input files?, [0-9]+ required environment names?|Snapshot: [0-9]+ bytes of recorded OTLP JSON|Suites: [0-9]+ listed with [0-9]+ members?|Candidate: valid (true|false); [0-9]+ errors?, [0-9]+ warnings?; [0-9]+ pipelines? affected; saved (true|false))\.$`)

const candidateBase = `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform:
    log_statements:
      - context: log
        statements: ['set(log.attributes["copy"], log.body)']
exporters: {otlp/backend: {endpoint: localhost:4317, tls: {insecure: true}}}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform], exporters: [otlp/backend]}
`

// TestRunReadToolsMapThePlatform reads a finished run's inputs and a
// snapshot, lists suites and validates a candidate through MCP.
func TestRunReadToolsMapThePlatform(t *testing.T) {
	ctx, session, service := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", map[string]any{"config": map[string]string{"text": candidateBase}, "input_json": previewPayload})
	id := runID(t, r, out)

	r, out = callTool(t, ctx, session, "linea_get_run_inputs", map[string]any{"run_id": id})
	config, _ := out["config"].(map[string]any)
	if r.IsError || out["mode"] != "quick" || config["text"] != candidateBase || textOf(t, r) != "Run inputs: quick run; 2 root sources, 0 input files, 0 required environment names." {
		t.Fatalf("inputs = %s, %+v", textOf(t, r), out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_run_inputs", map[string]any{"run_id": "run-missing"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("inputs of a missing run = %+v", out)
	}

	rep, err := service.GetRunReport(ctx, platform.Local("test"), id, true)
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	var ref string
	for _, e := range rep.Result.Events {
		if e.Snapshot != "" {
			ref = e.Snapshot
			break
		}
	}
	want, err := service.GetSnapshot(ctx, platform.Local("test"), id, ref)
	if err != nil {
		t.Fatal(err)
	}
	r, out = callTool(t, ctx, session, "linea_get_snapshot", map[string]any{"run_id": id, "ref": ref})
	got, _ := base64.StdEncoding.DecodeString(out["snapshot_base64"].(string))
	if r.IsError || string(got) != string(want) || out["bytes"] != float64(len(want)) {
		t.Fatalf("snapshot = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_get_snapshot", map[string]any{"run_id": id, "ref": "sha256:missing"}); !r.IsError || out["code"] != string(platform.CodeNotFound) {
		t.Fatalf("missing snapshot = %+v", out)
	}

	r, out = callTool(t, ctx, session, "linea_list_suites", map[string]any{})
	if suites, _ := out["suites"].([]any); r.IsError || len(suites) != 0 || textOf(t, r) != "Suites: 0 listed with 0 members." {
		t.Fatalf("suites = %s, %+v", textOf(t, r), out)
	}

	edit := map[string]any{"processor": "transform", "signal": "logs", "group": 0, "index": 0, "op": "replace",
		"expected": `set(log.attributes["copy"], log.body)`, "text": `set(log.attributes["kept"], log.body)`}
	r, out = callTool(t, ctx, session, "linea_create_candidate", map[string]any{"config": map[string]string{"text": candidateBase}, "edit": edit})
	if text := textOf(t, r); r.IsError || out["valid"] != true || !strings.Contains(out["proposed_yaml"].(string), `"kept"`) || text != "Candidate: valid true; 0 errors, 0 warnings; 1 pipeline affected; saved false." {
		t.Fatalf("candidate = %s, %+v", text, out)
	}
	edit["expected"] = "stale"
	if r, out = callTool(t, ctx, session, "linea_create_candidate", map[string]any{"config": map[string]string{"text": candidateBase}, "edit": edit}); !r.IsError || out["code"] != string(platform.CodeConflict) {
		t.Fatalf("stale edit = %+v", out)
	}
	if r, out = callTool(t, ctx, session, "linea_create_candidate", map[string]any{"config": map[string]string{"text": candidateBase}}); !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("no edit = %+v", out)
	}
}
