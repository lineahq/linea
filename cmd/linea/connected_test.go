package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/server"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// runServer runs the team server, as linea-server does, until it returns.
func runServer(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := server.Main(context.Background(), args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Connected mode prints what local mode prints, with the same exit codes, in
// both run modes.
func TestConnectedModeMatchesLocalOutput(t *testing.T) {
	// Serve's store bounds each run's evidence at the server config's run_limit; a local run's
	// bound is the platform's 64 MiB, which the report records; the store
	// keeps 1 MiB of a run's limit as headroom for the analysis.
	server := startServe(t, withRunLimit("65MiB"))
	dir := filepath.Join("..", "..", "samples")
	config := filepath.Join(dir, "logs-cleanup", "config.yaml")
	scenario := filepath.Join(dir, "logs-cleanup", "scenario.yaml")
	broken := filepath.Join(dir, "broken-routing")
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"inspect", "--config", config, "--format", "json"}, exitOK},
		{[]string{"inspect", "--config", config, "--scenario", scenario}, exitOK},
		{[]string{"run", "--config", config, "--fixture", filepath.Join(dir, "logs-cleanup", "input.json"), "--normalize"}, exitOK},
		{[]string{"run", "--config", config, "--scenario", scenario, "--normalize"}, exitOK},
		{[]string{"run", "--config", config, "--scenario", scenario, "--normalize", "--format", "text"}, exitOK},
		{[]string{"test", "--config", config, "--scenario", scenario, "--format", "json"}, exitOK},
		{[]string{"test", "--config", filepath.Join(broken, "config.yaml"), "--scenario", filepath.Join(broken, "scenario.yaml")}, exitFail},
	} {
		args := tc.args
		t.Run(args[0], func(t *testing.T) {
			localCode, local, localErr := runCLI(t, args...)
			code, connected, connectedErr := runCLI(t, append(args, "--server", server)...)
			if args[0] == "test" {
				// Durations differ between runs.
				local, connected = dropDurations(local), dropDurations(connected)
			}
			local = string(asSentByPath(t, []byte(local), args))
			connected = string(asSentByPath(t, []byte(connected), append(args, "--server")))
			if localCode != tc.want || local == "" || code != localCode || connected != local {
				t.Fatalf("%v: local exit %d (%s)\n%s\nconnected exit %d (%s)\n%s", args, localCode, localErr, local, code, connectedErr, connected)
			}
		})
	}
}

// The server reads no host file: the connected CLI sends a scenario's files
// inline, so it runs a scenario whose inputs only the client can reach.
func TestConnectedModeSendsScenarioFilesInline(t *testing.T) {
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	config, scenario := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	for _, args := range [][]string{
		{"inspect", "--config", config, "--scenario", scenario},
		{"run", "--config", config, "--scenario", scenario},
		{"test", "--config", config, "--scenario", scenario},
	} {
		if code, _, stderr := runCLI(t, append(args, "--server", server)...); code != exitOK {
			t.Fatalf("%v: exit %d: %s", args, code, stderr)
		}
	}
	missing := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(missing, []byte("version: 1\ninputs: [{file: absent.json}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, "run", "--config", config, "--scenario", missing, "--server", server); code != exitError || !strings.Contains(stderr, "inputs[0]") {
		t.Fatalf("missing input: exit %d: %s", code, stderr)
	}
}

// asSentByPath makes a command's output with --scenario comparable to one
// where the server read the scenario's files itself. A connected CLI sends
// the scenario with its files inlined: the server reports that text's hash
// under the same path, and the run's inputs digest, which covers the
// submitted bytes, differs (comparisons still find the inputs identical
// through their input identity). Digests are blanked on every side, and the
// lab's registered runtime reads as the built-in registry.
func asSentByPath(t *testing.T, out []byte, args []string) []byte {
	t.Helper()
	return labRuntimePattern.ReplaceAll(asSent(t, out, args), []byte(`"kind": "built_in",`))
}

// asSent is asSentByPath without the runtime.
func asSent(t *testing.T, out []byte, args []string) []byte {
	t.Helper()
	i := slices.Index(args, "--scenario")
	if i < 0 {
		return out
	}
	if slices.Contains(args, "--server") {
		out = bytes.ReplaceAll(out, []byte(inlinedSHA(t, args[i+1])), []byte(fileSHA(t, args[i+1])))
	}
	return withoutInputsDigests(out)
}

func withoutInputsDigests(out []byte) []byte {
	return inputsDigestPattern.ReplaceAll(out, []byte(`"inputs_digest": "sha256:INPUTS"`))
}

// labRuntimePattern matches the lab's runtime in a report. The lab runs on a
// registered runtime, which differs from the built-in registry of a direct
// command only in its kind and name.
var labRuntimePattern = regexp.MustCompile(`"kind": "remote",\s+"name": "lab",`)

var inputsDigestPattern = regexp.MustCompile(`"inputs_digest": "sha256:[0-9a-f]{64}"`)

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// mcpFile is an MCP source with a file's content, labelled with its path, as
// a client sends it: the server reads no host file.
func mcpFile(t *testing.T, path string) map[string]string {
	t.Helper()
	text, err := readArg("config", path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"text": text, "label": path}
}

// mcpScenario is mcpFile for a scenario, with the files it names inlined.
func mcpScenario(t *testing.T, path string) map[string]string {
	t.Helper()
	text, err := scenarioSource(path)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"text": text, "label": path}
}

