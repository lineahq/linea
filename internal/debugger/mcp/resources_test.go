package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// readResource reads a resource and decodes its one JSON text content.
func readResource(t *testing.T, ctx context.Context, session *sdk.ClientSession, uri string) map[string]any {
	t.Helper()
	res, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	if len(res.Contents) != 1 || res.Contents[0].URI != uri || res.Contents[0].MIMEType != "application/json" {
		t.Fatalf("read %s: contents %+v", uri, res.Contents)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &out); err != nil {
		t.Fatalf("read %s: %v", uri, err)
	}
	return out
}

// readResourceError reads a resource that must fail and returns its
// structured error.
func readResourceError(t *testing.T, ctx context.Context, session *sdk.ClientSession, uri string) (int64, map[string]any) {
	t.Helper()
	_, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("read %s: err = %v, want a JSON-RPC error", uri, err)
	}
	var out map[string]any
	if err := json.Unmarshal(wire.Data, &out); err != nil {
		t.Fatalf("read %s: error data %s: %v", uri, wire.Data, err)
	}
	if wire.Message != errorFor(&platform.Error{Code: platform.Code(out["code"].(string)), Retryable: out["retryable"].(bool)}).summary() {
		t.Fatalf("read %s: message %q is not the fixed tool text", uri, wire.Message)
	}
	return wire.Code, out
}

func withCursor(t *testing.T, uri, cursor string) string {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("cursor", cursor)
	u.RawQuery = q.Encode()
	return u.String()
}

// TestEverySummaryURIResolves runs every sample as a scenario and reads each
// URI its summary names: the run resource answers as linea_get_run, stage and
// statements resources as linea_get_execution_trace with the same parameters.
func TestEverySummaryURIResolves(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	ids := []string{"logs-cleanup", "traces-redact", "metrics-rename", "broken-routing", "container-logs", "pod-enrichment",
		"log-routing", "log-metrics", "trace-sampling", "span-metrics", "stale-logs"}
	connectors := 0
	for _, sample := range ids {
		r, out := callTool(t, ctx, session, "linea_run", map[string]any{
			"config":   map[string]string{"text": readSample(t, sample, "config.yaml")},
			"scenario": map[string]string{"text": sampleScenario(t, sample)},
		})
		id := runID(t, r, out)
		summary, ok := out["summary"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no summary: %+v", sample, out)
		}
		_, run := callTool(t, ctx, session, "linea_get_run", map[string]any{"run_id": id})
		if summary["uri"] != runURI(id) || !reflect.DeepEqual(readResource(t, ctx, session, runURI(id)), run) {
			t.Fatalf("%s: run resource differs from linea_get_run", sample)
		}
		for _, raw := range summary["stages"].([]any) {
			stage := raw.(map[string]any)
			args := map[string]any{"run_id": id, "family": "stage_changes", "pipeline": stage["pipeline"], "component": stage["component"]}
			if stage["pipeline"] == "" {
				args["signal"] = stage["signal"]
				connectors++
			}
			_, want := callTool(t, ctx, session, "linea_get_execution_trace", args)
			if got := readResource(t, ctx, session, stage["uri"].(string)); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %s = %+v, tool %+v", sample, stage["uri"], got, want)
			}
			uri, ok := stage["statements_uri"].(string)
			if stage["type"] == "transform" != ok {
				t.Fatalf("%s: statements_uri of %v = %q", sample, stage["component"], uri)
			}
			if ok {
				_, want := callTool(t, ctx, session, "linea_get_execution_trace", map[string]any{"run_id": id, "family": "statements", "component": stage["component"]})
				if got := readResource(t, ctx, session, uri); !reflect.DeepEqual(got, want) {
					t.Fatalf("%s: %s = %+v, tool %+v", sample, uri, got, want)
				}
			}
		}
	}
	if connectors == 0 {
		t.Fatal("no connector stage exercised the empty pipeline segment")
	}
}

