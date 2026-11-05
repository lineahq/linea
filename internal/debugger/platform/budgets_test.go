package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestBudgetsApplyDefaultsAndCaps(t *testing.T) {
	const mib = 1 << 20
	defaults := Budgets{Execution: 10 * time.Minute, EvidenceBytes: 64 * mib, Events: 200_000, PageBytes: 64 << 10, PageItems: 100}
	tests := []struct {
		name     string
		in       Budgets
		runBytes int64
		want     Budgets
	}{
		{"ephemeral defaults", Budgets{}, 0, defaults},
		{"negative values use defaults", Budgets{Execution: -1, EvidenceBytes: -1, Events: -1, PageBytes: -1, PageItems: -1}, 0, defaults},
		{"workspace allowance minus headroom", Budgets{}, 64 * mib, withEvidence(defaults, 63*mib)},
		{"small workspace allowance is halved", Budgets{}, 3 * mib / 2, withEvidence(defaults, 3*mib/4)},
		{"explicit evidence above the workspace is lowered", Budgets{EvidenceBytes: 100 * mib}, 10 * mib, withEvidence(defaults, 9*mib)},
		{"explicit evidence below the workspace is kept", Budgets{EvidenceBytes: 2 * mib}, 10 * mib, withEvidence(defaults, 2*mib)},
		{"explicit ephemeral evidence is kept", Budgets{EvidenceBytes: 200 * mib}, 0, withEvidence(defaults, 200*mib)},
		{"pages are capped", Budgets{Execution: time.Minute, Events: 5, PageBytes: 2 * mib, PageItems: 5000}, 0,
			Budgets{Execution: time.Minute, EvidenceBytes: 64 * mib, Events: 5, PageBytes: mib, PageItems: 1000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.normalized(tt.runBytes); got != tt.want {
				t.Fatalf("normalized = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func withEvidence(b Budgets, bytes int64) Budgets {
	b.EvidenceBytes = bytes
	return b
}

func TestRunsCarryThePolicyLimits(t *testing.T) {
	seen := make(chan engine.Limits, 1)
	s := New(Options{Prepare: unprepared, Budgets: Budgets{Execution: time.Minute, Events: 7}, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		seen <- req.Limits
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	waitRun(t, submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}))
	if got, want := receive(t, seen), (engine.Limits{Execution: time.Minute, EvidenceBytes: 64 << 20, Events: 7}); got != want {
		t.Fatalf("limits = %+v, want %+v", got, want)
	}
}

// TestWorkspaceRunsLargerThanTheirAllowanceKeepIncompleteEvidence: the
// evidence budget leaves the archive's headroom, so a run that would not fit
// ends with incomplete evidence instead of a persistence error, in both
// modes, and reads back the same after a restart.
func TestWorkspaceRunsLargerThanTheirAllowanceKeepIncompleteEvidence(t *testing.T) {
	sample := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario := "version: 1\nname: budget\ninputs:\n  - data: '" + strings.ReplaceAll(strings.TrimSpace(string(input)), "\n", " ") +
		"'\nassertions:\n  - name: three logs leave\n    target: {exporter: otlp/backend, signal: logs}\n    count: {equals: 3}\n"
	data := memory.NewData()
	const runBytes = 32 << 10
	s := openPlatformLimits(t, data, artifact.Limits{RunBytes: runBytes}, Options{})
	var ids []string
	for _, req := range []engine.Request{
		{ConfigYAML: string(config), FixtureJSON: string(input)},
		{ConfigYAML: string(config), ScenarioYAML: scenario},
	} {
		got := waitRun(t, submit(t, s, req))
		if got.State != StateDone || got.Error != "" || got.Report == nil {
			t.Fatalf("run state %s, error %q", got.State, got.Error)
		}
		r := got.Report.Result
		if r.Status != runresult.StatusPartial || r.Truncation == nil || r.Truncation.Reason != runresult.TruncationEvidenceBudget {
			t.Fatalf("status %s, truncation %+v", r.Status, r.Truncation)
		}
		if l := got.Report.Limits; l == nil || l.EvidenceBytes != runBytes/2 {
			t.Fatalf("limits = %+v", l)
		}
		if got.Bundle == nil || got.Bundle.Limits == nil || got.Bundle.Limits.EvidenceBytes != runBytes/2 {
			t.Fatalf("bundle limits = %+v", got.Bundle)
		}
		if req.ScenarioYAML != "" && got.Report.Assertions.Outcome != assertion.Inconclusive {
			t.Fatalf("assertions = %s, want inconclusive", got.Report.Assertions.Outcome)
		}
		ids = append(ids, got.ID)
	}
	s.Close()
	s = openPlatformLimits(t, data, artifact.Limits{RunBytes: runBytes}, Options{})
	for _, id := range ids {
		h, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		got := h.View(true)
		if got.Report == nil || got.Report.Result.Status != runresult.StatusPartial || got.Report.Result.Truncation == nil {
			t.Fatalf("after restart: %+v", got)
		}
	}
}

// TestOversizedAnalysisIsOmittedRatherThanLosingTheRun: when the report's
// analysis outgrows the archive's headroom, the run is kept without its
// derived sections, the same before and after a restart. The analysis is
// random hex, so it outgrows the allowance even compressed.
func TestOversizedAnalysisIsOmittedRatherThanLosingTheRun(t *testing.T) {
	data := memory.NewData()
	limits := artifact.Limits{RunBytes: 64 << 10}
	random := make([]byte, 128<<10)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	s := openPlatformLimits(t, data, limits, Options{Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Stages = []analysis.Stage{{Component: hex.EncodeToString(random)}}
		return rep, nil
	}})
	got := waitRun(t, submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}))
	check := func(when string, got Run) {
		t.Helper()
		if got.State != StateDone || got.Report == nil || len(got.Report.Stages) != 0 {
			t.Fatalf("%s: state %s, error %q", when, got.State, got.Error)
		}
		cut := got.Report.Result.Truncation
		if cut == nil || cut.Reason != runresult.TruncationEvidenceBudget || !slices.Equal(cut.Omitted, omittedSections) {
			t.Fatalf("%s: truncation = %+v", when, cut)
		}
		if got.Report.Result.Status != runresult.StatusCompleted {
			t.Fatalf("%s: status %s; omitting analysis does not change the run's status", when, got.Report.Result.Status)
		}
	}
	check("before restart", got)
	s.Close()
	s = openPlatformLimits(t, data, limits, Options{})
	h, err := s.Get(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("after restart", h.View(true))
}
