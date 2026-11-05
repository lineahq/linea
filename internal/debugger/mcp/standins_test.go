package mcp

import (
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestServerInstructionsTellAgentsAboutStandIns: the instructions are fixed
// text naming inline or path sources, stand-ins and how to supply real
// values, and that inconclusive is never a pass.
func TestServerInstructionsTellAgentsAboutStandIns(t *testing.T) {
	_, session, _ := newTestClient(t, platform.Options{})
	got := session.InitializeResult().Instructions
	if got != serverInstructions {
		t.Fatalf("instructions = %q", got)
	}
	for _, want := range []string{"inline text", "reads no host file", "stand-in", "environment:", "mocks.files", "inconclusive assertion is never a pass"} {
		if !strings.Contains(got, want) {
			t.Fatalf("instructions miss %q", want)
		}
	}
}

// TestValidationNamesStandIns: an unknown component type and an unreadable
// reference keep the configuration valid; the structured result names them
// and the text only counts them.
func TestValidationNamesStandIns(t *testing.T) {
	ctx, session, _ := newTestClient(t, platform.Options{})
	config := `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  geoip: {}
  attributes/secret:
    actions: [{key: token, value: "${file:/nonexistent/lab/token}", action: upsert}]
exporters: {debug: {}}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [geoip, attributes/secret], exporters: [debug]}
`
	for _, tool := range []string{"linea_validate_config", "linea_list_pipelines"} {
		r, out := callTool(t, ctx, session, tool, map[string]any{"config": map[string]any{"text": config}})
		if text := r.Content[0].(*sdk.TextContent).Text; !strings.Contains(text, "; 1 stand-in component; 1 stand-in reference.") {
			t.Fatalf("%s: text = %q", tool, text)
		}
		if out["valid"] != true {
			t.Fatalf("%s: %v", tool, out)
		}
		components, _ := out["stand_in_components"].([]any)
		if len(components) != 1 || components[0] != "processor/geoip" {
			t.Fatalf("%s: stand_in_components = %v", tool, out["stand_in_components"])
		}
		refs, _ := out["references"].([]any)
		if len(refs) != 1 || refs[0].(map[string]any)["source"] != "stand_in" || refs[0].(map[string]any)["target"] != "/nonexistent/lab/token" {
			t.Fatalf("%s: references = %v", tool, out["references"])
		}
	}
}
