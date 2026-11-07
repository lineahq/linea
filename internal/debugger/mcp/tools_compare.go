package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type compareConfigsInput struct {
	Before        source   `json:"before" jsonschema:"Collector configuration before the change."`
	After         source   `json:"after" jsonschema:"Collector configuration after the change."`
	InputJSON     *string  `json:"input_json,omitempty" jsonschema:"Inline OTLP JSON text both configurations receive in quick runs; mutually exclusive with scenario."`
	Scenario      *source  `json:"scenario,omitempty" jsonschema:"Scenario source both configurations run; mutually exclusive with input_json and quick-only fields."`
	SetupYAML     *string  `json:"setup_yaml,omitempty" jsonschema:"Optional quick-run setup YAML."`
	Receiver      *string  `json:"receiver,omitempty" jsonschema:"Optional quick-run injection receiver ID."`
	ClientAddress *string  `json:"client_address,omitempty" jsonschema:"Optional quick-run sender IP address."`
	Ignore        []string `json:"ignore,omitempty" jsonschema:"OTTL paths whose differences are counted but do not make the runs different, such as log.attributes[\"request.id\"]."`
	WaitSeconds   *int     `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for both runs, from 0 to 120; defaults to 30. Waiting never cancels either run; compare unfinished runs later with linea_compare_runs."`
	Runtime       string   `json:"runtime,omitempty" jsonschema:"Optional runtime of the before run from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
	AfterRuntime  string   `json:"after_runtime,omitempty" jsonschema:"Optional runtime of the after run; empty means the before run's runtime. Two runtimes compare a Collector upgrade."`
}

type compareRunsInput struct {
	BeforeRunID string   `json:"before_run_id" jsonschema:"Plain ID of the run before the change."`
	AfterRunID  string   `json:"after_run_id" jsonschema:"Plain ID of the run after the change."`
	Ignore      []string `json:"ignore,omitempty" jsonschema:"OTTL paths whose differences are counted but do not make the runs different, such as log.attributes[\"request.id\"]."`
}

type verifyRecommendationInput struct {
	RunID          string `json:"run_id" jsonschema:"Plain ID of a finished run."`
	Recommendation int    `json:"recommendation" jsonschema:"Position of the recommendation in the run's report.recommendations, as its evidence ref recommendation:<n> names it."`
}

// verificationResult is a recommendation verification with the candidate run
// in the shape the run tools return.
type verificationResult struct {
	RunID          string               `json:"run_id"`
	Recommendation int                  `json:"recommendation"`
	Rule           string               `json:"rule"`
	Overrides      []string             `json:"overrides"`
	Candidate      runResult            `json:"candidate"`
	Confidence     string               `json:"confidence,omitempty"`
	Comparison     *platform.Comparison `json:"comparison,omitempty"`
	Error          string               `json:"error,omitempty"`
}

// summary states the outcome without component names or values.
func (r verificationResult) summary() string {
	text := fmt.Sprintf("Recommendation %d (%s) of run %s: candidate run %s ", r.Recommendation, r.Rule, r.RunID, r.Candidate.RunID)
	switch {
	case r.Error != "":
		return text + "failed."
	case r.Confidence == string(platform.ConfidenceVerifiedOnInput):
		return text + "gave the same output on the run's inputs (verified_on_input)."
	case r.Confidence == string(platform.ConfidenceDiffersOnInput):
		return text + "changed the output on the run's inputs (differs_on_input)."
	}
	return text + "could not be compared conclusively; see comparison.reasons."
}

// comparisonResult is a comparison with its Markdown rendering. A
// configuration comparison also carries both runs; while either is
// unfinished it has no comparison yet.
type comparisonResult struct {
	BeforeRun  *runResult           `json:"before_run,omitempty"`
	AfterRun   *runResult           `json:"after_run,omitempty"`
	Comparison *platform.Comparison `json:"comparison,omitempty"`
	Markdown   string               `json:"markdown,omitempty"`
}

// summary states server-computed facts only: the outcome and counts, never
// component names, paths or values.
func (r comparisonResult) summary() string {
	c := r.Comparison
	if c == nil {
		why := "not finished within the wait"
		if terminal(r.BeforeRun.State) && terminal(r.AfterRun.State) {
			why = "without a result, since a run has no report"
		}
		return fmt.Sprintf("Configuration comparison %s: before run state %s, after run state %s.", why, r.BeforeRun.State, r.AfterRun.State)
	}
	text := "Run comparison: outcome " + string(c.Outcome)
	if len(c.Reasons) > 0 {
		text += "; " + plural(len(c.Reasons), "inconclusive reason")
	}
	if c.InputsIdentical {
		text += "; inputs identical"
	} else {
		text += "; inputs not proven identical"
	}
	differ := 0
	for _, e := range c.Exporters {
		if !e.Equal && !e.EqualAfterIgnore {
			differ++
		}
	}
	text += fmt.Sprintf("; %d of %s differ", differ, plural(len(c.Exporters), "exporter output"))
	if rec := c.Records; rec != nil {
		text += fmt.Sprintf("; %d injected records aligned, %d changed", rec.Aligned, rec.Changed)
	}
	ignored := 0
	for _, i := range c.Ignored {
		ignored += i.Count
	}
	if len(c.Ignored) > 0 {
		text += "; " + plural(ignored, "rule-matched difference")
	}
	return text + "."
}

func (in compareConfigsInput) platform() (platform.ComparisonRequest, int, error) {
	run, wait, err := debugRunInput{Config: in.Before, InputJSON: in.InputJSON, Scenario: in.Scenario, SetupYAML: in.SetupYAML,
		Receiver: in.Receiver, ClientAddress: in.ClientAddress, WaitSeconds: in.WaitSeconds, Runtime: in.Runtime}.platform()
	if err != nil {
		return platform.ComparisonRequest{}, 0, err
	}
	after, err := in.After.platform()
	if err != nil {
		return platform.ComparisonRequest{}, 0, err
	}
	return platform.ComparisonRequest{Before: run.Config, After: after, Mode: run.Mode, Quick: run.Quick, Scenario: run.Scenario,
		Runtime: in.Runtime, AfterRuntime: in.AfterRuntime}, wait, nil
}

func addComparisonTools(server *sdk.Server, service *platform.Service, web webLinks) {
	addTool(server, "linea_compare_configs", "Run two configurations on one captured input and compare what they did: outcome same, different or inconclusive, per-exporter records, per-record fates and field changes. Both are admitted or neither; waiting only observes.",
		func(ctx context.Context, in compareConfigsInput) (summarized, error) {
			req, seconds, err := in.platform()
			if err != nil {
				return nil, err
			}
			runs, err := service.SubmitComparison(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			ids := []string{runs.Before.ID, runs.After.ID}
			views, err := waitRuns(ctx, service, ids, seconds)
			if err != nil {
				return nil, err
			}
			before, after := runView(views[0], web), runView(views[1], web)
			out := comparisonResult{BeforeRun: &before, AfterRun: &after}
			if !terminal(views[0].State) || !terminal(views[1].State) {
				return out, nil
			}
			c, err := service.CompareRuns(ctx, callerOf(ctx), platform.CompareRequest{Before: ids[0], After: ids[1], Ignore: in.Ignore})
			if platform.ErrorCode(err) == platform.CodeConflict {
				// A run that failed has no report; its state says so.
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			out.Comparison, out.Markdown = c, c.Markdown()
			return out, nil
		})
	addTool(server, "linea_compare_runs", "Compare two finished runs from their recorded evidence: outcome same, different or inconclusive with reasons. Nothing executes.",
		func(ctx context.Context, in compareRunsInput) (summarized, error) {
			if in.BeforeRunID == "" || in.AfterRunID == "" {
				return nil, invalidRequest("before_run_id and after_run_id must not be empty")
			}
			c, err := service.CompareRuns(ctx, callerOf(ctx), platform.CompareRequest{Before: in.BeforeRunID, After: in.AfterRunID, Ignore: in.Ignore})
			if err != nil {
				return nil, err
			}
			return comparisonResult{Comparison: c, Markdown: c.Markdown()}, nil
		})
	addTool(server, "linea_verify_recommendation", "Run a recommendation's suggested change on its run's captured inputs and runtime, without changing the user's configuration (the change is layered as provider overrides), and compare the candidate with the run: verified_on_input when the output is the same, differs_on_input when it changes. One more run; waits for it. Only processor-order (swap) changes are verifiable; others are refused as invalid_request.",
		func(ctx context.Context, in verifyRecommendationInput) (summarized, error) {
			if in.RunID == "" {
				return nil, invalidRequest("run_id must not be empty")
			}
			v, err := service.VerifyRecommendation(ctx, callerOf(ctx), platform.VerifyRecommendationRequest{Run: in.RunID, Recommendation: in.Recommendation})
			if err != nil {
				return nil, err
			}
			return verificationResult{RunID: v.Run, Recommendation: v.Recommendation, Rule: v.Rule, Overrides: v.Overrides,
				Candidate: runView(v.Candidate, web), Confidence: string(v.Confidence), Comparison: v.Comparison, Error: v.Error}, nil
		})
}

func terminal(s platform.State) bool {
	return s.Terminal()
}

// waitRuns waits for several runs within one bound, as waitRun does for one.
func waitRuns(ctx context.Context, service *platform.Service, ids []string, seconds int) ([]platform.Run, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	for _, id := range ids {
		_, _ = service.GetRunReport(waitCtx, callerOf(waitCtx), id, true)
	}
	out := make([]platform.Run, len(ids))
	for i, id := range ids {
		run, err := service.GetRun(ctx, callerOf(ctx), id)
		if err != nil {
			return nil, err
		}
		out[i] = run
	}
	return out, nil
}
