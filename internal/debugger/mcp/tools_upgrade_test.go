package mcp

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// engineWorker is a runtime worker that is the in-process engine behind a
// bundle boundary, as a worker is.
type engineWorker struct{ id string }

func (w engineWorker) Target() engine.RuntimeTarget { return engine.RuntimeTarget{ID: w.id} }

func (w engineWorker) Execute(ctx context.Context, req engine.Request) (*engine.Report, error) {
	raw, _, err := engine.EncodeBundle(req)
	if err != nil {
		return nil, err
	}
	decoded, err := engine.DecodeBundleForRuntime(ctx, raw, w.id)
	if err != nil {
		return nil, err
	}
	decoded.RunID, decoded.Observer = req.RunID, req.Observer
	return engine.Execute(ctx, decoded)
}

func (w engineWorker) Inspect(ctx context.Context, in *engine.InspectionInput) (*engine.Inspection, error) {
	return engine.CompleteInspection(ctx, in)
}

func (w engineWorker) EvaluateOTTL(ctx context.Context, in *engine.OTTLInput) (*engine.OTTLEvaluation, error) {
	return engine.CompleteOTTL(ctx, in)
}

func (engineWorker) Close() error { return nil }

var upgradeToolText = regexp.MustCompile(`^Upgrade check: verdict (compatible|configuration_breaks|behavior_changes|inconclusive); configuration valid from (true|false), to (true|false); [0-9]+ new diagnostics?; [0-9]+ feature gate changes?(; [0-9]+ inconclusive reasons?)?; [0-9]+ scenarios?(, [0-9]+ (same|different|inconclusive|not compared))*\.$`)

// TestCompareVersionsToolChecksAnUpgrade checks a sample on the built-in
// runtime and a built one; the text names no runtime or version.
func TestCompareVersionsToolChecksAnUpgrade(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{}, start: func(info *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
		return engineWorker{id: "sha256:runtime-" + info.Name}, nil
	}}
	store.images["next"] = platform.RuntimeInfo{Name: "next", ImageID: "sha256:next", CollectorVersion: "0.163.0", Protocol: 2}
	ctx, session, _ := newTestClient(t, platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: store})

	r, out := callTool(t, ctx, session, "linea_compare_versions", map[string]any{"to": "next",
		"config": map[string]any{"text": readSample(t, "logs-cleanup", "config.yaml")}, "scenarios": []any{map[string]any{"text": sampleScenario(t, "logs-cleanup")}}})
	text := textOf(t, r)
	check, _ := out["check"].(map[string]any)
	if r.IsError || check["verdict"] != "compatible" || !strings.HasPrefix(out["markdown"].(string), "# Upgrade check: compatible") {
		t.Fatalf("check = %s, %+v", text, out)
	}
	if !upgradeToolText.MatchString(text) || !strings.Contains(text, "1 scenario, 1 same") || strings.Contains(text, "next") || strings.Contains(text, "0.163") {
		t.Fatalf("text = %s", text)
	}

	for _, tc := range []struct {
		args map[string]any
		code platform.Code
	}{
		{map[string]any{"config": map[string]any{"text": inspectionConfig}}, platform.CodeInvalidRequest},
		{map[string]any{"to": "built-in", "config": map[string]any{"text": inspectionConfig}}, platform.CodeInvalidRequest},
		{map[string]any{"to": "gone", "config": map[string]any{"text": inspectionConfig}}, platform.CodeUnavailable},
	} {
		r, out := callTool(t, ctx, session, "linea_compare_versions", tc.args)
		if !r.IsError || out["code"] != string(tc.code) {
			t.Errorf("%v = %+v, want %s", tc.args, out, tc.code)
		}
	}
}
