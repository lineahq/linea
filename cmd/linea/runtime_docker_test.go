package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/container"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/workersrc"
)

// TestABuiltRuntimeWorksThroughEveryInterface builds the built-in manifest
// as a runtime in Docker, then runs one sample on it through the CLI, HTTP
// and MCP, quick and scenario: all three give the same evidence, which is
// the built-in runtime's apart from the report's runtime. It needs
// LINEA_DOCKER_TESTS=1, like the container package's Docker tests.
func TestABuiltRuntimeWorksThroughEveryInterface(t *testing.T) {
	if os.Getenv("LINEA_DOCKER_TESTS") != "1" {
		t.Skip("set LINEA_DOCKER_TESTS=1 to build runtimes in Docker")
	}
	const name = "v5-parity"
	raw, err := workersrc.Pack(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	src, err := workersrc.Unpack(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Kept small for developer machines; the built-in components fit.
	builder := platform.New(platform.Options{Paths: platform.PathPolicy{Unrestricted: true},
		Runtimes: platform.DockerRuntimes(platform.DockerOptions{Source: src, BuildMemory: 3 << 30, BuildCPUs: 2})})
	t.Cleanup(builder.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	manifest, err := filepath.Abs(filepath.Join("..", "..", "build", "linea-runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := builder.BuildRuntime(ctx, cliCaller, platform.BuildRuntimeRequest{Name: name, Manifest: platform.Source{Path: manifest}})
	if err != nil {
		t.Fatal(err)
	}
	if b, err = builder.WaitRuntimeBuild(ctx, cliCaller, b.ID, 0); err != nil || b.State != platform.BuildDone {
		t.Fatalf("build: %v, %+v", err, b)
	}
	t.Cleanup(func() {
		if err := builder.DeleteRuntime(context.Background(), cliCaller, name); err != nil {
			t.Errorf("removing the runtime: %v", err)
		}
	})

	root, err := filepath.Abs(filepath.Join("..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.yaml")
	fixturePath := filepath.Join(root, "input.json")
	scenarioPath := filepath.Join(root, "scenario.yaml")
	input, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	// A team server runs an image runtime on workers started from its
	// image: register the image, then start its worker dialing the lab.
	l := startLab(t)
	baseURL := l.URL
	if code, out, stderr := runCLI(t, "runtime", "register", container.Ref(name), "--name", name, "--server", baseURL); code != exitOK {
		t.Fatalf("register: exit %d, %s%s", code, out, stderr)
	}
	startImageWorker(t, l, container.Ref(name), name)
	ctx, cancelRuns := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancelRuns)
	session, err := sdk.NewClient(&sdk.Implementation{Name: "runtime-test", Version: "1"}, nil).
		Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			cliArgs := []string{"run", "--config", configPath}
			httpArgs := api.Request{ConfigYAML: mcpFile(t, configPath)["text"], ConfigLabel: configPath, Runtime: name}
			mcpArgs := map[string]any{"config": mcpFile(t, configPath), "runtime": name}
			fixtureSHA := ""
			if mode == "quick" {
				cliArgs = append(cliArgs, "--fixture", fixturePath)
				httpArgs.FixtureJSON = string(input)
				mcpArgs["input_json"] = string(input)
				fixtureSHA = fmt.Sprintf("%x", sha256.Sum256(input))
			} else {
				cliArgs = append(cliArgs, "--scenario", scenarioPath)
				httpArgs.ScenarioYAML, httpArgs.ScenarioLabel = mcpScenario(t, scenarioPath)["text"], scenarioPath
				mcpArgs["scenario"] = mcpScenario(t, scenarioPath)
			}
			builtIn := parityCLI(t, ctx, cliArgs)
			cli := parityCLI(t, ctx, append(cliArgs, "--runtime", name))

			var created api.Run
			decodeParityJSON(t, parityHTTP(t, ctx, http.MethodPost, baseURL+"/api/v1/runs", httpArgs, http.StatusAccepted), &created)
			viaHTTP := parityHTTPReport(t, ctx, baseURL, created.ID)
			var mcpRun parityRun
			parityTool(t, ctx, session, "linea_run", mcpArgs, &mcpRun)
			viaMCP := parityHTTPReport(t, ctx, baseURL, mcpRun.RunID)

			rt := decodeParityReport(t, cli).Runtime
			if rt == nil || rt.Kind != engine.RuntimeContainer || rt.Name != name || rt.ImageID == "" || rt.ManifestDigest == "" {
				t.Fatalf("CLI report runtime = %+v", rt)
			}
			// The server ran it on the image's worker, which it names.
			for via, raw := range map[string][]byte{"HTTP": viaHTTP, "MCP": viaMCP} {
				if rt := decodeParityReport(t, raw).Runtime; rt == nil || rt.Kind != engine.RuntimeRemote || rt.Name != name {
					t.Fatalf("%s report runtime = %+v", via, rt)
				}
			}
			want := withoutRuntime(t, normalizeParityReport(t, cli, fixtureSHA))
			// The server got the scenario with its files inlined, as a
			// connected CLI sends it, and keeps evidence within its store's
			// per-run allowance; the rest is compared byte for byte.
			server := func(raw []byte) []byte {
				sent := asSent(t, raw, append(append([]string{}, cliArgs...), "--server"))
				return withoutRuntime(t, normalizeParityReport(t, withCLIEvidenceBudget(t, sent, cli), fixtureSHA))
			}
			assertParityBytes(t, "CLI / HTTP on the runtime", withoutInputsDigests(want), withoutInputsDigests(server(viaHTTP)))
			assertParityBytes(t, "CLI / MCP on the runtime", withoutInputsDigests(want), withoutInputsDigests(server(viaMCP)))
			assertParityBytes(t, "built-in / runtime apart from the runtime",
				withoutRuntime(t, normalizeParityReport(t, builtIn, fixtureSHA)), want)
		})
	}
}

// startImageWorker runs a runtime image's worker in a container that dials
// the lab with its registration token, as a deployment's runtime workers
// do, and waits until the lab confirms the runtime. The container is
// removed when the test ends. Docker Desktop reaches the host's loopback as
// host.docker.internal; elsewhere the container shares the host network.
func startImageWorker(t *testing.T, l lab, image, name string) {
	t.Helper()
	u, err := url.Parse(l.URL)
	if err != nil {
		t.Fatal(err)
	}
	network := []string{"--network", "host"}
	if goruntime.GOOS != "linux" {
		network = nil
		u.Host = "host.docker.internal:" + u.Port()
	}
	workerName := "linea-parity-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := append([]string{"run", "--detach", "--rm", "--name", workerName, "--memory", "1g", "--cpus", "2",
		// The token reaches the container from this process's environment,
		// never from its command line.
		"--env", workerTokenEnv}, network...)
	args = append(args, image, "--connect", u.String(), "--name", name, "--capacity", "2")
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), workerTokenEnv+"="+l.WorkerToken)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("starting the runtime's worker: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "40", workerName).CombinedOutput()
			t.Logf("runtime worker logs:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "--force", workerName).Run()
	})
	deadline := time.Now().Add(2 * time.Minute)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for ; ; <-tick.C {
		code, out, _ := runCLI(t, "runtime", "list", "--format", "json", "--server", l.URL)
		var list []api.Runtime
		if code == exitOK && json.Unmarshal([]byte(out), &list) == nil {
			for _, rt := range list {
				if rt.Name == name && rt.Catalog != nil && rt.Catalog.ConfirmedAt != nil && rt.Catalog.Workers > 0 {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", "--tail", "40", workerName).CombinedOutput()
			t.Fatalf("the runtime's worker did not connect; its logs:\n%s", logs)
		}
	}
}

// workerTokenEnv is the variable linea-worker reads its registration token
// from (cmd/linea-worker).
const workerTokenEnv = "LINEA_WORKER_TOKEN"

// withoutRuntime re-encodes a report without its runtime.
func withoutRuntime(t *testing.T, raw []byte) []byte {
	t.Helper()
	report := decodeParityReport(t, raw)
	report.Runtime = nil
	var b bytes.Buffer
	if err := engine.Encode(&b, report); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
