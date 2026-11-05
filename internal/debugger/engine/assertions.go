package engine

import (
	"fmt"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/scenario"
)

// AssertionsRequest re-evaluates the assertions of an edited scenario against
// a finished run, without executing it again.
type AssertionsRequest struct {
	// ScenarioYAML is the scenario text; only its assertions are used, but
	// the whole scenario must be valid.
	ScenarioYAML string `json:"scenario_yaml"`
}

// EvaluateAssertions evaluates the assertions of req's scenario against the
// observations recorded in rep. Assertions read recorded events only, so the
// result equals what a run of the same inputs would report. A scenario that
// does not parse is an error that does not wrap ErrInvalidRequest: it is the
// user's input, not a malformed request.
func EvaluateAssertions(rep *Report, req AssertionsRequest) (*assertion.Report, error) {
	if req.ScenarioYAML == "" {
		return nil, fmt.Errorf("%w: scenario_yaml is required", ErrInvalidRequest)
	}
	if rep == nil || rep.Topology == nil || rep.Result == nil {
		return nil, fmt.Errorf("%w: the run has no recorded result", ErrInvalidRequest)
	}
	s, err := scenario.Parse([]byte(req.ScenarioYAML), "")
	if err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	return assertion.Evaluate(s.Assertions, rep.Topology, rep.Result)
}
