package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

var logsCleanup = filepath.Join("..", "..", "..", "samples", "logs-cleanup")

// TestDeclaredTimeoutsAboveThePolicyAreClamped: a declared execution.timeout
// lowers the policy's deadline and never raises it; a clamp is recorded in
// the report, while the bundle records the policy itself.
func TestDeclaredTimeoutsAboveThePolicyAreClamped(t *testing.T) {
	config := filepath.Join(logsCleanup, "config.yaml")
	input, err := os.ReadFile(filepath.Join(logsCleanup, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	inline := strings.ReplaceAll(strings.TrimSpace(string(input)), "\n", " ")
	scenarioWith := func(timeout string) string {
		return "version: 1\nname: clamp\nexecution: {timeout: " + timeout + "}\ninputs:\n  - data: '" + inline + "'\n"
	}
	policy := Limits{Execution: 20 * time.Second, EvidenceBytes: 64 << 20, Events: 1000}
	tests := []struct {
		name    string
		req     Request
		want    string
		clamped []Clamp
	}{
		{"scenario above the policy", Request{Config: config, ScenarioYAML: scenarioWith("45s")}, "20s",
			[]Clamp{{Limit: "execution", Requested: "45s", Effective: "20s"}}},
		{"scenario below the policy", Request{Config: config, ScenarioYAML: scenarioWith("15s")}, "15s", nil},
		{"quick setup above the policy", Request{Config: config, FixtureJSON: string(input), SetupYAML: "execution: {timeout: 1m}\n"}, "20s",
			[]Clamp{{Limit: "execution", Requested: "1m0s", Effective: "20s"}}},
		{"quick run without a timeout", Request{Config: config, FixtureJSON: string(input)}, "20s", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			tt.req.Limits = policy
			req, err := Prepare(ctx, tt.req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _, err := EncodeBundle(req)
			if err != nil {
				t.Fatal(err)
			}
			info, err := InspectBundle(raw)
			if err != nil {
				t.Fatal(err)
			}
			if want := (&ReportLimits{Execution: "20s", EvidenceBytes: 64 << 20, Events: 1000}); !reflect.DeepEqual(info.Limits, want) {
				t.Fatalf("bundle limits = %+v, want the policy %+v", info.Limits, want)
			}
			rep, err := Execute(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			want := &ReportLimits{Execution: tt.want, EvidenceBytes: 64 << 20, Events: 1000, Clamped: tt.clamped}
			if !reflect.DeepEqual(rep.Limits, want) {
				t.Fatalf("report limits = %+v, want %+v", rep.Limits, want)
			}
			if rep.Result.Status != runresult.StatusCompleted || rep.Result.Truncation != nil {
				t.Fatalf("status %s, truncation %+v", rep.Result.Status, rep.Result.Truncation)
			}
		})
	}
}

// TestBudgetLimitedRunsArePartial: past the event or byte budget only
// observation stops. The run is partial, its assertions cannot pass, the
// lifecycle survives and exporter counts still cover everything.
func TestBudgetLimitedRunsArePartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	base := Request{Config: filepath.Join(logsCleanup, "config.yaml"), Scenario: filepath.Join(logsCleanup, "scenario.yaml")}
	full, err := Execute(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if full.Result.Status != runresult.StatusCompleted || full.Assertions.Outcome != assertion.Pass || full.Limits != nil {
		t.Fatalf("unbudgeted run: status %s, outcome %s, limits %+v", full.Result.Status, full.Assertions.Outcome, full.Limits)
	}

	for _, tt := range []struct {
		name   string
		limits Limits
	}{
		{"events", Limits{Events: 3}},
		{"bytes", Limits{EvidenceBytes: 4 << 10}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			req.Limits = tt.limits
			rep, err := Execute(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			r := rep.Result
			if r.Status != runresult.StatusPartial || r.StatusReason != "evidence budget reached; observations are incomplete" {
				t.Fatalf("status %s (%s), want partial", r.Status, r.StatusReason)
			}
			cut := r.Truncation
			if cut == nil || cut.Reason != runresult.TruncationEvidenceBudget || cut.DroppedEvents == 0 || cut.DroppedBytes == 0 {
				t.Fatalf("truncation = %+v", cut)
			}
			if rep.Assertions.Outcome == assertion.Pass {
				t.Fatalf("a budget-limited run passed: %+v", rep.Assertions)
			}
			if rep.Assertions.Outcome != assertion.Inconclusive {
				t.Fatalf("outcome %s, want inconclusive", rep.Assertions.Outcome)
			}
			events := r.Events
			if len(events) < 2 || events[0].Kind != instrument.KindRunStarted || events[len(events)-1].Kind != instrument.KindRunCompleted {
				t.Fatalf("lifecycle not kept: %d events", len(events))
			}
			for i, e := range events {
				if e.Seq != i+1 {
					t.Fatalf("event %d has seq %d", i, e.Seq)
				}
			}
			if len(r.Outputs) != len(full.Result.Outputs) {
				t.Fatalf("outputs = %d, want %d", len(r.Outputs), len(full.Result.Outputs))
			}
			for i, o := range r.Outputs {
				if o.Counts != full.Result.Outputs[i].Counts || o.Batches != full.Result.Outputs[i].Batches {
					t.Fatalf("output %s/%s counts %+v (%d batches), want %+v (%d)", o.Exporter, o.Signal, o.Counts, o.Batches,
						full.Result.Outputs[i].Counts, full.Result.Outputs[i].Batches)
				}
			}
			if tt.name == "events" && len(events) > 3+1 {
				t.Fatalf("kept %d events under a budget of 3 plus the forced end", len(events))
			}
		})
	}
}

// TestTruncatedComparisonIsIncomplete: when either run reached the budget,
// the outputs are not compared and the report is no mismatch.
func TestTruncatedComparisonIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: filepath.Join(logsCleanup, "config.yaml"),
		Fixture: filepath.Join(logsCleanup, "input.json"), Compare: true, Limits: Limits{Events: 3}})
	if err != nil {
		t.Fatal(err)
	}
	c := rep.Comparison
	if c == nil || c.Equal || c.Incomplete != runresult.TruncationEvidenceBudget || len(c.Differences) != 0 || len(c.Notes) == 0 {
		t.Fatalf("comparison = %+v", c)
	}
	if rep.Mismatch() {
		t.Fatal("an incomplete comparison is reported as a mismatch")
	}
}
