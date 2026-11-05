package api

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// A comparison is submitted as two runs and read back once both are done, as
// JSON or Markdown; failures map through the platform's codes.
func TestComparisonRoutesSubmitAndCompare(t *testing.T) {
	_, srv := newServer(t)
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(config), `"upper"`, `"lower"`, 1)
	req := ComparisonRequest{Before: ComparisonConfig{ConfigYAML: readText(t, filepath.Join(dir, "config.yaml")), ConfigLabel: filepath.Join(dir, "config.yaml")},
		After: ComparisonConfig{ConfigYAML: edited, ConfigLabel: "edited.yaml"}, ScenarioYAML: scenarioText(t, filepath.Join(dir, "scenario.yaml"))}
	runs := decodeBody[ComparisonRuns](t, post(t, srv.URL+"/api/v1/comparisons", req), http.StatusAccepted)
	if runs.Before.ID == "" || runs.After.ID == "" || runs.Before.ID == runs.After.ID {
		t.Fatalf("runs = %+v", runs)
	}
	compare := srv.URL + "/api/v1/runs/" + runs.Before.ID + "/compare/" + runs.After.ID
	c := decodeBody[platform.Comparison](t, get(t, compare+"?wait=true&ignore="+url.QueryEscape(`log.attributes["x"]`)), http.StatusOK)
	if c.Outcome != platform.ComparisonDifferent || !c.InputsIdentical || c.Before.ID != runs.Before.ID || len(c.Ignored) != 1 || c.Ignored[0].Rule != `log.attributes["x"]` {
		t.Fatalf("comparison = %+v", c)
	}

	resp := get(t, compare+"?format=markdown")
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/markdown; charset=utf-8" ||
		!strings.HasPrefix(string(raw), "# Run comparison: different\n") {
		t.Fatalf("markdown: %d %s %q, %v", resp.StatusCode, resp.Header.Get("Content-Type"), raw, err)
	}

	for _, tc := range []struct {
		url    string
		status int
	}{
		{compare + "?format=html", http.StatusBadRequest},
		{compare + "?ignore=body", http.StatusBadRequest},
		{srv.URL + "/api/v1/runs/" + runs.Before.ID + "/compare/missing", http.StatusNotFound},
	} {
		decodeBody[Error](t, get(t, tc.url), tc.status)
	}
	// Neither configuration is admitted when the after one does not validate.
	before := len(listRunsHTTP(t, srv.URL))
	req.After = ComparisonConfig{ConfigYAML: "receivers: [", ConfigLabel: "broken.yaml"}
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/comparisons", req), http.StatusUnprocessableEntity)
	req.After, req.FixtureJSON = ComparisonConfig{ConfigYAML: edited}, `{"resourceLogs":[]}`
	decodeBody[Error](t, post(t, srv.URL+"/api/v1/comparisons", req), http.StatusBadRequest)
	if after := len(listRunsHTTP(t, srv.URL)); after != before {
		t.Fatalf("refused comparisons admitted %d runs", after-before)
	}
}

func listRunsHTTP(t *testing.T, base string) []Run {
	t.Helper()
	return decodeBody[RunPage](t, get(t, base+"/api/v1/runs"), http.StatusOK).Runs
}
