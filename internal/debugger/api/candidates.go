package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// CandidateRequest proposes a configuration change from a base configuration
// (text or a saved revision): exactly one of complete proposed YAML or
// one addressed transform statement edit. Saving creates a new configuration
// artifact; the base, its head and host files are never changed.
type CandidateRequest struct {
	ConfigYAML  string                `json:"config_yaml,omitempty"`
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ConfigLabel string                `json:"config_label,omitempty"`
	// Edit replaces, inserts, appends or deletes one statement. Replace and
	// delete name the statement's expected text; a stale edit is a 409 with
	// the current text.
	Edit         *platform.StatementEdit `json:"edit,omitempty"`
	ProposedYAML string                  `json:"proposed_yaml,omitempty"`
	Save         bool                    `json:"save,omitempty"`
	// Label names the saved artifact; it defaults to the platform's label.
	Label string `json:"label,omitempty"`
	// Runtime validates the proposal on a built runtime (see Request.Runtime).
	Runtime string `json:"runtime,omitempty"`
}

func (r CandidateRequest) candidate() platform.CandidateRequest {
	return platform.CandidateRequest{Base: platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef, Label: r.ConfigLabel},
		YAML: r.ProposedYAML, Statement: r.Edit, Save: r.Save, Label: r.Label, Runtime: r.Runtime}
}

// Candidate is a validated proposal: the proposed configuration text, a
// unified patch from the base, the pipelines it affects, its validation and
// the saved artifact when one was requested and the proposal is valid.
type Candidate struct {
	ProposedYAML      string                  `json:"proposed_yaml"`
	Patch             string                  `json:"patch"`
	AffectedPipelines []string                `json:"affected_pipelines"`
	Valid             bool                    `json:"valid"`
	Diagnostics       []platform.Diagnostic   `json:"diagnostics"`
	Saved             *platform.SavedArtifact `json:"saved,omitempty"`
}

// ScenarioFromRunRequest derives a self-contained scenario from a run's
// captured inputs. Assertions-only YAML is appended after the run's own.
type ScenarioFromRunRequest struct {
	Name           string `json:"name,omitempty"`
	AssertionsYAML string `json:"assertions_yaml,omitempty"`
	Save           bool   `json:"save,omitempty"`
	Label          string `json:"label,omitempty"`
}

// ScenarioFromRun is the derived scenario text, the environment names a
// rerun must supply and the saved artifact when one was requested.
type ScenarioFromRun struct {
	RunID               string                  `json:"run_id"`
	YAML                string                  `json:"yaml"`
	RequiredEnvironment []string                `json:"required_environment"`
	Saved               *platform.SavedArtifact `json:"saved,omitempty"`
}

// ExportRequest names and extends the exported scenario, as in
// ScenarioFromRunRequest; an export is never saved.
type ExportRequest struct {
	Name           string `json:"name,omitempty"`
	AssertionsYAML string `json:"assertions_yaml,omitempty"`
}

// ScenarioExport is a run as a reviewable scenario file: a comment header
// (source run, build, required environment names, reproducibility and what
// was left out) and the scenario YAML. The client writes the file.
type ScenarioExport struct {
	RunID               string   `json:"run_id"`
	File                string   `json:"file"`
	RequiredEnvironment []string `json:"required_environment"`
}

func (s *Server) createCandidate(w http.ResponseWriter, r *http.Request) {
	var req CandidateRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.runs.CreateCandidate(r.Context(), callerOf(r), req.candidate())
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := candidateOf(c)
	status := http.StatusOK
	if out.Saved != nil {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func candidateOf(c *platform.Candidate) Candidate {
	out := Candidate{ProposedYAML: c.Proposed, Patch: c.Patch, AffectedPipelines: c.AffectedPipelines, Valid: c.Valid,
		Diagnostics: c.Diagnostics, Saved: c.Saved}
	if out.AffectedPipelines == nil {
		out.AffectedPipelines = []string{}
	}
	if out.Diagnostics == nil {
		out.Diagnostics = []platform.Diagnostic{}
	}
	return out
}

func (s *Server) createScenarioFromRun(w http.ResponseWriter, r *http.Request) {
	var req ScenarioFromRunRequest
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	sc, err := s.runs.CreateScenarioFromRun(r.Context(), callerOf(r), id, platform.ScenarioFromRunRequest{
		Assertions: req.AssertionsYAML, Name: req.Name, Save: req.Save, Label: req.Label})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := ScenarioFromRun{RunID: id, YAML: sc.YAML, RequiredEnvironment: sc.RequiredEnvironment, Saved: sc.Saved}
	if out.RequiredEnvironment == nil {
		out.RequiredEnvironment = []string{}
	}
	status := http.StatusOK
	if out.Saved != nil {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func (s *Server) exportScenario(w http.ResponseWriter, r *http.Request) {
	var req ExportRequest
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	sc, err := s.runs.ExportScenario(r.Context(), callerOf(r), id, platform.ScenarioFromRunRequest{Assertions: req.AssertionsYAML, Name: req.Name})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := ScenarioExport{RunID: id, File: sc.Export, RequiredEnvironment: sc.RequiredEnvironment}
	if out.RequiredEnvironment == nil {
		out.RequiredEnvironment = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}
