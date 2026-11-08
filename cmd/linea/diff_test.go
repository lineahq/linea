package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// diffFiles writes the logs-cleanup configuration and an edit of it, and
// returns them with the sample's fixture and scenario.
func diffFiles(t *testing.T) (before, after, fixture, scenario string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	after = filepath.Join(t.TempDir(), "after.yaml")
	if err := os.WriteFile(after, []byte(strings.Replace(string(config), `"upper"`, `"lower"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "config.yaml"), after, filepath.Join(root, "input.json"), filepath.Join(root, "scenario.yaml")
}

func runDiff(t *testing.T, ctx context.Context, want int, args ...string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, append([]string{"diff"}, args...), &stdout, &stderr); code != want {
		t.Fatalf("diff %v: exit %d, want %d: %s", args, code, want, stderr.String())
	}
	return asSentByPath(t, stdout.Bytes(), args)
}

func TestDiffExitsWithTheOutcome(t *testing.T) {
	before, after, fixture, scenario := diffFiles(t)
	ctx := context.Background()
	out := runDiff(t, ctx, exitOK, "--before", before, "--after", before, "--fixture", fixture)
	if !strings.HasPrefix(string(out), "SAME  before ") {
		t.Fatalf("same: %s", out)
	}
	out = runDiff(t, ctx, exitMismatch, "--before", before, "--after", after, "--scenario", scenario)
	if !strings.HasPrefix(string(out), "DIFFERENT  before ") || !strings.Contains(string(out), `log.severity_text modified in 3 record(s): "WARNING" -> "warning"`) {
		t.Fatalf("different: %s", out)
	}
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("receivers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	runDiff(t, ctx, exitError, "--before", before, "--after", broken, "--fixture", fixture)
	runDiff(t, ctx, exitError, "--before", before, "--after", after, "--fixture", fixture, "--format", "html")
	runDiff(t, ctx, exitError, "--before", before, "--after", after)
	runDiff(t, ctx, exitError, "--runs", "a", "b")
	for outcome, code := range map[platform.ComparisonOutcome]int{platform.ComparisonSame: exitOK, platform.ComparisonDifferent: exitMismatch,
		platform.ComparisonInconclusive: exitInconclusive, "": exitError} {
		if got := comparisonExit(outcome); got != code {
			t.Errorf("exit for %q = %d, want %d", outcome, got, code)
		}
	}
}

// withoutIDs replaces the run IDs, in order of appearance, which are the
// only bytes that differ between two comparisons of the same configurations
// and input.
func withoutIDs(out []byte) []byte {
	seen := map[string]string{}
	return runIDPattern.ReplaceAllFunc(out, func(id []byte) []byte {
		if _, ok := seen[string(id)]; !ok {
			seen[string(id)] = fmt.Sprintf("RUN%d", len(seen)+1)
		}
		return []byte(seen[string(id)])
	})
}

var runIDPattern = regexp.MustCompile(`run-[0-9a-f]{16}`)

// A comparison reads the same locally, through a server's CLI, HTTP and MCP
// interfaces, in both modes.
func TestDiffParity(t *testing.T) {
	before, after, fixture, scenario := diffFiles(t)
	ctx, baseURL, _ := startMCPServe(t)
	session, err := sdk.NewClient(&sdk.Implementation{Name: "diff-parity", Version: "1"}, nil).
		Connect(ctx, mcpTransport(baseURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			input := []string{"--fixture", fixture}
			mcpArgs := map[string]any{"before": mcpFile(t, before), "after": mcpFile(t, after)}
			if mode == "quick" {
				raw, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				mcpArgs["input_json"] = string(raw)
			} else {
				input = []string{"--scenario", scenario}
				mcpArgs["scenario"] = mcpScenario(t, scenario)
			}
			args := append([]string{"--before", before, "--after", after}, input...)

			// Local and connected diff print the same bytes apart from run IDs.
			for _, format := range []string{"json", "markdown", "text"} {
				local := runDiff(t, ctx, exitMismatch, append(args, "--format", format)...)
				connected := runDiff(t, ctx, exitMismatch, append(args, "--format", format, "--server", baseURL)...)
				if l, c := withoutIDs(local), withoutIDs(connected); !bytes.Equal(l, c) {
					t.Fatalf("%s: local and connected differ:\n%s\n---\n%s", format, l, c)
				}
				// Each changed record is followed in both runs.
				if want := map[string]string{"json": `"lineage_id": "`, "markdown": "| Diverges at |", "text": "    before: otlp -> "}[format]; !bytes.Contains(local, []byte(want)) {
					t.Fatalf("%s lacks %q:\n%s", format, want, local)
				}
			}

			// One comparison read through MCP, HTTP and the CLI.
			var mcpOut struct {
				BeforeRun struct {
					RunID string `json:"run_id"`
				} `json:"before_run"`
				AfterRun struct {
					RunID string `json:"run_id"`
				} `json:"after_run"`
				Comparison json.RawMessage `json:"comparison"`
				Markdown   string          `json:"markdown"`
			}
			parityTool(t, ctx, session, "linea_compare_configs", mcpArgs, &mcpOut)
			a, b := mcpOut.BeforeRun.RunID, mcpOut.AfterRun.RunID
			compareURL := baseURL + "/api/v1/runs/" + a + "/compare/" + b
			httpJSON := parityHTTP(t, ctx, http.MethodGet, compareURL, nil, http.StatusOK)
			cliJSON := runDiff(t, ctx, exitMismatch, "--server", baseURL, "--runs", a, b, "--format", "json")
			if !bytes.Equal(asSentByPath(t, httpJSON, nil), cliJSON) {
				t.Fatalf("HTTP and CLI JSON differ:\n%s\n---\n%s", httpJSON, cliJSON)
			}
			var fromMCP, fromHTTP any
			if err := json.Unmarshal(mcpOut.Comparison, &fromMCP); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(httpJSON, &fromHTTP); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fromMCP, fromHTTP) {
				t.Fatalf("MCP and HTTP comparisons differ:\n%s\n---\n%s", mcpOut.Comparison, httpJSON)
			}
			httpMarkdown := parityHTTP(t, ctx, http.MethodGet, compareURL+"?format=markdown", nil, http.StatusOK)
			cliMarkdown := runDiff(t, ctx, exitMismatch, "--server", baseURL, "--runs", a, b, "--format", "markdown")
			if string(httpMarkdown) != mcpOut.Markdown || !bytes.Equal(httpMarkdown, cliMarkdown) {
				t.Fatalf("Markdown differs:\nHTTP %s\nMCP %s\nCLI %s", httpMarkdown, mcpOut.Markdown, cliMarkdown)
			}
		})
	}
}
