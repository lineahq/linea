package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/build"
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

// TestUpgradeRoutesMapPlatformOperations checks an upgrade from the
// built-in runtime to a built one over HTTP, as JSON and Markdown, and
// moves a manifest to another release.
func TestUpgradeRoutesMapPlatformOperations(t *testing.T) {
	store := &memoryRuntimes{images: map[string]platform.RuntimeInfo{}, start: func(info *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
		return engineWorker{id: "sha256:runtime-" + info.Name}, nil
	}}
	store.images["next"] = platform.RuntimeInfo{Name: "next", ImageID: "sha256:next", CollectorVersion: "0.163.0", Protocol: 2}
	service := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true}, Runtimes: store})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service})
	api := srv.URL + "/api/v1"

	req := UpgradeCheckRequest{To: "next", ConfigYAML: readText(t, multisignalConfig), Scenarios: []SuiteScenario{{ScenarioYAML: scenarioText(t, multisignalScenario)}}}
	c := decodeBody[platform.UpgradeCheck](t, post(t, api+"/upgrade-checks", req), http.StatusOK)
	if c.Verdict != platform.UpgradeCompatible || c.From.Name != platform.BuiltInRuntime || c.To.Name != "next" || c.To.Verified ||
		len(c.Scenarios) != 1 || c.Scenarios[0].Outcome != platform.ComparisonSame || !c.Scenarios[0].InputsIdentical {
		t.Fatalf("check = %+v", c)
	}
	resp := post(t, api+"/upgrade-checks?format=markdown", req)
	md, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(md), "# Upgrade check: compatible") ||
		!strings.Contains(string(md), "`0.163.0` (unverified)") {
		t.Fatalf("markdown %d: %s", resp.StatusCode, md)
	}
	same := decodeBody[OperationError](t, post(t, api+"/upgrade-checks", UpgradeCheckRequest{From: "next", To: "next", ConfigYAML: readText(t, multisignalConfig)}), http.StatusBadRequest)
	if same.Code != string(platform.CodeInvalidRequest) {
		t.Fatalf("same runtime = %+v", same)
	}
	decodeBody[OperationError](t, post(t, api+"/upgrade-checks", UpgradeCheckRequest{To: "gone", ConfigYAML: readText(t, multisignalConfig)}), http.StatusServiceUnavailable)
	decodeBody[Error](t, post(t, api+"/upgrade-checks?format=html", req), http.StatusBadRequest)

	m := decodeBody[RetargetedManifest](t, post(t, api+"/manifests/retarget", ManifestRetargetRequest{ManifestYAML: string(build.Manifest), Version: "0.160.0"}), http.StatusOK)
	if m.To != "0.160.0" || !strings.Contains(m.Manifest, "otelcol_version: 0.160.0") || len(m.Moved) == 0 || m.Missing == nil || m.Custom == nil {
		t.Fatalf("retargeted = %+v", m)
	}
	decodeBody[OperationError](t, post(t, api+"/manifests/retarget", ManifestRetargetRequest{ManifestYAML: string(build.Manifest), Version: "0.150.0"}), http.StatusNotFound)
	bad := decodeBody[OperationError](t, post(t, api+"/manifests/retarget", ManifestRetargetRequest{ManifestYAML: "receivers: [{gomod: x}]"}), http.StatusUnprocessableEntity)
	if len(bad.Diagnostics) != 1 || bad.Diagnostics[0].Code != "invalid_manifest" {
		t.Fatalf("invalid manifest = %+v", bad)
	}
}
