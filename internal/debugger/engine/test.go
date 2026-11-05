package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

// TestSchemaVersion versions the TestReport JSON shape.
const TestSchemaVersion = "0.1"

// TestRequest runs the assertions of several scenarios against one
// configuration.
type TestRequest struct {
	Config    string   `json:"config"`
	Scenarios []string `json:"scenarios"`

	Logger *zap.Logger `json:"-"`
}

// TestReport is the outcome of a TestRequest. Its outcome is the worst of
// its scenarios: error, then fail, then inconclusive, then pass.
type TestReport struct {
	Schema    string            `json:"test_schema"`
	Config    string            `json:"config"`
	Outcome   assertion.Outcome `json:"outcome"`
	Scenarios []ScenarioResult  `json:"scenarios"`
}

// ScenarioResult is the outcome of one scenario. Error is set when the
// scenario could not be run or declares no assertions.
type ScenarioResult struct {
	Path     string            `json:"path"`
	Name     string            `json:"name,omitempty"`
	RunID    string            `json:"run_id,omitempty"`
	Outcome  assertion.Outcome `json:"outcome"`
	Error    string            `json:"error,omitempty"`
	Duration float64           `json:"duration_seconds"`
	// Assertions is the evaluation; it is absent when Error is set.
	Assertions *assertion.Report `json:"assertions,omitempty"`
}

// errNoAssertions keeps a scenario without assertions from passing silently.
var errNoAssertions = errors.New("scenario declares no assertions")

// Test runs each scenario in order. Failures of individual scenarios are
// reported in the result, not returned: the error return is reserved for an
// invalid request.
func Test(ctx context.Context, req TestRequest) (*TestReport, error) {
	return TestWithExecutor(ctx, req, Execute)
}

// TestWithExecutor aggregates scenario outcomes using the supplied execution
// path. Platform callers supply their shared scheduler; Test remains the
// low-level compatibility helper for direct engine callers.
func TestWithExecutor(ctx context.Context, req TestRequest, execute func(context.Context, Request) (*Report, error)) (*TestReport, error) {
	if req.Config == "" || len(req.Scenarios) == 0 {
		return nil, fmt.Errorf("%w: config and at least one scenario are required", ErrInvalidRequest)
	}
	if execute == nil {
		return nil, fmt.Errorf("%w: scenario executor is required", ErrInvalidRequest)
	}
	rep := &TestReport{Schema: TestSchemaVersion, Config: req.Config, Outcome: assertion.Pass, Scenarios: []ScenarioResult{}}
	for _, path := range req.Scenarios {
		res := ScenarioResult{Path: path}
		start := time.Now()
		run, err := execute(ctx, Request{Config: req.Config, Scenario: path, Logger: req.Logger})
		res.Duration = time.Since(start).Seconds()
		switch {
		case err != nil:
			res.Outcome, res.Error = assertion.Error, err.Error()
		case run == nil:
			res.Outcome, res.Error = assertion.Error, "scenario executor returned no report"
		case run.Assertions == nil:
			res.Name = run.Scenario
			res.Outcome, res.Error = assertion.Error, errNoAssertions.Error()
		default:
			res.Name, res.RunID = run.Scenario, run.Result.RunID
			res.Outcome, res.Assertions = run.Assertions.Outcome, run.Assertions
		}
		rep.Outcome = assertion.Worst(rep.Outcome, res.Outcome)
		rep.Scenarios = append(rep.Scenarios, res)
	}
	return rep, nil
}