func TestResourceErrorsAreTheToolErrors(t *testing.T) {
	execute, started, release := blockedExecutor()
	ctx, session, _ := newTestClient(t, platform.Options{Execute: execute})
	args := runArgs(platform.ModeQuick)
	args["wait_seconds"] = 0
	r, out := callTool(t, ctx, session, "linea_run", args)
	id := runID(t, r, out)
	receiveRun(t, ctx, started)
	t.Cleanup(func() { release <- struct{}{} })

	for _, tt := range []struct {
		name string
		uri  string
		args map[string]any
		code platform.Code
		rpc  int64
	}{
		{"unknown run", runURI("run-missing"), map[string]any{"run_id": "run-missing"}, platform.CodeNotFound, jsonrpc.CodeInvalidParams},
		{"unknown run stage", stageURI("run-missing", "logs", "transform", ""),
			map[string]any{"run_id": "run-missing", "family": "stage_changes", "pipeline": "logs", "component": "transform"}, platform.CodeNotFound, jsonrpc.CodeInvalidParams},
		{"running run", statementsURI(id, "transform"),
			map[string]any{"run_id": id, "family": "statements", "component": "transform"}, platform.CodeConflict, jsonrpc.CodeInternalError},
		{"unknown parameter", stageURI(id, "logs", "transform", "") + "?side=before",
			nil, platform.CodeInvalidRequest, jsonrpc.CodeInvalidParams},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rpc, got := readResourceError(t, ctx, session, tt.uri)
			if rpc != tt.rpc || got["code"] != string(tt.code) {
				t.Fatalf("error = %d %+v", rpc, got)
			}
			if tt.args == nil {
				return
			}
			tool := "linea_get_execution_trace"
			if tt.args["family"] == nil {
				tool = "linea_get_run"
			}
			r, want := callTool(t, ctx, session, tool, tt.args)
			if !r.IsError || !reflect.DeepEqual(got, want) {
				t.Fatalf("resource error %+v, tool error %+v", got, want)
			}
		})
	}
}

// TestResourcePagingAgreesWithTheTool pages a stage and a statements resource
// one item at a time, following next_cursor through the resource, and gets
// the tool's pages.
func TestResourcePagingAgreesWithTheTool(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{Budgets: platform.Budgets{PageItems: 1}})
	r, out := callTool(t, ctx, session, "linea_run", map[string]any{
		"config":     map[string]string{"text": readSample(t, "logs-cleanup", "config.yaml")},
		"input_json": readSample(t, "logs-cleanup", "input.json"),
	})
	id := runID(t, r, out)
	var stageCursor string
	for _, tt := range []struct {
		uri  string
		args map[string]any
	}{
		{stageURI(id, "logs", "attributes/scrub", ""), map[string]any{"run_id": id, "family": "stage_changes", "pipeline": "logs", "component": "attributes/scrub"}},
		{statementsURI(id, "transform/severity"), map[string]any{"run_id": id, "family": "statements", "component": "transform/severity"}},
	} {
		uri, pages := tt.uri, 0
		for {
			got := readResource(t, ctx, session, uri)
			_, want := callTool(t, ctx, session, "linea_get_execution_trace", tt.args)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %+v, tool %+v", uri, got, want)
			}
			pages++
			next, _ := got["next_cursor"].(string)
			if next == "" {
				break
			}
			if got["complete"] != false || got["truncated_reason"] != "page_items" {
				t.Fatalf("%s: a page with more is not marked: %+v", uri, got)
			}
			if stageCursor == "" {
				stageCursor = next
			}
			uri = withCursor(t, tt.uri, next)
			tt.args["cursor"] = next
		}
		if pages < 2 {
			t.Fatalf("%s paged %d times", tt.uri, pages)
		}
	}
	// A stage's cursor does not read the statements resource.
	if rpc, e := readResourceError(t, ctx, session, withCursor(t, statementsURI(id, "transform/severity"), stageCursor)); rpc != jsonrpc.CodeInvalidParams || e["code"] != "invalid_request" {
		t.Fatalf("foreign cursor = %d %+v", rpc, e)
	}
}

