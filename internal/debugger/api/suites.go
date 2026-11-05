package api

import (
	"mime"
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// SuiteRequest submits up to 32 scenarios against one configuration, each an
// ordinary scenario run admitted in order. The configuration is text or a
// saved revision, as in Request.
type SuiteRequest struct {
	ConfigYAML  string                `json:"config_yaml,omitempty"`
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ConfigLabel string                `json:"config_label,omitempty"`
	Scenarios   []SuiteScenario       `json:"scenarios"`
	// Runtime names a built runtime (name, image reference or image ID);
	// empty or "built-in" is the debugger's own registry.
	Runtime string `json:"runtime,omitempty"`
}

// SuiteScenario is one member: scenario text or a saved revision. Its inputs
// must be inline (data).
type SuiteScenario struct {
	ScenarioYAML  string                `json:"scenario_yaml,omitempty"`
	ScenarioRef   *platform.ArtifactRef `json:"scenario_ref,omitempty"`
	ScenarioLabel string                `json:"scenario_label,omitempty"`
}

func (r SuiteRequest) submit() platform.SubmitSuiteRequest {
	req := platform.SubmitSuiteRequest{Config: platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef, Label: r.ConfigLabel},
		Members: make([]platform.ScenarioInput, 0, len(r.Scenarios)), Runtime: r.Runtime}
	for _, s := range r.Scenarios {
		req.Members = append(req.Members, platform.ScenarioInput{
			Scenario: platform.Source{Text: s.ScenarioYAML, Artifact: s.ScenarioRef, Label: s.ScenarioLabel}})
	}
	return req
}

// Suite is a submitted suite: its configuration label and ordered members,
// each with its run ID or the error that kept it from being admitted.
type Suite struct {
	ID      string        `json:"id"`
	Project string        `json:"project_id"`
	Created time.Time     `json:"created"`
	Config  string        `json:"config"`
	Members []SuiteMember `json:"members"`
}

type SuiteMember struct {
	Label string `json:"label"`
	RunID string `json:"run_id,omitempty"`
	Error string `json:"error,omitempty"`
}

// SuiteStatus derives each member's state and outcome from its run. Outcome
// is set once every member has one.
type SuiteStatus struct {
	ID      string              `json:"id"`
	Created time.Time           `json:"created"`
	Config  string              `json:"config"`
	Done    bool                `json:"done"`
	Outcome assertion.Outcome   `json:"outcome,omitempty"`
	Members []SuiteMemberStatus `json:"members"`
}

type SuiteMemberStatus struct {
	Label   string            `json:"label"`
	RunID   string            `json:"run_id,omitempty"`
	State   State             `json:"state,omitempty"`
	Name    string            `json:"name,omitempty"`
	Outcome assertion.Outcome `json:"outcome,omitempty"`
	// Message explains an error outcome without assertion results.
	Message string `json:"message,omitempty"`
}

func suiteOf(v *platform.Suite) Suite {
	out := Suite{ID: v.ID, Project: v.Project, Created: v.Created, Config: v.Config, Members: make([]SuiteMember, 0, len(v.Members))}
	for _, m := range v.Members {
		out.Members = append(out.Members, SuiteMember{Label: m.Label, RunID: m.RunID, Error: m.Error})
	}
	return out
}

func suiteStatusOf(v *platform.SuiteStatus) SuiteStatus {
	out := SuiteStatus{ID: v.ID, Created: v.Created, Config: v.Config, Done: v.Done, Outcome: v.Outcome, Members: make([]SuiteMemberStatus, 0, len(v.Members))}
	for _, m := range v.Members {
		member := SuiteMemberStatus{Label: m.Label, RunID: m.RunID, Name: m.Name, Outcome: m.Outcome, Message: m.Message}
		if m.RunID != "" {
			member.State = stateOf(m.State)
		}
		out.Members = append(out.Members, member)
	}
	return out
}

func (s *Server) createSuite(w http.ResponseWriter, r *http.Request) {
	var req SuiteRequest
	if !decode(w, r, &req) {
		return
	}
	suite, err := s.runs.SubmitSuite(r.Context(), callerOf(r), req.submit())
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/suites/"+suite.ID)
	writeJSON(w, http.StatusAccepted, suiteOf(suite))
}

// listSuites returns admission metadata for retained suites, newest first.
func (s *Server) listSuites(w http.ResponseWriter, r *http.Request) {
	suites, err := s.runs.ListSuites(r.Context(), callerOf(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	views := make([]Suite, len(suites))
	for i := range suites {
		views[i] = suiteOf(&suites[i])
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getSuite(w http.ResponseWriter, r *http.Request) {
	status, err := s.runs.GetSuite(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, suiteStatusOf(status))
}

// getSuiteReport returns the report `test` prints. With ?wait=true it blocks
// until every member finishes; otherwise an unfinished suite is a 409.
func (s *Server) getSuiteReport(w http.ResponseWriter, r *http.Request) {
	rep, err := s.runs.GetSuiteReport(r.Context(), callerOf(r), r.PathValue("id"), r.URL.Query().Get("wait") == "true")
	if err != nil {
		if r.Context().Err() == nil {
			writeFailure(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// rerunSuite admits a new suite from a suite's recorded inputs.
func (s *Server) rerunSuite(w http.ResponseWriter, r *http.Request) {
	suite, err := s.runs.RerunSuite(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/suites/"+suite.ID)
	writeJSON(w, http.StatusAccepted, suiteOf(suite))
}

// getSuiteJUnit returns a finished suite's report as JUnit XML.
func (s *Server) getSuiteJUnit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, err := s.runs.GetSuiteJUnit(r.Context(), callerOf(r), id)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": id + ".junit.xml"}))
	_, _ = w.Write(raw)
}
