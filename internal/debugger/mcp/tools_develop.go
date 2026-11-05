package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

type statementEditInput struct {
	Processor string `json:"processor" jsonschema:"Configured transform processor ID, for example transform/redact."`
	Signal    string `json:"signal" jsonschema:"Statement signal: traces, metrics or logs."`
	Group     int    `json:"group" jsonschema:"Zero-based statement group index in the processor's list."`
	Index     int    `json:"index" jsonschema:"Zero-based statement index in the group."`
	Op        string `json:"op" jsonschema:"Edit operation: replace, insert, append or delete."`
	Text      string `json:"text,omitempty" jsonschema:"New statement text for replace, insert or append."`
	Expected  string `json:"expected,omitempty" jsonschema:"Current statement text; required for replace and delete to detect stale edits."`
}

func (e *statementEditInput) platform() (*platform.StatementEdit, error) {
	edit := &platform.StatementEdit{Processor: e.Processor, Signal: e.Signal, Group: e.Group, Index: e.Index, Text: e.Text, Expected: e.Expected}
	// The alias's operation type is intentionally not an adapter import.
	switch e.Op {
	case "replace":
		edit.Op = "replace"
	case "insert":
		edit.Op = "insert"
	case "append":
		edit.Op = "append"
	case "delete":
		edit.Op = "delete"
	default:
		return nil, invalidRequest("op must be replace, insert, append or delete")
	}
	return edit, nil
}

type testStatementInput struct {
	Config         source              `json:"config,omitempty" jsonschema:"Base Collector configuration source; never written. Required unless run_id is supplied."`
	RunID          string              `json:"run_id,omitempty" jsonschema:"Optional plain run ID: test on that run's recorded configuration and inputs as a scenario run, on the runtime its report names unless runtime is supplied. Excludes config, input_json, scenario and the quick-only fields."`
	Edit           *statementEditInput `json:"edit,omitempty" jsonschema:"Addressed statement edit; supply exactly one of edit or proposed_yaml."`
	ProposedYAML   *string             `json:"proposed_yaml,omitempty" jsonschema:"Complete proposed configuration YAML; supply exactly one of proposed_yaml or edit."`
	InputJSON      *string             `json:"input_json,omitempty" jsonschema:"Inline OTLP JSON text for quick mode; supply exactly one of input_json or scenario."`
	Scenario       *source             `json:"scenario,omitempty" jsonschema:"Scenario source; mutually exclusive with input_json and quick-only fields."`
	SetupYAML      *string             `json:"setup_yaml,omitempty" jsonschema:"Optional quick-only setup YAML."`
	Receiver       *string             `json:"receiver,omitempty" jsonschema:"Optional quick-only injection receiver ID."`
	ClientAddress  *string             `json:"client_address,omitempty" jsonschema:"Optional quick-only sender IP address."`
	AssertionsYAML *string             `json:"assertions_yaml,omitempty" jsonschema:"Assertions-only scenario v1 YAML; quick assertions or replacement scenario assertions."`
	WaitSeconds    *int                `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait, from 0 to 120; defaults to 30. Waiting never cancels the run."`
	Runtime        string              `json:"runtime,omitempty" jsonschema:"Optional runtime name, image reference or ID from linea_list_runtimes; empty or built-in is the debugger's own Collector."`
}

func (in testStatementInput) platform() (platform.StatementTestRequest, int, error) {
	if (in.Edit == nil) == (in.ProposedYAML == nil) {
		return platform.StatementTestRequest{}, 0, invalidRequest("supply exactly one of edit or proposed_yaml")
	}
	if in.RunID != "" {
		return in.onRun()
	}
	run, seconds, err := (debugRunInput{Config: in.Config, InputJSON: in.InputJSON, Scenario: in.Scenario,
		SetupYAML: in.SetupYAML, Receiver: in.Receiver, ClientAddress: in.ClientAddress, WaitSeconds: in.WaitSeconds}).platform()
	if err != nil {
		return platform.StatementTestRequest{}, 0, err
	}
	req := platform.StatementTestRequest{Candidate: platform.CandidateRequest{Base: run.Config, Runtime: in.Runtime}, Mode: run.Mode, Quick: run.Quick, Scenario: run.Scenario}
	if e := in.Edit; e != nil {
		var err error
		if req.Candidate.Statement, err = e.platform(); err != nil {
			return platform.StatementTestRequest{}, 0, err
		}
	} else {
		req.Candidate.YAML = *in.ProposedYAML
	}
	if in.AssertionsYAML != nil {
		if req.Quick != nil {
			req.Quick.Assertions = *in.AssertionsYAML
		} else {
			req.Assertions = *in.AssertionsYAML
		}
	}
	return req, seconds, nil
}

