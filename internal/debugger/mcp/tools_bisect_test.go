package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/fleetcapture/fleetcapturetest"
	"github.com/lineahq/linea/internal/debugger/platform"
)

var bisectToolText = regexp.MustCompile(`^Config bisect: verdict (found|no_regression|already_bad|inconclusive); versions [0-9]+ to [0-9]+; [0-9]+ probes?(; first bad version (between )?[0-9]+(,| and) (last good )?[0-9]+)?(; [0-9]+ inconclusive reasons?)?\.$`)

// TestBisectConfigToolFindsTheVersion bisects a fleet config whose third of
// four versions stops dropping the health check; the text names no config.
func TestBisectConfigToolFindsTheVersion(t *testing.T) {
	sample, err := filepath.Abs(filepath.Join("..", "..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	good := string(raw)
	bad := strings.Replace(good, "filter/health, ", "", 1)
	fleet := fleetcapturetest.NewServer(t)
	fleet.ManageConfig(good, good, bad, bad)
	ctx, session, _ := newTestClient(t, platform.Options{Paths: platform.PathPolicy{Unrestricted: true},
		Capture: platform.CaptureOptions{Fleet: platform.FleetOptions{Endpoint: fleet.URL, Token: fleetcapturetest.Token}}})

	r, out := callTool(t, ctx, session, "linea_bisect_config", map[string]any{"agent": fleetcapturetest.Peer.AgentID,
		"scenarios": []any{map[string]any{"text": sampleScenario(t, "logs-cleanup")}}})
	text := textOf(t, r)
	b, _ := out["bisection"].(map[string]any)
	if r.IsError || b["verdict"] != "found" || !strings.HasPrefix(out["markdown"].(string), "# Config bisect: found") {
		t.Fatalf("bisection = %s, %+v", text, out)
	}
	if !bisectToolText.MatchString(text) || !strings.Contains(text, "first bad version 3, last good 2") || strings.Contains(text, "Pipeline") {
		t.Fatalf("text = %s", text)
	}

	r, out = callTool(t, ctx, session, "linea_bisect_config", map[string]any{"agent": fleetcapturetest.Peer.AgentID})
	if !r.IsError || out["code"] != string(platform.CodeInvalidRequest) {
		t.Fatalf("without scenarios = %+v", out)
	}
}
