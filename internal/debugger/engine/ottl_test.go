package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
)

var (
	pagingConfig   = filepath.Join("..", "..", "..", "testdata", "paging", "config.yaml")
	pagingScenario = filepath.Join("..", "..", "..", "testdata", "paging", "scenario.yaml")
)

// TestPagingFixtureIsCorrectedThroughThePlayground follows the paging exit
// criterion headlessly: the scenario fails because a where clause never
// matches; the replay shows the statement matching nothing; evaluating the
// corrected statement matches the error log; and the corrected
// configuration passes.
func TestPagingFixtureIsCorrectedThroughThePlayground(t *testing.T) {
	ctx := context.Background()
	test := func(cfg string) assertion.Outcome {
		t.Helper()
		rep, err := Test(ctx, TestRequest{Config: cfg, Scenarios: []string{pagingScenario}})
		if err != nil {
			t.Fatal(err)
		}
		return rep.Outcome
	}
	if got := test(pagingConfig); got != assertion.Fail {
		t.Fatalf("fixture outcome = %s, want fail", got)
	}

	rep, err := Execute(ctx, Request{Config: pagingConfig, Scenario: pagingScenario})
	if err != nil {
		t.Fatal(err)
	}
	replay := ottlOf(t, rep, "transform/route")
	if replay.Verification != ottldebug.VerificationReplayVerified {
		t.Fatalf("replay verification = %s", replay.Verification)
	}
	if st := replay.Groups[0].Statements[0]; st.Matched != 0 || st.NotMatched != 2 {
		t.Fatalf("buggy statement matched %d, not matched %d; want 0 and 2", st.Matched, st.NotMatched)
	}

	raw, err := os.ReadFile(pagingConfig)
	if err != nil {
		t.Fatal(err)
	}
	fixed := strings.Replace(string(raw), `log.severity_text == "error"`, `log.severity_text == "ERROR"`, 1)
	logs, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "paging", "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := EvaluateOTTL(ctx, OTTLRequest{ConfigYAML: fixed, Processor: "transform/route", Input: logs})
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Valid || ev.Replay.Groups[0].Statements[0].Matched != 1 || !strings.Contains(string(ev.Output), `"pager"`) {
		t.Fatalf("evaluation of the fix = %+v", ev)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(fixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := test(path); got != assertion.Pass {
		t.Fatalf("corrected outcome = %s, want pass", got)
	}
}

func TestPreviewFollowsTheEnvironmentPolicy(t *testing.T) {
	t.Setenv(policyVariable, "private-host-value")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['set(log.attributes[\"bound\"], \"${env:" + policyVariable + "}\")']}]}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n"
	logs, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "paging", "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range policyCases {
		t.Run(tt.name, func(t *testing.T) {
			req := OTTLRequest{ConfigYAML: yaml, Processor: "transform", Input: logs}
			if tt.declared {
				req.ScenarioYAML = "version: 1\nenvironment: {" + policyVariable + ": declared-value}\ninputs: [{data: '{\"resourceLogs\": []}'}]\n"
			}
			ev, err := EvaluateOTTLWithEnvironment(context.Background(), req, tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			checkPolicyOutcome(t, tt.want, ev.Valid, ev.Diagnostics, ev.Output)
		})
	}
}
