package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// TestAnUpgradeBetweenBuiltReleasesAgreesEverywhere walks the release check:
// a manifest derived from a sample's configuration at 0.160.0 is
// moved to 0.162.0, both are built offline from the warm cache, and one
// upgrade check runs through the CLI, locally and connected, HTTP and MCP
// with the same answer, its scenario on identical inputs. It needs
// LINEA_DOCKER_TESTS=1 and the modules of both releases in the
// cache volume (the release Docker tests download them).
func TestAnUpgradeBetweenBuiltReleasesAgreesEverywhere(t *testing.T) {
	if os.Getenv("LINEA_DOCKER_TESTS") != "1" {
		t.Skip("set LINEA_DOCKER_TESTS=1 to build runtimes in Docker")
	}
	raw, err := workersrc.Pack(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	src, err := workersrc.Unpack(raw)
	if err != nil {
		t.Fatal(err)
	}
	saved := dockerRuntimes
	// Kept small for developer machines; a configuration's components fit.
	dockerRuntimes = func() platform.RuntimeStore {
		return platform.DockerRuntimes(platform.DockerOptions{Source: src, BuildMemory: 3 << 30, BuildCPUs: 2})
	}
	t.Cleanup(func() { dockerRuntimes = saved })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	root, err := filepath.Abs(filepath.Join("..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	config, scenario := filepath.Join(root, "config.yaml"), filepath.Join(root, "scenario.yaml")
	dir := t.TempDir()
	m160, m162 := filepath.Join(dir, "m160.yaml"), filepath.Join(dir, "m162.yaml")
	parityCLI(t, ctx, []string{"runtime", "manifest", "--version", "0.160.0", "--config", config, "--out", m160})
	parityCLI(t, ctx, []string{"runtime", "retarget", "--manifest", m160, "--version", "0.162.0", "--out", m162})
	for name, manifest := range map[string]string{"up-160": m160, "up-162": m162} {
		parityCLI(t, ctx, []string{"runtime", "build", "--name", name, "--manifest", manifest, "--offline"})
		t.Cleanup(func() {
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{"runtime", "rm", name}, &stdout, &stderr); code != exitOK {
				t.Errorf("removing %s: %s", name, stderr.String())
			}
		})
	}

	args := []string{"--config", config, "--from", "up-160", "--to", "up-162", "--scenario", scenario}
	local := withoutIDs(upgradeVerdict(t, ctx, append(args, "--format", "json")...))
	// The team server runs each image runtime on a worker started from it.
	l := startLab(t)
	baseURL := l.URL
	for _, name := range []string{"up-160", "up-162"} {
		parityCLI(t, ctx, []string{"runtime", "register", container.Ref(name), "--name", name, "--server", baseURL})
		startImageWorker(t, l, container.Ref(name), name)
	}
	connected := withoutIDs(upgradeVerdict(t, ctx, append(args, "--format", "json", "--server", baseURL)...))
	// Locally each side runs in a container, on the server on a connected
	// worker, which received the scenario with its files inlined: the
	// verdict, gates, diagnostics and runs agree apart from where they ran.
	if l, c := wherever(t, local), wherever(t, connected); !bytes.Equal(l, c) {
		t.Fatalf("local and connected differ:\n%s\n---\n%s", l, c)
	}
	httpRaw := parityHTTP(t, ctx, http.MethodPost, baseURL+"/api/v1/upgrade-checks", api.UpgradeCheckRequest{
		From: "up-160", To: "up-162", ConfigYAML: mcpFile(t, config)["text"], ConfigLabel: config,
		Scenarios: []api.SuiteScenario{{ScenarioYAML: mcpScenario(t, scenario)["text"], ScenarioLabel: scenario}}}, http.StatusOK)
	if !bytes.Equal(connected, withoutIDs(httpRaw)) {
		t.Fatalf("connected CLI and HTTP differ:\n%s\n---\n%s", connected, withoutIDs(httpRaw))
	}
	var check platform.UpgradeCheck
	if err := json.Unmarshal(httpRaw, &check); err != nil {
		t.Fatal(err)
	}
	if check.From.CollectorVersion != "0.160.0" || check.To.CollectorVersion != "0.162.0" || !check.From.Verified || !check.To.Verified {
		t.Fatalf("sides = %+v, %+v", check.From, check.To)
	}
	if len(check.FeatureGates.Unknown) != 0 || !check.Configuration.FromValid || !check.Configuration.ToValid ||
		len(check.Scenarios) != 1 || !check.Scenarios[0].InputsIdentical {
		t.Fatalf("check = %s", httpRaw)
	}
	// The configuration reads no files: the filelog receiver behind file
	// mocks is the debugger's own, and its gates are not the check's.
	var gates []string
	for _, g := range check.FeatureGates.Changes {
		gates = append(gates, g.ID+"@"+g.Component)
		if strings.HasPrefix(g.ID, "filelog.") || strings.HasPrefix(g.ID, "stanza.") {
			t.Errorf("gate %s of the debugger's filelog receiver listed", g.ID)
		}
	}
	t.Logf("verdict %s, gate changes %v, %d new diagnostic(s), scenario %s",
		check.Verdict, gates, len(check.Configuration.Added), check.Scenarios[0].Outcome)

	session, err := sdk.NewClient(&sdk.Implementation{Name: "upgrade-docker", Version: "1"}, nil).
		Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	var mcpOut struct {
		Check    json.RawMessage `json:"check"`
		Markdown string          `json:"markdown"`
	}
	parityTool(t, ctx, session, "linea_compare_versions", map[string]any{"from": "up-160", "to": "up-162",
		"config": mcpFile(t, config), "scenarios": []any{mcpScenario(t, scenario)}}, &mcpOut)
	blank := func(raw []byte) []byte { return runIDPattern.ReplaceAll(raw, []byte("RUN")) }
	var fromMCP, fromHTTP any
	if err := json.Unmarshal(blank(mcpOut.Check), &fromMCP); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blank(httpRaw), &fromHTTP); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromMCP, fromHTTP) {
		t.Fatalf("MCP and HTTP checks differ:\n%s\n---\n%s", mcpOut.Check, httpRaw)
	}
	if !strings.Contains(mcpOut.Markdown, "0.160.0") || !strings.Contains(mcpOut.Markdown, "0.162.0") {
		t.Fatalf("markdown:\n%s", mcpOut.Markdown)
	}
}

// wherever re-encodes an upgrade check without what depends on where its
// runs ran: each run's runtime placement and the digests of its inputs.
func wherever(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			delete(v, "inputs_digest")
			if rt, ok := v["runtime"].(map[string]any); ok {
				for _, k := range []string{"kind", "image_id", "manifest_digest", "module_set"} {
					delete(rt, k)
				}
			}
			for _, e := range v {
				walk(e)
			}
		case []any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	walk(v)
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// upgradeVerdict runs upgrade-check and accepts any verdict's exit code.
func upgradeVerdict(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(ctx, append([]string{"upgrade-check"}, args...), &stdout, &stderr)
	if code == exitError || code != upgradeExit(platform.UpgradeVerdict(verdictOf(t, stdout.Bytes()))) {
		t.Fatalf("upgrade-check %v: exit %d: %s", args, code, stderr.String())
	}
	return stdout.Bytes()
}

func verdictOf(t *testing.T, raw []byte) string {
	t.Helper()
	var v struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return v.Verdict
}