func inlinedSHA(t *testing.T, path string) string {
	t.Helper()
	text, err := scenarioSource(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func dropDurations(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.Contains(line, `"duration_seconds"`) && !strings.Contains(line, `"run_id"`) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func TestConnectedModeRefusesNonLoopbackAndReportsADownServer(t *testing.T) {
	config := filepath.Join("..", "..", "samples", "logs-cleanup", "config.yaml")
	code, _, stderr := runCLI(t, "inspect", "--config", config, "--server", "http://192.0.2.1:7070")
	if code != exitError || !strings.Contains(stderr, "not a loopback host") {
		t.Fatalf("non-loopback: exit %d, %s", code, stderr)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	down := "http://" + l.Addr().String()
	l.Close()
	for _, args := range [][]string{
		{"inspect", "--config", config},
		{"run", "--config", config, "--fixture", filepath.Join("..", "..", "samples", "logs-cleanup", "input.json")},
		{"test", "--config", config, "--scenario", filepath.Join("..", "..", "samples", "logs-cleanup", "scenario.yaml")},
	} {
		code, _, stderr := runCLI(t, append(args, "--server", down)...)
		if code != exitError || !strings.Contains(stderr, "cannot reach the debugger server at "+down+" (is linea-server running?)") {
			t.Fatalf("%s against a down server: exit %d, %s", args[0], code, stderr)
		}
	}
	code, _, stderr = runCLI(t, "run", "--config", config, "--fixture", "x.json", "--verbose", "--server", down)
	if code != exitError || !strings.Contains(stderr, "cannot be used with --server") {
		t.Fatalf("--verbose: exit %d, %s", code, stderr)
	}
}

// export writes the server's file on the client and never overwrites one.
func TestExportWritesTheScenarioFileOnce(t *testing.T) {
	server := startServe(t)
	dir := filepath.Join("..", "..", "samples", "logs-cleanup")
	code, out, stderr := runCLI(t, "test", "--config", filepath.Join(dir, "config.yaml"), "--scenario", filepath.Join(dir, "scenario.yaml"), "--format", "json", "--server", server)
	if code != exitOK {
		t.Fatalf("test: exit %d, %s", code, stderr)
	}
	var rep struct {
		Scenarios []struct {
			RunID string `json:"run_id"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Scenarios) != 1 || rep.Scenarios[0].RunID == "" {
		t.Fatalf("report %s: %v", out, err)
	}
	id := rep.Scenarios[0].RunID
	file := filepath.Join(t.TempDir(), "exported.yaml")
	code, out, stderr = runCLI(t, "export", "--server", server, "--run", id, "--out", file)
	if code != exitOK || !strings.Contains(out, "wrote "+file+" from run "+id) || !strings.Contains(out, "recorded telemetry") {
		t.Fatalf("export: exit %d, %s%s", code, out, stderr)
	}
	written, err := os.ReadFile(file)
	if err != nil || !strings.HasPrefix(string(written), "# Scenario exported from linea run "+id+".") || !strings.Contains(string(written), "name: logs-cleanup") {
		t.Fatalf("exported file %q: %v", written, err)
	}
	code, _, stderr = runCLI(t, "export", "--server", server, "--run", id, "--out", file)
	if code != exitError || !strings.Contains(stderr, "never overwrites") {
		t.Fatalf("second export: exit %d, %s", code, stderr)
	}
	if again, _ := os.ReadFile(file); string(again) != string(written) {
		t.Fatal("the existing file changed")
	}
	code, _, stderr = runCLI(t, "export", "--server", server, "--run", "missing", "--out", filepath.Join(t.TempDir(), "x.yaml"))
	if code != exitError || !strings.Contains(stderr, "404") {
		t.Fatalf("unknown run: exit %d, %s", code, stderr)
	}
	if code, _, _ = runCLI(t, "export", "--run", id, "--out", file+".2"); code != exitError {
		t.Fatalf("export without --server: exit %d", code)
	}
}
