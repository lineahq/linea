package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// StatementTestRequest tests a candidate on a run's recorded configuration
// and inputs: exactly one of edit or proposed_yaml. The
// run's configuration is the base; nothing is saved or changed.
type StatementTestRequest struct {
	Edit         *platform.StatementEdit `json:"edit,omitempty"`
	ProposedYAML string                  `json:"proposed_yaml,omitempty"`
	// AssertionsYAML replaces the recorded assertions; empty keeps them.
	AssertionsYAML string `json:"assertions_yaml,omitempty"`
	// Runtime overrides the runtime the run's report names.
	Runtime string `json:"runtime,omitempty"`
}

// StatementTest is the validated candidate and, when it is valid, the run
// that tests it, which is not awaited. has_assertions is false when no
// assertion declares an output boundary, so the run cannot pass.
type StatementTest struct {
	Candidate     Candidate `json:"candidate"`
	Run           *Run      `json:"run,omitempty"`
	HasAssertions bool      `json:"has_assertions"`
}

func (s *Server) testStatementOnRun(w http.ResponseWriter, r *http.Request) {
	var req StatementTestRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := s.runs.TestStatement(r.Context(), callerOf(r), platform.StatementTestRequest{RunID: r.PathValue("id"),
		Candidate: platform.CandidateRequest{Statement: req.Edit, YAML: req.ProposedYAML, Runtime: req.Runtime}, Assertions: req.AssertionsYAML})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := StatementTest{Candidate: candidateOf(t.Candidate), HasAssertions: t.HasAssertions}
	status := http.StatusOK
	if t.Run != nil {
		run := runOf(*t.Run)
		out.Run, status = &run, http.StatusAccepted
	}
	writeJSON(w, status, out)
}
