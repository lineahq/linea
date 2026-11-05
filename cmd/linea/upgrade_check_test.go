package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/release"
)

// engineRuntimes is a runtime store holding one runtime, "next", whose
// worker is the in-process engine behind a bundle boundary, as a worker is.
type engineRuntimes struct{}

var nextRuntime = platform.RuntimeInfo{Name: "next", ImageID: "sha256:next", CollectorVersion: release.CollectorVersion, Protocol: 2, Verified: true}

func (engineRuntimes) Check(platform.RuntimeBuildSpec) error { return errors.New("unused") }

func (engineRuntimes) Build(context.Context, platform.RuntimeBuildSpec, func(string)) (*platform.RuntimeInfo, error) {
	return nil, errors.New("unused")
}

func (engineRuntimes) List(context.Context) ([]platform.RuntimeInfo, error) {
	return []platform.RuntimeInfo{nextRuntime}, nil
}

func (engineRuntimes) Get(_ context.Context, ref string) (*platform.RuntimeInfo, error) {
	if ref != nextRuntime.Name {
		return nil, platform.ErrRuntimeNotFound
	}
	info := nextRuntime
	return &info, nil
}

func (engineRuntimes) Delete(context.Context, *platform.RuntimeInfo) error {
	return errors.New("unused")
}

func (engineRuntimes) Push(context.Context, *platform.RuntimeInfo, string) error {
	return errors.New("unused")
}

func (engineRuntimes) Import(context.Context, string, bool, func(*platform.RuntimeInfo) error) (*platform.RuntimeInfo, error) {
	return nil, platform.ErrRuntimeNotFound
}

func (engineRuntimes) Start(_ context.Context, info *platform.RuntimeInfo) (platform.RuntimeWorker, error) {
	return engineWorker{id: "sha256:runtime-" + info.Name}, nil
}

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

func runUpgradeCheck(t *testing.T, ctx context.Context, want int, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, append([]string{"upgrade-check"}, args...), &stdout, &stderr); code != want {
		t.Fatalf("upgrade-check %v: exit %d, want %d: %s", args, code, want, stderr.String())
	}
	return asSent(t, stdout.Bytes(), args)
}

// TestUpgradeCheckParity checks one upgrade through the CLI, locally and
// connected, HTTP and MCP, and gets the same check from each.
func TestUpgradeCheckParity(t *testing.T) {
	saved := dockerRuntimes
	dockerRuntimes = func() platform.RuntimeStore { return engineRuntimes{} }
	t.Cleanup(func() { dockerRuntimes = saved })
	config, _, _, scenario := diffFiles(t)
	ctx, baseURL, _ := startMCPServe(t, withRuntimes("next"))
	args := []string{"--config", config, "--to", "next", "--scenario", scenario}

	for _, format := range []string{"json", "markdown", "text"} {
		local := runUpgradeCheck(t, ctx, exitOK, append(args, "--format", format)...)
		connected := upgradeAsLocal(runUpgradeCheck(t, ctx, exitOK, append(args, "--format", format, "--server", baseURL)...))
		if l, c := upgradeWithoutRuntimeIdentity(withoutIDs(local)), upgradeWithoutRuntimeIdentity(withoutIDs(connected)); !bytes.Equal(l, c) {
			t.Fatalf("%s: local and connected differ:\n%s\n---\n%s", format, l, c)
		}
		if format == "text" && !strings.HasPrefix(string(local), "upgrade check: compatible\n") {
			t.Fatalf("text:\n%s", local)
		}
	}
	cliJSON := withoutIDs(runUpgradeCheck(t, ctx, exitOK, append(args, "--format", "json", "--server", baseURL)...))
	httpRaw := parityHTTP(t, ctx, http.MethodPost, baseURL+"/api/v1/upgrade-checks",
		api.UpgradeCheckRequest{To: "next", ConfigYAML: mcpFile(t, config)["text"], ConfigLabel: config,
			Scenarios: []api.SuiteScenario{{ScenarioYAML: mcpScenario(t, scenario)["text"], ScenarioLabel: scenario}}}, http.StatusOK)
	httpJSON := withoutInputsDigests(withoutIDs(httpRaw))
	if !bytes.Equal(cliJSON, httpJSON) {
		t.Fatalf("CLI and HTTP differ:\n%s\n---\n%s", cliJSON, httpJSON)
	}
	var check platform.UpgradeCheck
	if err := json.Unmarshal(httpJSON, &check); err != nil {
		t.Fatal(err)
	}
	if check.Verdict != platform.UpgradeCompatible || check.To.CollectorVersion != release.CollectorVersion || len(check.Scenarios) != 1 ||
		!check.Scenarios[0].InputsIdentical || check.Scenarios[0].Name != scenario {
		t.Fatalf("check = %+v", check)
	}

	session, err := sdk.NewClient(&sdk.Implementation{Name: "upgrade-parity", Version: "1"}, nil).
		Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	var mcpOut struct {
		Check    json.RawMessage `json:"check"`
		Markdown string          `json:"markdown"`
	}
	parityTool(t, ctx, session, "linea_compare_versions", map[string]any{"to": "next", "config": mcpFile(t, config),
		"scenarios": []any{mcpScenario(t, scenario)}}, &mcpOut)
	var fromMCP, fromHTTP any
	// MCP orders object keys differently, so run IDs are blanked rather
	// than numbered by appearance.
	blank := func(raw []byte) []byte { return runIDPattern.ReplaceAll(raw, []byte("RUN")) }
	if err := json.Unmarshal(blank(mcpOut.Check), &fromMCP); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blank(httpRaw), &fromHTTP); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromMCP, fromHTTP) {
		t.Fatalf("MCP and HTTP checks differ:\n%s\n---\n%s", mcpOut.Check, httpJSON)
	}
	cliMarkdown := withoutIDs(runUpgradeCheck(t, ctx, exitOK, append(args, "--format", "markdown", "--server", baseURL)...))
	if string(withoutIDs([]byte(mcpOut.Markdown))) != string(cliMarkdown) {
		t.Fatalf("Markdown differs:\nMCP %s\nCLI %s", mcpOut.Markdown, cliMarkdown)
	}

	// Refusals are errors, not verdicts.
	runUpgradeCheck(t, ctx, exitError, "--config", config, "--to", "built-in")
	runUpgradeCheck(t, ctx, exitError, "--config", config, "--to", "gone", "--server", baseURL)
	runUpgradeCheck(t, ctx, exitError, "--config", config)
	runUpgradeCheck(t, ctx, exitError, "--config", config, "--to", "next", "--scenario", scenario, "--tests", filepath.Dir(scenario))
}

