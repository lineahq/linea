package platform

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// TestVerifyRecommendationRunsTheChangeOnTheRunsInputs: logs-cleanup's
// filter can run first; the candidate runs that order on the run's captured
// inputs, with the user's configuration unchanged, and its output is the
// run's. An unknown recommendation is refused before any run.
func TestVerifyRecommendationRunsTheChangeOnTheRunsInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	samples := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(samples, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: PathPolicy{Unrestricted: true}})
	t.Cleanup(s.Close)
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeScenario, Config: Source{Text: string(config)},
		Scenario: &ScenarioInput{Scenario: Source{Path: filepath.Join(samples, "scenario.yaml")}}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	move := slices.IndexFunc(rep.Recommendations, func(r recommend.Recommendation) bool { return r.Rule == "filter.move_earlier" })
	if move < 0 {
		t.Fatalf("no filter.move_earlier in %+v", rep.Recommendations)
	}
	got, err := s.VerifyRecommendation(ctx, testCaller, VerifyRecommendationRequest{Run: run.ID, Recommendation: move})
	if err != nil {
		t.Fatal(err)
	}
	want := `yaml:service::pipelines::logs::processors: ["filter/health","resource","attributes/scrub","transform/severity","batch"]`
	if got.Rule != "filter.move_earlier" || !slices.Equal(got.Overrides, []string{want}) || got.Error != "" {
		t.Fatalf("verification = %+v", got)
	}
	if got.Confidence != recommend.ConfidenceVerifiedOnInput || got.Comparison == nil || !got.Comparison.InputsIdentical {
		t.Fatalf("confidence %q, comparison %+v", got.Confidence, got.Comparison)
	}
	if got.Candidate.ID == run.ID || got.Candidate.State != StateDone {
		t.Fatalf("candidate = %+v", got.Candidate)
	}
	cand, err := s.GetRunReport(ctx, testCaller, got.Candidate.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range cand.Topology.Pipelines {
		if p.ID == "logs" && !strings.HasPrefix(strings.Join(p.Processors, ","), "processor/filter/health,") {
			t.Fatalf("candidate pipeline = %v", p.Processors)
		}
	}
	// The candidate's configuration is the user's, unchanged.
	var digests []string
	for _, id := range []string{run.ID, got.Candidate.ID} {
		inputs, err := s.GetRunInputs(ctx, testCaller, id)
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, inputs.Config.SHA256)
	}
	if digests[0] == "" || digests[0] != digests[1] {
		t.Fatalf("configuration digests = %q", digests)
	}

	for _, c := range []struct {
		index int
		code  Code
	}{{len(rep.Recommendations), CodeNotFound}, {-1, CodeNotFound}} {
		_, err := s.VerifyRecommendation(ctx, testCaller, VerifyRecommendationRequest{Run: run.ID, Recommendation: c.index})
		if ErrorCode(err) != c.code {
			t.Fatalf("recommendation %d: %v, want %s", c.index, err, c.code)
		}
	}
}

// TestVerifyRecommendationReportsAChangedOutput: in filter-order the filter
// reads what transform/tag sets; filter.after_batch moves it before both, so
// the candidate drops nothing and its output differs from the run's.
func TestVerifyRecommendationReportsAChangedOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	samples := filepath.Join("..", "..", "..", "samples", "filter-order")
	config, err := os.ReadFile(filepath.Join(samples, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: PathPolicy{Unrestricted: true}})
	t.Cleanup(s.Close)
	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeScenario, Config: Source{Text: string(config)},
		Scenario: &ScenarioInput{Scenario: Source{Path: filepath.Join(samples, "scenario.yaml")}}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(rep.Recommendations, func(r recommend.Recommendation) bool { return r.Rule == "filter.after_batch" })
	if at < 0 || len(rep.Recommendations[at].Blockers) == 0 {
		t.Fatalf("no blocked filter.after_batch in %+v", rep.Recommendations)
	}
	got, err := s.VerifyRecommendation(ctx, testCaller, VerifyRecommendationRequest{Run: run.ID, Recommendation: at})
	if err != nil {
		t.Fatal(err)
	}
	if got.Confidence != recommend.ConfidenceDiffersOnInput || got.Comparison == nil || !got.Comparison.InputsIdentical || got.Error != "" {
		t.Fatalf("verification = %+v", got)
	}
}