// onRun maps a statement test on a run's recorded inputs;
// the platform refuses a base or input alongside it.
func (in testStatementInput) onRun() (platform.StatementTestRequest, int, error) {
	if in.Config.Text != nil || in.Config.Artifact != nil || in.InputJSON != nil || in.Scenario != nil ||
		in.SetupYAML != nil || in.Receiver != nil || in.ClientAddress != nil {
		return platform.StatementTestRequest{}, 0, invalidRequest("run_id replaces config, input_json, scenario and the quick-only fields")
	}
	seconds, err := waitSeconds(in.WaitSeconds)
	if err != nil {
		return platform.StatementTestRequest{}, 0, err
	}
	req := platform.StatementTestRequest{RunID: in.RunID, Candidate: platform.CandidateRequest{Runtime: in.Runtime}}
	if e := in.Edit; e != nil {
		if req.Candidate.Statement, err = e.platform(); err != nil {
			return platform.StatementTestRequest{}, 0, err
		}
	} else {
		req.Candidate.YAML = *in.ProposedYAML
	}
	if in.AssertionsYAML != nil {
		req.Assertions = *in.AssertionsYAML
	}
	return req, seconds, nil
}

type testStatementResult struct {
	Valid             bool                  `json:"valid"`
	Diagnostics       []platform.Diagnostic `json:"diagnostics"`
	Patch             string                `json:"patch"`
	AffectedPipelines []string              `json:"affected_pipelines"`
	ConfigChanged     bool                  `json:"config_changed"`
	HasAssertions     bool                  `json:"has_assertions"`
	Run               *runResult            `json:"run,omitempty"`
	Statements        *statementCounts      `json:"statements,omitempty"`
}

func (r testStatementResult) summary() string {
	if r.Run == nil {
		return "Statement test: candidate invalid; " + diagnosticCounts(r.Diagnostics) + "; no run."
	}
	outcome := "none (not a pass)"
	if r.HasAssertions {
		// A pending or failed run has no assertion outcome yet. Do not invent one.
		outcome = "none"
		if r.Run.Summary != nil && r.Run.Summary.Assertions != nil {
			outcome = string(r.Run.Summary.Assertions.Outcome)
		}
	}
	return fmt.Sprintf("Statement test: run %s%s; assertions %s.", r.Run.State, r.Run.facts(), outcome)
}

type statementCounts struct {
	Processor    string           `json:"processor"`
	Inputs       int              `json:"inputs"`
	Verification string           `json:"verification"`
	Groups       []statementGroup `json:"groups"`
}

type statementGroup struct {
	Index      int             `json:"index"`
	Context    string          `json:"context"`
	Statements []statementStat `json:"statements"`
}

type statementStat struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	Matched      int    `json:"matched"`
	NotMatched   int    `json:"not_matched"`
	NotEvaluated int    `json:"not_evaluated"`
	Errors       int    `json:"errors"`
}

// Counts preserve first-seen report order and sum only recorded executions.
// The platform alias exposes fields but not ottldebug's verification constants.
func countStatements(processor string, reports []*platform.OTTLReport) *statementCounts {
	out := &statementCounts{Processor: processor, Verification: "none", Groups: []statementGroup{}}
	groups := map[int]int{}
	statements := map[[2]int]int{}
	for _, report := range reports {
		if report == nil || report.Processor != processor {
			continue
		}
		out.Inputs++
		switch {
		case report.Verification == "disagrees":
			out.Verification = "disagrees"
		case report.Verification == "not_verifiable" && out.Verification != "disagrees":
			out.Verification = "not_verifiable"
		case out.Verification == "none":
			out.Verification = "replay_verified"
		}
		for _, g := range report.Groups {
			gi, exists := groups[g.Index]
			if !exists {
				gi = len(out.Groups)
				groups[g.Index] = gi
				out.Groups = append(out.Groups, statementGroup{Index: g.Index, Context: g.Context, Statements: []statementStat{}})
			}
			group := &out.Groups[gi]
			for _, s := range g.Statements {
				key := [2]int{g.Index, s.Index}
				si, exists := statements[key]
				if !exists {
					si = len(group.Statements)
					statements[key] = si
					group.Statements = append(group.Statements, statementStat{Index: s.Index, Text: s.Text})
				}
				stat := &group.Statements[si]
				stat.Matched += s.Matched
				stat.NotMatched += s.NotMatched
				stat.NotEvaluated += s.NotEvaluated
				stat.Errors += len(s.Errors)
			}
		}
	}
	return out
}

