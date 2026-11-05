package api

import (
	"fmt"
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// ComparisonRequest runs two configurations on one input: the before
// and after configurations, each text or a saved revision, and the
// input of a quick run or a scenario, as in Request.
type ComparisonRequest struct {
	Before ComparisonConfig `json:"before"`
	After  ComparisonConfig `json:"after"`
	// The input, as Request names it.
	FixtureJSON   string                `json:"fixture_json,omitempty"`
	FixtureRef    *platform.ArtifactRef `json:"fixture_ref,omitempty"`
	FixtureLabel  string                `json:"fixture_label,omitempty"`
	ScenarioYAML  string                `json:"scenario_yaml,omitempty"`
	ScenarioRef   *platform.ArtifactRef `json:"scenario_ref,omitempty"`
	ScenarioLabel string                `json:"scenario_label,omitempty"`
	Receiver      string                `json:"receiver,omitempty"`
	ClientAddress string                `json:"client_address,omitempty"`
	SetupYAML     string                `json:"setup_yaml,omitempty"`
	// Runtime is the runtime of both runs, unless AfterRuntime names the
	// after run's: one configuration on two Collector releases.
	Runtime      string `json:"runtime,omitempty"`
	AfterRuntime string `json:"after_runtime,omitempty"`
}

// ComparisonConfig is one configuration of a comparison: text or a saved
// revision, with an optional label for text.
type ComparisonConfig struct {
	ConfigYAML  string                `json:"config_yaml,omitempty"`
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ConfigLabel string                `json:"config_label,omitempty"`
}

func (c ComparisonConfig) source() platform.Source {
	return platform.Source{Text: c.ConfigYAML, Artifact: c.ConfigRef, Label: c.ConfigLabel}
}

func (r ComparisonRequest) submit() platform.ComparisonRequest {
	run := Request{FixtureJSON: r.FixtureJSON, FixtureRef: r.FixtureRef, FixtureLabel: r.FixtureLabel,
		ScenarioYAML: r.ScenarioYAML, ScenarioRef: r.ScenarioRef, ScenarioLabel: r.ScenarioLabel,
		Receiver: r.Receiver, ClientAddress: r.ClientAddress, SetupYAML: r.SetupYAML}.run()
	return platform.ComparisonRequest{Before: r.Before.source(), After: r.After.source(), Mode: run.Mode, Quick: run.Quick, Scenario: run.Scenario,
		Runtime: r.Runtime, AfterRuntime: r.AfterRuntime}
}

// ComparisonRuns are the two runs a comparison admitted.
type ComparisonRuns struct {
	Before Run `json:"before"`
	After  Run `json:"after"`
}

// createComparison admits both runs or neither and returns without waiting;
// GET /runs/{id}/compare/{after} compares them.
func (s *Server) createComparison(w http.ResponseWriter, r *http.Request) {
	var req ComparisonRequest
	if !decode(w, r, &req) {
		return
	}
	runs, err := s.runs.SubmitComparison(r.Context(), callerOf(r), req.submit())
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, ComparisonRuns{Before: runOf(runs.Before), After: runOf(runs.After)})
}

// compareRuns compares two terminal runs, as JSON or, with format=markdown,
// as the Markdown rendering. With wait=true it first waits for both runs to
// finish; ending the request never cancels them.
func (s *Server) compareRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	format := q.Get("format")
	if format != "" && format != "json" && format != "markdown" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown format %q: use json or markdown", format))
		return
	}
	req := platform.CompareRequest{Before: r.PathValue("id"), After: r.PathValue("after"), Ignore: q["ignore"]}
	if q.Get("wait") == "true" {
		for _, id := range []string{req.Before, req.After} {
			// A run that fails or is unknown is reported by the comparison.
			if _, err := s.runs.GetRunReport(r.Context(), callerOf(r), id, true); err != nil && r.Context().Err() != nil {
				return
			}
		}
	}
	c, err := s.runs.CompareRuns(r.Context(), callerOf(r), req)
	if err != nil {
		if r.Context().Err() == nil {
			writeFailure(w, err)
		}
		return
	}
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(c.Markdown()))
		return
	}
	writeJSON(w, http.StatusOK, c)
}
