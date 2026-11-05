package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/api"
	resolver "github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// TestLabConfigWorksThroughEveryInterface checks that an ordinary
// lab configuration whose exporter reads a credential file this host does
// not have validates and runs through a lab team server with MCP (by text and
// by path, from a server config that allows the config's directory), HTTP and the
// CLI. The unreadable file is one stand-in reference, named in every
// interface; health_check is inert.
func TestLabConfigWorksThroughEveryInterface(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "usability"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "lab-config.yaml")
	fixturePath := filepath.Join(root, "lab-traces.json")
	configYAML, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	const credential = "/run/lab/authorization.yaml"
	// A direct command finds the file missing; the team server reads no host
	// file, so it is outside its roots. Both are stand-ins.
	const missing, outsideRoots = resolver.ReasonMissing, resolver.ReasonOutsideRoots
	assertReferences := func(t *testing.T, label, reason string, refs []resolver.Reference) {
		t.Helper()
		if len(refs) != 1 || refs[0].Scheme != "file" || refs[0].Target != credential ||
			refs[0].Source != resolver.SourceStandIn || refs[0].Reason != reason || refs[0].StandIn == "" {
			t.Fatalf("%s references = %+v", label, refs)
		}
	}

	ctx, baseURL, _ := startMCPServe(t)
	session, err := sdk.NewClient(&sdk.Implementation{Name: "usability-test", Version: "1"}, nil).
		Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if !strings.Contains(session.InitializeResult().Instructions, "stand-in") {
		t.Fatal("MCP instructions do not explain stand-ins")
	}

	type validation struct {
		Valid       bool                  `json:"valid"`
		Diagnostics []resolver.Diagnostic `json:"diagnostics"`
		References  []resolver.Reference  `json:"references"`
		StandIns    []string              `json:"stand_in_components"`
	}
	for name, source := range map[string]map[string]string{
		"text":     {"text": string(configYAML)},
		"labelled": mcpFile(t, configPath),
	} {
		var got validation
		parityTool(t, ctx, session, "linea_validate_config", map[string]any{"config": source}, &got)
		if !got.Valid || len(got.StandIns) != 0 {
			t.Fatalf("MCP validate by %s = %+v", name, got)
		}
		for _, d := range got.Diagnostics {
			if d.Severity == resolver.SeverityError {
				t.Fatalf("MCP validate by %s: error %+v", name, d)
			}
		}
		assertReferences(t, "MCP validate by "+name, outsideRoots, got.References)
	}

	var mcpRun struct {
		parityRun
		References []resolver.Reference `json:"references"`
	}
	parityTool(t, ctx, session, "linea_run", map[string]any{
		"config": mcpFile(t, configPath), "input_json": string(input),
	}, &mcpRun)
	assertParityRun(t, mcpRun.parityRun, mcpRun.RunID, "quick")
	assertReferences(t, "MCP run", outsideRoots, mcpRun.References)
	report := decodeParityReport(t, parityHTTPReport(t, ctx, baseURL, mcpRun.RunID))
	assertParitySummary(t, mcpRun.parityRun, report)
	assertReferences(t, "HTTP report", outsideRoots, report.References)
	if report.Result.Status != "completed" || len(report.Result.Outputs) != 1 || report.Result.Outputs[0].Counts.Items != 1 {
		t.Fatalf("lab run outputs = %+v (status %s); the filter drops okey-dokey-0 only", report.Result.Outputs, report.Result.Status)
	}

	// The metrics pipeline converts the lab's exponential histogram.
	metrics, err := os.ReadFile(filepath.Join(root, "lab-metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metricRun parityRun
	parityTool(t, ctx, session, "linea_run", map[string]any{
		"config": map[string]string{"text": string(configYAML)}, "input_json": string(metrics),
	}, &metricRun)
	assertParityRun(t, metricRun, metricRun.RunID, "quick")
	metricReport := decodeParityReport(t, parityHTTPReport(t, ctx, baseURL, metricRun.RunID))
	if out := metricReport.Result.Outputs; metricReport.Result.Status != "completed" || len(out) != 1 || out[0].Signal != "metrics" ||
		!bytes.Contains(out[0].Data, []byte(`"histogram"`)) || bytes.Contains(out[0].Data, []byte("exponentialHistogram")) {
		t.Fatalf("lab metric run = %s, outputs %+v", metricReport.Result.Status, out)
	}

	var inspection engine.Inspection
	decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, baseURL+"/api/v1/config/validate", api.InspectRequest{ConfigYAML: string(configYAML), ConfigLabel: configPath}, http.StatusOK), &inspection)
	if !inspection.Valid {
		t.Fatalf("HTTP inspection = %+v", inspection)
	}
	assertReferences(t, "HTTP inspection", outsideRoots, inspection.References)

	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"inspect", "--config", configPath}, &stdout, &stderr); code != exitOK ||
		!strings.Contains(stdout.String(), credential) {
		t.Fatalf("CLI inspect exit %d: %s%s", code, stdout.String(), stderr.String())
	}
	cliRaw := parityCLI(t, ctx, []string{"run", "--config", configPath, "--fixture", fixturePath})
	assertReferences(t, "CLI report", missing, decodeParityReport(t, cliRaw).References)
	// The batch processor flushes on its timer or at shutdown depending on
	// timing, which moves events between canonical segments, so the gate
	// compares what reached the exporter rather than whole-report bytes.
	sha := fmt.Sprintf("%x", sha256.Sum256(input))
	outputs := func(raw []byte) []byte {
		normalized := decodeParityReport(t, normalizeParityReport(t, raw, sha))
		encoded, err := json.Marshal(normalized.Result.Outputs)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	assertParityBytes(t, "HTTP / CLI lab outputs", outputs(parityHTTPReport(t, ctx, baseURL, mcpRun.RunID)), outputs(cliRaw))
}