type createScenarioInput struct {
	RunID          string `json:"run_id" jsonschema:"Plain run ID from which to derive a scenario."`
	Name           string `json:"name,omitempty" jsonschema:"Optional replacement scenario name."`
	AssertionsYAML string `json:"assertions_yaml,omitempty" jsonschema:"Optional assertions-only YAML appended after the run's own assertions."`
	Save           bool   `json:"save,omitempty" jsonschema:"Save a new scenario artifact when a workspace is available."`
	Label          string `json:"label,omitempty" jsonschema:"Optional saved-artifact label."`
}

type createScenarioResult struct {
	RunID               string                  `json:"run_id"`
	YAML                string                  `json:"yaml"`
	RequiredEnvironment []string                `json:"required_environment"`
	Saved               *platform.SavedArtifact `json:"saved,omitempty"`
	// Export is the YAML after a comment header naming the source run, build,
	// required environment names, reproducibility and what was left out: the
	// file to write for CI. A saved revision holds the YAML only.
	Export string `json:"export"`
}

func (r createScenarioResult) summary() string {
	return fmt.Sprintf("Scenario created from run: %d required environment names; saved %t.", len(r.RequiredEnvironment), r.Saved != nil)
}

type runScenariosInput struct {
	Config      source   `json:"config" jsonschema:"Collector configuration source shared by all scenarios."`
	Scenarios   []source `json:"scenarios" jsonschema:"Scenario sources in member order; from 1 to 32 entries."`
	WaitSeconds *int     `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for the whole suite, from 0 to 120; defaults to 30. Waiting never cancels members."`
	Runtime     string   `json:"runtime,omitempty" jsonschema:"Optional runtime name, image reference or ID from linea_list_runtimes for every member; empty or built-in is the debugger's own Collector."`
}

type rerunSuiteInput struct {
	SuiteID     string `json:"suite_id" jsonschema:"The suite to rerun, from linea_run_scenarios."`
	WaitSeconds *int   `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for the new suite, from 0 to 120; defaults to 30. Waiting never cancels members."`
}

type suiteJUnitInput struct {
	SuiteID string `json:"suite_id" jsonschema:"A finished suite, from linea_run_scenarios or linea_rerun_suite."`
}

type suiteJUnitResult struct {
	SuiteID string `json:"suite_id"`
	JUnit   string `json:"junit"`
}

func (r suiteJUnitResult) summary() string {
	return fmt.Sprintf("Suite JUnit: %d bytes of XML.", len(r.JUnit))
}

type suiteResult struct {
	SuiteID string              `json:"suite_id"`
	Done    bool                `json:"done"`
	Outcome assertion.Outcome   `json:"outcome,omitempty"`
	Config  string              `json:"config"`
	Members []suiteMemberResult `json:"members"`
}

type suiteMemberResult struct {
	Label   string            `json:"label"`
	RunID   string            `json:"run_id,omitempty"`
	State   platform.State    `json:"state,omitempty"`
	Name    string            `json:"name,omitempty"`
	Outcome assertion.Outcome `json:"outcome,omitempty"`
	Message string            `json:"message,omitempty"`
	Error   string            `json:"error,omitempty"`
}

func suiteView(s *platform.SuiteStatus) suiteResult {
	out := suiteResult{SuiteID: s.ID, Done: s.Done, Outcome: s.Outcome, Config: s.Config, Members: []suiteMemberResult{}}
	for _, m := range s.Members {
		out.Members = append(out.Members, suiteMemberResult{Label: m.Label, RunID: m.RunID, State: m.State, Name: m.Name, Outcome: m.Outcome, Message: m.Message, Error: m.Error})
	}
	return out
}

func (r suiteResult) summary() string {
	counts := map[assertion.Outcome]int{}
	for _, m := range r.Members {
		counts[m.Outcome]++
	}
	text := fmt.Sprintf("Scenario suite: %d members; done %t", len(r.Members), r.Done)
	if r.Done {
		text += "; outcome " + string(r.Outcome)
	}
	return text + fmt.Sprintf("; %d pass, %d fail, %d inconclusive, %d error, %d pending.", counts[assertion.Pass], counts[assertion.Fail], counts[assertion.Inconclusive], counts[assertion.Error], counts[""])
}