func TestUpgradeExitFollowsTheVerdict(t *testing.T) {
	for v, want := range map[platform.UpgradeVerdict]int{
		platform.UpgradeCompatible: exitOK, platform.UpgradeConfigurationBreaks: exitMismatch,
		platform.UpgradeBehaviorChanges: exitMismatch, platform.UpgradeInconclusive: exitInconclusive, "": exitError,
	} {
		if got := upgradeExit(v); got != want {
			t.Errorf("%q: exit %d, want %d", v, got, want)
		}
	}
}

// TestRuntimeRetargetMovesAManifest moves the built-in manifest to the
// oldest release into a new file, never over one.
func TestRuntimeRetargetMovesAManifest(t *testing.T) {
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.yaml"), filepath.Join(dir, "out.yaml")
	if err := os.WriteFile(in, build.Manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	args := []string{"runtime", "retarget", "--manifest", in, "--version", "0.160.0", "--out", out}
	if code := run(context.Background(), args, &stdout, &stderr); code != exitOK || !strings.Contains(stderr.String(), "to 0.160.0") {
		t.Fatalf("retarget: exit %d, stderr %s", code, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(data), "otelcol_version: 0.160.0") {
		t.Fatalf("manifest = %.200s, %v", data, err)
	}
	stderr.Reset()
	if code := run(context.Background(), args, &stdout, &stderr); code != exitError || !strings.Contains(stderr.String(), "never overwrites") {
		t.Fatalf("second write: exit %d, stderr %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"runtime", "retarget", "--version", "0.160.0"}, &stdout, &stderr); code != exitError {
		t.Fatalf("without --manifest: exit %d", code)
	}
}

// upgradeAsLocal reads a lab's upgrade check as the direct command prints
// it: the lab's own runtime is the built-in registry, and the registered
// "next" runtime is the container runtime a direct command starts.
func upgradeAsLocal(out []byte) []byte {
	out = labRuntimePattern.ReplaceAll(out, []byte(`"kind": "built_in",`))
	out = regexp.MustCompile(`"kind": "remote",(\s+)"name": "next",`).ReplaceAll(out, []byte(`"kind": "container",${1}"name": "next",`))
	out = bytes.ReplaceAll(out, []byte("`lab`"), []byte("`built-in`"))
	out = bytes.ReplaceAll(out, []byte("from lab:"), []byte("from built-in:"))
	out = bytes.ReplaceAll(out, []byte("feature gates: the from runtime does not list them\n"), nil)
	out = bytes.ReplaceAll(out, []byte("the from and to runtime does"), []byte("the to runtime does"))
	return bytes.ReplaceAll(out, []byte(`"name": "lab"`), []byte(`"name": "built-in"`))
}

// upgradeWithoutRuntimeIdentity drops what only a runtime's kind decides: a
// container runtime's image and manifest digests, and which side's feature
// gates a check could not read.
func upgradeWithoutRuntimeIdentity(out []byte) []byte {
	out = regexp.MustCompile(`\n\s+"(image_id|manifest_digest)": "[^"]*",`).ReplaceAll(out, nil)
	return regexp.MustCompile(`"unknown": \[(\s+"(from|to)",?)+\s+\]`).ReplaceAll(out, []byte(`"unknown": []`))
}
