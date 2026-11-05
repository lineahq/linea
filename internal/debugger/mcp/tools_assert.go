package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type evaluateAssertionsInput struct {
	RunID        string `json:"run_id" jsonschema:"Plain run ID returned by linea_run."`
	ScenarioYAML string `json:"scenario_yaml" jsonschema:"Inline scenario v1 YAML; only its assertions are used, inputs are ignored."`
}

type evaluateAssertionsResult struct {
	EvaluationID string                    `json:"evaluation_id"`
	RunID        string                    `json:"run_id"`
	Assertions   *platform.AssertionReport `json:"assertions"`
}

func (r evaluateAssertionsResult) summary() string {
	counts := platform.SummarizeAssertions(r.Assertions)
	return fmt.Sprintf("Evaluated assertions: outcome %s; %d pass, %d fail, %d inconclusive, %d error.",
		counts.Outcome, counts.Pass, counts.Fail, counts.Inconclusive, counts.Error)
}

type evaluationInput struct {
	RunID        string `json:"run_id" jsonschema:"Plain run ID."`
	EvaluationID string `json:"evaluation_id" jsonschema:"evaluation_id returned by linea_evaluate_assertions or listed by linea_list_evaluations."`
}

// evaluationInfo is GET /runs/{id}/evaluations' item.
type evaluationInfo struct {
	ID      string    `json:"id"`
	RunID   string    `json:"run_id"`
	Client  string    `json:"client"`
	Created time.Time `json:"created"`
	Outcome string    `json:"outcome"`
}

func evaluationInfoOf(e platform.EvaluationInfo) evaluationInfo {
	return evaluationInfo{ID: e.ID, RunID: e.RunID, Client: e.Caller.Client, Created: e.Created, Outcome: string(e.Outcome)}
}

type evaluationsResult struct {
	RunID       string           `json:"run_id"`
	Evaluations []evaluationInfo `json:"evaluations"`
}

func (r evaluationsResult) summary() string {
	return fmt.Sprintf("Evaluations: %s recorded.", plural(len(r.Evaluations), "evaluation"))
}

type evaluationResult struct {
	evaluationInfo
	Result *platform.AssertionReport `json:"result"`
}

func (r evaluationResult) summary() string {
	counts := platform.SummarizeAssertions(r.Result)
	return fmt.Sprintf("Recorded evaluation: outcome %s; %d pass, %d fail, %d inconclusive, %d error.",
		counts.Outcome, counts.Pass, counts.Fail, counts.Inconclusive, counts.Error)
}

func addAssertionTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_evaluations", "List the assertion evaluations recorded beside a run, in recording order, without their results.",
		func(ctx context.Context, in runInput) (summarized, error) {
			list, err := service.ListEvaluations(ctx, callerOf(ctx), in.RunID)
			if err != nil {
				return nil, err
			}
			out := evaluationsResult{RunID: in.RunID, Evaluations: make([]evaluationInfo, 0, len(list))}
			for _, e := range list {
				out.Evaluations = append(out.Evaluations, evaluationInfoOf(e))
			}
			return out, nil
		})
	addTool(server, "linea_get_evaluation", "Read one recorded evaluation of a run with its assertion report. The scenario text it evaluated is never returned.",
		func(ctx context.Context, in evaluationInput) (summarized, error) {
			e, err := service.GetEvaluation(ctx, callerOf(ctx), in.EvaluationID)
			if err == nil && e.RunID != in.RunID {
				err = &platform.Error{Code: platform.CodeNotFound, Message: "unknown evaluation"}
			}
			if err != nil {
				return nil, err
			}
			return evaluationResult{evaluationInfo: evaluationInfoOf(e.EvaluationInfo), Result: e.Result}, nil
		})
	addTool(server, "linea_evaluate_assertions", "Evaluate a scenario's assertions against a finished run's recorded observations without executing it again. Only the assertions are used; the scenario's inputs are ignored. The evaluation is recorded beside the run and never changes the run.",
		func(ctx context.Context, in evaluateAssertionsInput) (summarized, error) {
			report, err := service.EvaluateAssertions(ctx, callerOf(ctx), in.RunID, platform.AssertionsRequest{ScenarioYAML: in.ScenarioYAML, Record: true})
			if err != nil {
				return nil, err
			}
			return evaluateAssertionsResult{EvaluationID: report.ID, RunID: in.RunID, Assertions: report.Result}, nil
		})
}