func addDevelopmentTools(server *sdk.Server, service *platform.Service, web webLinks) {
	addTool(server, "linea_test_statement", "Test an unsaved configuration proposal through a real quick or scenario run, or with run_id on a retained run's recorded configuration and inputs. Assertions declare the expected output; a run without assertions is never a pass. The base configuration is never changed. Waiting only observes.",
		func(ctx context.Context, in testStatementInput) (summarized, error) {
			req, seconds, err := in.platform()
			if err != nil {
				return nil, err
			}
			test, err := service.TestStatement(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			c := test.Candidate
			out := testStatementResult{Valid: c.Valid, Diagnostics: append([]platform.Diagnostic{}, c.Diagnostics...), Patch: c.Patch,
				AffectedPipelines: append([]string{}, c.AffectedPipelines...), HasAssertions: test.HasAssertions}
			if test.Run == nil {
				return out, nil
			}
			run, err := waitRun(ctx, service, test.Run.ID, seconds)
			if err != nil {
				return nil, err
			}
			view := runView(run, web)
			out.Run = &view
			if in.Edit != nil && view.Summary != nil {
				if changes, err := service.GetStageChanges(ctx, callerOf(ctx), run.ID); err == nil {
					out.Statements = countStatements(in.Edit.Processor, changes.OTTL)
				}
			}
			return out, nil
		})
	addTool(server, "linea_create_scenario", "Create self-contained scenario YAML from a run, with inputs and mock files inlined. Host-imported environment values are never copied, only named. export is the same YAML after a header comment, the reviewable file to commit for CI (inline inputs are recorded telemetry). The run is unchanged.",
		func(ctx context.Context, in createScenarioInput) (summarized, error) {
			if in.RunID == "" {
				return nil, invalidRequest("run_id is required")
			}
			s, err := service.CreateScenarioFromRun(ctx, callerOf(ctx), in.RunID, platform.ScenarioFromRunRequest{Name: in.Name, Assertions: in.AssertionsYAML, Save: in.Save, Label: in.Label})
			if err != nil {
				return nil, err
			}
			return createScenarioResult{RunID: in.RunID, YAML: s.YAML, RequiredEnvironment: append([]string{}, s.RequiredEnvironment...), Saved: s.Saved, Export: s.Export}, nil
		})
	addTool(server, "linea_run_scenarios", "Submit an ordered scenario suite. Waiting only observes; timeout or disconnect never cancels members. After a timeout, read the suite with linea_get_test_results using suite_id.",
		func(ctx context.Context, in runScenariosInput) (summarized, error) {
			seconds, err := waitSeconds(in.WaitSeconds)
			if err != nil {
				return nil, err
			}
			if len(in.Scenarios) < 1 || len(in.Scenarios) > platform.MaxSuiteMembers {
				return nil, invalidRequest("scenarios must contain 1 to 32 entries")
			}
			config, err := in.Config.platform()
			if err != nil {
				return nil, err
			}
			members := make([]platform.ScenarioInput, len(in.Scenarios))
			for i := range in.Scenarios {
				member, err := scenarioSource(&in.Scenarios[i])
				if err != nil {
					return nil, err
				}
				members[i] = *member
			}
			suite, err := service.SubmitSuite(ctx, callerOf(ctx), platform.SubmitSuiteRequest{Config: config, Members: members, Runtime: in.Runtime})
			if err != nil {
				return nil, err
			}
			return awaitSuite(ctx, service, suite, seconds)
		})
	addTool(server, "linea_rerun_suite", "Rerun a suite with its recorded inputs: the configuration and each member's self-contained scenario, on the runtime a member's report names. No file is read again. Refused when a member was never admitted or read environment values. Waiting only observes.",
		func(ctx context.Context, in rerunSuiteInput) (summarized, error) {
			seconds, err := waitSeconds(in.WaitSeconds)
			if err != nil {
				return nil, err
			}
			suite, err := service.RerunSuite(ctx, callerOf(ctx), in.SuiteID)
			if err != nil {
				return nil, err
			}
			return awaitSuite(ctx, service, suite, seconds)
		})
	addTool(server, "linea_get_suite_junit", "A finished suite's test report as JUnit XML: one testsuite per member and one testcase per assertion; inconclusive is a failure.",
		func(ctx context.Context, in suiteJUnitInput) (summarized, error) {
			raw, err := service.GetSuiteJUnit(ctx, callerOf(ctx), in.SuiteID)
			if err != nil {
				return nil, err
			}
			return suiteJUnitResult{SuiteID: in.SuiteID, JUnit: string(raw)}, nil
		})
}

// awaitSuite waits up to seconds for a suite's members, then reads its
// status. Ending the wait never cancels a member.
func awaitSuite(ctx context.Context, service *platform.Service, suite *platform.Suite, seconds int) (summarized, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	for _, m := range suite.Members {
		if waitCtx.Err() != nil {
			break
		}
		if m.RunID != "" {
			_, _ = service.GetRunReport(waitCtx, callerOf(waitCtx), m.RunID, true)
		}
	}
	status, err := service.GetSuite(ctx, callerOf(ctx), suite.ID)
	if err != nil {
		return nil, err
	}
	return suiteView(status), nil
}
