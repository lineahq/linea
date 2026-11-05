package mcp

import (
	"context"
	"fmt"
	"net/url"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type debugRunInput struct {
	Config        source  `json:"config" jsonschema:"Collector configuration source."`
	InputJSON     *string `json:"input_json,omitempty" jsonschema:"Inline OTLP JSON text for a quick run; mutually exclusive with scenario."`
	Scenario      *source `json:"scenario,omitempty" jsonschema:"Scenario source; mutually exclusive with input_json and quick-only fields."`
	SetupYAML     *string `json:"setup_yaml,omitempty" jsonschema:"Optional quick-run setup YAML."`
	Receiver      *string `json:"receiver,omitempty" jsonschema:"Optional quick-run injection receiver ID."`
	ClientAddress *string `json:"client_address,omitempty" jsonschema:"Optional quick-run sender IP address."`
	WaitSeconds   *int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait, from 0 to 120; defaults to 30. Waiting never cancels the run."`
	Runtime       string  `json:"runtime,omitempty" jsonschema:"Optional runtime name, image reference or ID from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
}

func (in debugRunInput) platform() (platform.RunRequest, int, error) {
	wait, err := waitSeconds(in.WaitSeconds)
	if err != nil {
		return platform.RunRequest{}, 0, err
	}
	if (in.InputJSON == nil) == (in.Scenario == nil) {
		return platform.RunRequest{}, 0, invalidRequest("supply exactly one of input_json or scenario")
	}
	if in.Scenario != nil && (in.SetupYAML != nil || in.Receiver != nil || in.ClientAddress != nil) {
		return platform.RunRequest{}, 0, invalidRequest("setup_yaml, receiver and client_address are quick-only fields")
	}
	config, err := in.Config.platform()
	if err != nil {
		return platform.RunRequest{}, 0, err
	}
	req := platform.RunRequest{Config: config, Runtime: in.Runtime}
	if in.Scenario != nil {
		req.Mode = platform.ModeScenario
		req.Scenario, err = scenarioSource(in.Scenario)
		return req, wait, err
	}
	req.Mode = platform.ModeQuick
	req.Quick = &platform.QuickInput{Fixture: platform.Source{Text: *in.InputJSON}}
	if in.SetupYAML != nil {
		req.Quick.Setup = *in.SetupYAML
	}
	if in.Receiver != nil {
		req.Quick.Receiver = *in.Receiver
	}
	if in.ClientAddress != nil {
		req.Quick.ClientAddress = *in.ClientAddress
	}
	return req, wait, nil
}

func waitSeconds(seconds *int) (int, error) {
	wait := 30
	if seconds != nil {
		wait = *seconds
	}
	if wait < 0 || wait > 120 {
		return 0, invalidRequest("wait_seconds must be between 0 and 120")
	}
	return wait, nil
}

type runInput struct {
	RunID string `json:"run_id" jsonschema:"Plain run ID returned by linea_run."`
}

type runResult struct {
	RunID string             `json:"run_id"`
	Mode  platform.InputMode `json:"mode"`
	State platform.State     `json:"state"`
	Error *string            `json:"error,omitempty"`
	// Summary is present once the run is terminal; its text facts
	// count the stand-ins named here.
	Summary *platform.RunSummary `json:"summary,omitempty"`
	standIns
	// Runtime is the runtime the run executed on, once it has a report.
	Runtime *platform.ReportRuntime `json:"runtime,omitempty"`
	// WebURL opens the run in the web client served beside this adapter.
	WebURL string `json:"web_url,omitempty"`
}

// webLinks is the web client's base URL, empty when none is served.
type webLinks string

func (w webLinks) run(id string) string {
	if w == "" {
		return ""
	}
	return string(w) + "/?run=" + url.QueryEscape(id)
}

func runView(run platform.Run, web webLinks) runResult {
	out := runResult{RunID: run.ID, Mode: run.Mode, State: run.State, Summary: withURIs(run.ID, run.Summary()), WebURL: web.run(run.ID)}
	if run.State == platform.StateError {
		out.Error = &run.Error
	}
	if run.Report != nil {
		out.standIns = standInsOf(run.Report.Topology, run.Report.References)
		out.Runtime = run.Report.Runtime
	}
	return out
}

func (r runResult) modeName() string {
	if r.Mode == platform.ModeScenario {
		return "Scenario"
	}
	return "Quick"
}

func (r runResult) summary() string {
	completion := "finished"
	if !r.State.Terminal() {
		completion = "not finished within the wait"
	}
	return fmt.Sprintf("%s run %s: state %s%s.", r.modeName(), completion, r.State, r.facts())
}

func (r runResult) facts() string {
	if r.Summary == nil {
		return ""
	}
	return summaryFacts(r.Summary)
}

type getRunResult struct {
	runResult
	Created     time.Time  `json:"created"`
	Started     *time.Time `json:"started"`
	Finished    *time.Time `json:"finished"`
	Interrupted bool       `json:"interrupted"`
	Kept        bool       `json:"kept,omitempty"`
}

func (r getRunResult) summary() string {
	return fmt.Sprintf("%s run: state %s%s.", r.modeName(), r.State, r.facts())
}

type cancelRunResult struct {
	RunID     string         `json:"run_id"`
	State     platform.State `json:"state"`
	Effective bool           `json:"effective"`
}

func (r cancelRunResult) summary() string {
	return fmt.Sprintf("Run cancellation: state %s; effective %t.", r.State, r.Effective)
}

type runLogResult struct{ platform.RunLog }

func (r runLogResult) summary() string {
	if !r.Kept {
		return "Run log: not kept; only a failed or partial run keeps its Collector log."
	}
	return fmt.Sprintf("Run log: %d bytes, environment values redacted; truncated %t.", len(r.Log), r.Truncated)
}

type testResultsResult struct {
	RunID      string             `json:"run_id"`
	Mode       platform.InputMode `json:"mode"`
	Assertions any                `json:"assertions"`
}

type testResultsInput struct {
	RunID   *string `json:"run_id,omitempty" jsonschema:"Nonempty plain run ID; supply exactly one of run_id or suite_id."`
	SuiteID *string `json:"suite_id,omitempty" jsonschema:"Nonempty plain suite ID; supply exactly one of run_id or suite_id."`
}

func (r testResultsResult) summary() string {
	mode := runResult{Mode: r.Mode}.modeName()
	report, ok := r.Assertions.(*platform.AssertionReport)
	if !ok || report == nil {
		return mode + " run has no assertions."
	}
	counts := platform.SummarizeAssertions(report)
	return fmt.Sprintf("Run assertions: outcome %s; %d pass, %d fail, %d inconclusive, %d error.",
		counts.Outcome, counts.Pass, counts.Fail, counts.Inconclusive, counts.Error)
}

type listRunsInput struct {
	Scenario string `json:"scenario,omitempty" jsonschema:"Only runs of this scenario name."`
	Runtime  string `json:"runtime,omitempty" jsonschema:"Only runs on this runtime name; built-in is the debugger's own."`
	Result   string `json:"result,omitempty" jsonschema:"Only runs that ended this way: completed, partial, failed or cancelled for a run with a report, otherwise its state queued, running or error."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum runs, newest first; default 25, at most 100."`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous call with the same filters, for the next older runs."`
}

type listedRun struct {
	RunID   string               `json:"run_id"`
	Mode    platform.InputMode   `json:"mode"`
	State   platform.State       `json:"state"`
	Created time.Time            `json:"created"`
	Error   string               `json:"error,omitempty"`
	Summary *platform.RunListing `json:"summary,omitempty"`
	Kept    bool                 `json:"kept,omitempty"`
	WebURL  string               `json:"web_url,omitempty"`
}

type listRunsResult struct {
	Runs []listedRun `json:"runs"`
	// NextCursor continues the list with older matching runs; it is absent
	// on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

func (r listRunsResult) summary() string {
	return fmt.Sprintf("Runs: %d listed, newest first; more pages %t.", len(r.Runs), r.NextCursor != "")
}

func listRuns(ctx context.Context, service *platform.Service, web webLinks, in listRunsInput) (listRunsResult, error) {
	limit := in.Limit
	if limit < 0 || limit > 100 {
		return listRunsResult{}, invalidRequest("limit must be between 1 and 100")
	}
	if limit == 0 {
		limit = 25
	}
	page, err := service.ListRuns(ctx, callerOf(ctx), platform.RunQuery{RunFilter: platform.RunFilter{Scenario: in.Scenario, Runtime: in.Runtime, Result: in.Result}, Limit: limit, Cursor: in.Cursor})
	if err != nil {
		return listRunsResult{}, err
	}
	out := listRunsResult{Runs: []listedRun{}, NextCursor: page.NextCursor}
	for _, r := range page.Runs {
		out.Runs = append(out.Runs, listedRun{RunID: r.ID, Mode: r.Mode, State: r.State, Created: r.Created, Error: r.Error, Summary: r.Listing, Kept: r.Kept, WebURL: web.run(r.ID)})
	}
	return out, nil
}

func addRunTools(server *sdk.Server, service *platform.Service, web webLinks) {
	addTool(server, "linea_run", "Submit a quick or scenario run. Wait only observes; the run continues after a timeout or disconnect.",
		func(ctx context.Context, in debugRunInput) (summarized, error) {
			req, seconds, err := in.platform()
			if err != nil {
				return nil, err
			}
			run, err := service.SubmitRun(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			run, err = waitRun(ctx, service, run.ID, seconds)
			if err != nil {
				return nil, err
			}
			return runView(run, web), nil
		})
	addTool(server, "linea_list_runs", "List retained runs newest first, each with its summary once it has a report: scenario, runtime, how execution ended, inputs injected, truncation and assertion counts. Filter by scenario, runtime or result.",
		func(ctx context.Context, in listRunsInput) (summarized, error) {
			return listRuns(ctx, service, web, in)
		})
	addTool(server, "linea_get_run", "Read a run's current state, mode, timestamps and interruption flag.",
		func(ctx context.Context, in runInput) (summarized, error) {
			return getRun(ctx, service, web, in.RunID)
		})
	addTool(server, "linea_get_run_log", "Read the tail of a failed or partial run's Collector log (64 KB, info level and above, environment values redacted).",
		func(ctx context.Context, in runInput) (summarized, error) {
			log, err := service.GetRunLog(ctx, callerOf(ctx), in.RunID)
			if err != nil {
				return nil, err
			}
			return runLogResult{*log}, nil
		})
	addTool(server, "linea_cancel_run", "Request cancellation of a run and report whether the request took effect.",
		func(ctx context.Context, in runInput) (summarized, error) {
			result, err := service.CancelRun(ctx, callerOf(ctx), in.RunID)
			if err != nil {
				return nil, err
			}
			return cancelRunResult{RunID: result.Run.ID, State: result.Run.State, Effective: result.Effective}, nil
		})
	addTool(server, "linea_get_test_results", "Read a run's or a suite's recorded results.",
		func(ctx context.Context, in testResultsInput) (summarized, error) {
			if (in.RunID == nil) == (in.SuiteID == nil) {
				return nil, invalidRequest("supply exactly one of run_id or suite_id")
			}
			if in.SuiteID != nil {
				if *in.SuiteID == "" {
					return nil, invalidRequest("suite_id must not be empty")
				}
				suite, err := service.GetSuite(ctx, callerOf(ctx), *in.SuiteID)
				if err != nil {
					return nil, err
				}
				return suiteView(suite), nil
			}
			if *in.RunID == "" {
				return nil, invalidRequest("run_id must not be empty")
			}
			report, err := service.GetRunReport(ctx, callerOf(ctx), *in.RunID, false)
			if err != nil {
				return nil, err
			}
			run, err := service.GetRun(ctx, callerOf(ctx), *in.RunID)
			if err != nil {
				return nil, err
			}
			out := testResultsResult{RunID: run.ID, Mode: run.Mode, Assertions: report.Assertions}
			if report.Assertions == nil {
				out.Assertions = "none"
			}
			return out, nil
		})
}

func getRun(ctx context.Context, service *platform.Service, web webLinks, id string) (getRunResult, error) {
	run, err := service.GetRun(ctx, callerOf(ctx), id)
	if err != nil {
		return getRunResult{}, err
	}
	return getRunResult{runResult: runView(run, web), Created: run.Created, Started: run.Started,
		Finished: run.Finished, Interrupted: run.Interrupted, Kept: run.Kept}, nil
}

// Waiting only observes; the ordinary context reads the authoritative state
// even when the bounded report wait times out or retrieval fails.
func waitRun(ctx context.Context, service *platform.Service, id string, seconds int) (platform.Run, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	_, _ = service.GetRunReport(waitCtx, callerOf(waitCtx), id, true)
	return service.GetRun(ctx, callerOf(ctx), id)
}