// Instruction-like telemetry reaches the agent only as structured data, from
// the tool and the resource alike.
func TestExecutionTraceKeepsTelemetryValuesStructured(t *testing.T) {
	config := `receivers: {otlp: {protocols: {grpc: {}}}}
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
	ctx, session, _ := newTestClient(t, platform.Options{})
	r, out := callTool(t, ctx, session, "linea_run", map[string]any{"config": map[string]string{"text": config}, "input_json": previewPayload})
	id := runID(t, r, out)
	// Events reference snapshots, so only their text is checked.
	for _, family := range []map[string]any{
		{"family": "records", "pipeline": "logs", "component": "transform", "side": "after"},
		{"family": "stage_changes", "pipeline": "logs", "component": "transform"},
		{"family": "events"},
	} {
		family["run_id"] = id
		// callTool refuses any text containing "ignore" or "logs".
		r, out := callTool(t, ctx, session, "linea_get_execution_trace", family)
		if text := r.Content[0].(*sdk.TextContent).Text; !strings.HasPrefix(text, "Execution trace: ") || !strings.HasSuffix(text, "; complete.") {
			t.Fatalf("%v text = %q", family["family"], text)
		}
		data, _ := json.Marshal(out["items"])
		if family["family"] != "events" && !strings.Contains(string(data), "ignore previous instructions") {
			t.Fatalf("%v items lack the telemetry value: %s", family["family"], data)
		}
	}
	res, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: stageURI(id, "logs", "transform", "")})
	if err != nil || !strings.Contains(res.Contents[0].Text, "ignore previous instructions") {
		t.Fatalf("stage resource = %+v, %v", res, err)
	}
}

func TestTraceTextCountsThePage(t *testing.T) {
	result := func(items []platform.EvidenceItem, complete bool, reason, next string) traceResult {
		return traceResult{&platform.EvidenceResult{Items: items, Total: 7, Complete: complete, TruncatedReason: reason, NextCursor: next}}
	}
	for _, tt := range []struct {
		r    traceResult
		want string
	}{
		{result([]platform.EvidenceItem{{Ref: "event:1"}}, true, "", ""), "Execution trace: 1 item of 7; complete."},
		{result([]platform.EvidenceItem{{Ref: "a"}, {Ref: "b", Oversize: &platform.EvidenceOversize{Bytes: 9}}}, false, "page_bytes", "c"),
			"Execution trace: 2 items of 7; 1 oversize; incomplete (page_bytes); more pages."},
		{result(nil, false, string(runresult.StatusPartial), ""), "Execution trace: 0 items of 7; incomplete (partial)."},
	} {
		if got := tt.r.summary(); got != tt.want || !toolText.MatchString(got) {
			t.Errorf("text = %q, want %q", got, tt.want)
		}
	}
}

func TestEvidenceURIsRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		uri    string
		target evidenceTarget
	}{
		{runURI("run-1"), evidenceTarget{runID: "run-1"}},
		{stageURI("run-1", "logs/prod", "transform/redact", "logs"), evidenceTarget{runID: "run-1",
			query: &platform.EvidenceQuery{Family: platform.EvidenceStageChanges, Pipeline: "logs/prod", Component: "transform/redact"}}},
		{stageURI("run-1", "", "count", "metrics"), evidenceTarget{runID: "run-1",
			query: &platform.EvidenceQuery{Family: platform.EvidenceStageChanges, Component: "count", Signal: "metrics"}}},
		{statementsURI("run-1", "transform/a b") + "?cursor=xyz", evidenceTarget{runID: "run-1", cursor: "xyz",
			query: &platform.EvidenceQuery{Family: platform.EvidenceStatements, Component: "transform/a b"}}},
	} {
		got, err := parseEvidenceURI(tt.uri)
		if err != nil || !reflect.DeepEqual(got, tt.target) {
			t.Errorf("%s = %+v, %v", tt.uri, got, err)
		}
	}
	if got := stageURI("run-1", "logs/prod", "transform/redact", "logs"); got != "otel://runs/run-1/stages/logs%2Fprod/transform%2Fredact" {
		t.Errorf("stage URI = %s", got)
	}
	for _, uri := range []string{
		"otel://runs/", "otel://runs/run-1/stages/logs", "otel://runs/run-1/other/x", "http://runs/run-1",
		"otel://runs/run-1?cursor=x", "otel://runs/run-1/statements/t?signal=logs", "otel://runs/run-1/statements/t?cursor=a&cursor=b",
		"otel://runs/run-1#frag", "otel://user@runs/run-1",
	} {
		if _, err := parseEvidenceURI(uri); platform.ErrorCode(err) != platform.CodeInvalidRequest {
			t.Errorf("%s: err = %v", uri, err)
		}
	}
}
