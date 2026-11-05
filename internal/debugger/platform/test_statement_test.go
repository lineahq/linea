package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/statementedit"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const statementTestConfig = `receivers:
  otlp:
    protocols:
      grpc: {}
processors:
  transform/redact:
    log_statements:
      - 'set(log.attributes["marker"], true)'
exporters:
  otlp/backend: {endpoint: example.invalid:4317}
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [transform/redact]
      exporters: [otlp/backend]
`

const statementTestFixture = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"customer event"},"attributes":[{"key":"customer.email","value":{"stringValue":"customer@example.test"}},{"key":"other","value":{"stringValue":"kept"}}]}]}]}]}`
const statementTestAssertions = `assertions:
  - name: absent at exporter
    target: {exporter: otlp/backend, signal: logs}
    count: {equals: 1}
    attributes: {absent: [customer.email]}
`
const statementTestOld = `set(log.attributes["marker"], true)`
const statementTestDelete = `delete_key(log.attributes, "customer.email")`

func statementTestRequest(mode InputMode) StatementTestRequest {
	edit := &StatementEdit{Processor: "transform/redact", Signal: "logs", Op: statementedit.Replace, Expected: statementTestOld, Text: statementTestDelete}
	req := StatementTestRequest{Candidate: CandidateRequest{Base: Source{Text: statementTestConfig}, Statement: edit}, Mode: mode}
	if mode == ModeQuick {
		req.Quick = &QuickInput{Fixture: Source{Text: statementTestFixture}, Receiver: "otlp", ClientAddress: "10.0.0.9", Setup: "execution: {timestamps: recorded}\n", Assertions: statementTestAssertions}
	} else {
		req.Scenario = &ScenarioInput{Scenario: Source{Text: "version: 1\ninputs: [{receiver: otlp, data: '" + statementTestFixture + "'}]\n" + statementTestAssertions}}
	}
	return req
}

func TestStatementUsesRealRunsInBothModes(t *testing.T) {
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		for _, behavior := range []string{"redact", "insert", "complete YAML", "wrong key", "restored later", "no assertions"} {
			t.Run(string(mode)+"/"+behavior, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				s := New(Options{})
				defer s.Close()
				req := statementTestRequest(mode)
				want := assertion.Pass
				switch behavior {
				case "insert":
					req.Candidate.Statement.Op = statementedit.Insert
					req.Candidate.Statement.Expected = ""
				case "complete YAML":
					req.Candidate.Statement = nil
					req.Candidate.YAML = strings.Replace(statementTestConfig, statementTestOld, statementTestDelete, 1)
				case "wrong key":
					req.Candidate.Statement.Text = `delete_key(log.attributes, "other")`
					want = assertion.Fail
				case "restored later":
					req.Candidate.Base.Text = strings.Replace(statementTestConfig, "exporters:\n", "  transform/restore:\n    log_statements:\n      - 'set(log.attributes[\"customer.email\"], \"restored\")'\nexporters:\n", 1)
					req.Candidate.Base.Text = strings.Replace(req.Candidate.Base.Text, "processors: [transform/redact]", "processors: [transform/redact, transform/restore]", 1)
					// Demonstrate that the edited processor succeeds but the final
					// boundary still fails after the downstream restoration.
					processorAssertion := "  - name: absent after redact\n    target: {processor: transform/redact, signal: logs}\n    attributes: {absent: [customer.email]}\n"
					if mode == ModeQuick {
						req.Quick.Assertions += processorAssertion
					} else {
						req.Scenario.Scenario.Text += processorAssertion
					}
					want = assertion.Fail
				case "no assertions":
					if mode == ModeQuick {
						req.Quick.Assertions = ""
					} else {
						req.Scenario.Scenario.Text = strings.TrimSuffix(req.Scenario.Scenario.Text, statementTestAssertions)
					}
				}
				out, err := s.TestStatement(ctx, testCaller, req)
				if err != nil || out == nil || out.Candidate == nil || !out.Candidate.Valid || out.Run == nil || out.Candidate.Saved != nil {
					t.Fatalf("test = %+v, %v", out, err)
				}
				if out.Run.Mode != mode || out.Run.Caller != testCaller.Actor() || out.HasAssertions != (behavior != "no assertions") {
					t.Fatalf("run or output boundary = %+v", out)
				}
				rep, err := s.GetRunReport(ctx, testCaller, out.Run.ID, true)
				if err != nil {
					t.Fatal(err)
				}
				if rep.Result.Status != runresult.StatusCompleted {
					t.Fatalf("execution status = %s", rep.Result.Status)
				}
				if behavior == "no assertions" {
					if rep.Assertions != nil || bytes.Contains(evaluationJSON(t, rep), []byte(`"assertions":`)) {
						t.Fatal("no declared boundary acquired an assertion outcome")
					}
				} else if rep.Assertions == nil || rep.Assertions.Outcome != want {
					t.Fatalf("assertions = %+v, want %s", rep.Assertions, want)
				}
				if behavior == "restored later" && (rep.Assertions.Results[0].Outcome != assertion.Fail || rep.Assertions.Results[1].Outcome != assertion.Pass) {
					t.Fatalf("processor/exporter outcomes = %+v", rep.Assertions.Results)
				}
				if mode == ModeQuick {
					bundle, err := s.GetRunBundle(ctx, testCaller, out.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					parts, err := engine.ReadBundleParts(bundle)
					if err != nil || string(parts.Fixture) != req.Quick.Fixture.Text || string(parts.Setup) != req.Quick.Setup || string(parts.Assertions) != req.Quick.Assertions || parts.Receiver != req.Quick.Receiver || parts.ClientAddress != req.Quick.ClientAddress {
						t.Fatalf("quick input changed: %+v, %v", parts, err)
					}
				}
			})
		}
	}
}

func TestStatementScenarioFileReplacesAssertionsAndCapturesRelativeInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	configPath, scenarioPath, inputPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml"), filepath.Join(dir, "input.json")
	// Replacing (not appending) must remove this deliberately failing assertion.
	original := "version: 1\nname: relative input\ninputs: [{file: input.json}]\nassertions: [{target: {exporter: otlp/backend, signal: logs}, count: {equals: 99}}]\n"
	for path, raw := range map[string]string{configPath: statementTestConfig, scenarioPath: original, inputPath: statementTestFixture} {
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	policy := engine.Policy{Paths: PathPolicy{Roots: []string{dir}}}
	s, err := openMemory(memory.NewData(), Options{Paths: policy.Paths, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		// At admission, replace the source on disk with invalid text: preparation
		// must consume the already-read inline root, not reopen this path.
		if req.Scenario != "" || req.ScenarioYAML == "" || req.ScenarioDir != dir {
			t.Fatal("scenario root was not captured inline with the file directory")
		}
		if err := os.WriteFile(scenarioPath, []byte("version: 99\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.WriteFile(scenarioPath, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
		}()
		return engine.PrepareWithPolicy(ctx, req, policy)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: s.repository.Info().ID, Kind: artifact.Configuration, Label: "base", Content: []byte(statementTestConfig)})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.ListArtifacts(ctx, testCaller, ListArtifactsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"path", "artifact bytes", "inline scenario"} {
		req := statementTestRequest(ModeScenario)
		req.Candidate.Base = Source{Path: configPath}
		if source == "artifact bytes" {
			revision, err := s.GetArtifactRevision(ctx, testCaller, base.Artifact.ID, base.Artifact.Head)
			if err != nil {
				t.Fatal(err)
			}
			req.Candidate.Base = Source{Text: string(revision.Content)}
		}
		req.Scenario = &ScenarioInput{Scenario: Source{Path: scenarioPath}, Dir: "ignored-for-file"}
		if source == "inline scenario" {
			req.Scenario = &ScenarioInput{Scenario: Source{Text: original}, Dir: dir}
		}
		req.Assertions = statementTestAssertions
		out, err := s.TestStatement(ctx, testCaller, req)
		if err != nil || out == nil || out.Run == nil || !out.HasAssertions || out.Candidate.Saved != nil {
			t.Fatalf("test = %+v, %v", out, err)
		}
		rep, err := s.GetRunReport(ctx, testCaller, out.Run.ID, true)
		if err != nil || rep.Assertions == nil || rep.Assertions.Outcome != assertion.Pass || len(rep.Assertions.Results) != 1 {
			t.Fatalf("replacement report = %+v, %v", rep, err)
		}
		bundle, err := s.GetRunBundle(ctx, testCaller, out.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := engine.ReadBundleParts(bundle)
		if err != nil || parts.ScenarioDir != dir || string(parts.Files[inputPath]) != statementTestFixture {
			t.Fatalf("relative input capture = %+v, %v", parts, err)
		}
		parsed, err := scenario.Parse(parts.Scenario, parts.ScenarioDir)
		if err != nil || parsed.Name != "relative input" || len(parsed.Assertions) != 1 || parsed.Assertions[0].Name != "absent at exporter" {
			t.Fatalf("captured scenario = %+v, %v", parsed, err)
		}
	}
	after, err := s.ListArtifacts(ctx, testCaller, ListArtifactsRequest{})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("statement tests changed source artifacts", err)
	}
	for path, want := range map[string]string{configPath: statementTestConfig, scenarioPath: original, inputPath: statementTestFixture} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("source file %s changed: %v", path, err)
		}
	}
}

func TestStatementRefusalsAdmitNothing(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		edit func(*StatementTestRequest)
		code Code
	}{
		{"stale", func(r *StatementTestRequest) { r.Candidate.Statement.Expected = "stale" }, CodeConflict},
		{"save", func(r *StatementTestRequest) { r.Candidate.Save = true; r.Candidate.Base = Source{Path: "never-read"} }, CodeInvalidRequest},
		{"no mode", func(r *StatementTestRequest) { r.Mode = "" }, CodeInvalidRequest},
		{"quick without input", func(r *StatementTestRequest) { r.Quick = nil }, CodeInvalidRequest},
		{"scenario without input", func(r *StatementTestRequest) { r.Mode = ModeScenario; r.Quick = nil }, CodeInvalidRequest},
		{"both", func(r *StatementTestRequest) { r.Scenario = statementTestRequest(ModeScenario).Scenario }, CodeInvalidRequest},
		{"wrong mode", func(r *StatementTestRequest) { r.Mode = ModeScenario }, CodeInvalidRequest},
		{"quick replacement", func(r *StatementTestRequest) { r.Assertions = statementTestAssertions }, CodeInvalidRequest},
		{"invalid quick assertions", func(r *StatementTestRequest) { r.Quick.Assertions = "assertions: []" }, CodeInvalidRequest},
		{"invalid replacement", func(r *StatementTestRequest) { *r = statementTestRequest(ModeScenario); r.Assertions = "inputs: []" }, CodeInvalidRequest},
		{"invalid scenario", func(r *StatementTestRequest) {
			*r = statementTestRequest(ModeScenario)
			r.Scenario.Scenario.Text = "version: 99"
		}, CodeInvalidRequest},
		{"scenario both sources", func(r *StatementTestRequest) {
			*r = statementTestRequest(ModeScenario)
			r.Scenario.Scenario.Path = "never-read"
		}, CodeInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := allRuns(ctx, s, testCaller, RunFilter{})
			if err != nil {
				t.Fatal(err)
			}
			req := statementTestRequest(ModeQuick)
			tc.edit(&req)
			out, err := s.TestStatement(ctx, testCaller, req)
			wantCode(t, err, tc.code)
			if out != nil {
				t.Fatal("failed operation returned a result")
			}
			if tc.name == "stale" && (err.(*Error).CurrentStatement != statementTestOld || strings.Contains(err.Error(), statementTestOld)) {
				t.Fatalf("conflict = %+v", err)
			}
			if tc.name == "invalid replacement" && !strings.HasPrefix(err.Error(), "assertions: ") {
				t.Fatal(err)
			}
			after, err := allRuns(ctx, s, testCaller, RunFilter{})
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refusal admitted a run", err)
			}
		})
	}
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		req := statementTestRequest(mode)
		req.Candidate.Statement.Text = "unknown_function()"
		// Invalid candidates return before any attempt to read the input.
		if mode == ModeScenario {
			req.Scenario = &ScenarioInput{Scenario: Source{Path: "never-read"}}
		}
		out, err := s.TestStatement(ctx, testCaller, req)
		if err != nil || out == nil || out.Candidate.Valid || len(out.Candidate.Diagnostics) == 0 || out.Run != nil || out.HasAssertions {
			t.Fatalf("invalid candidate = %+v, %v", out, err)
		}
	}
	if runs, err := allRuns(ctx, s, testCaller, RunFilter{}); err != nil || len(runs) != 0 {
		t.Fatal("invalid candidates admitted runs", err)
	}
	_, err := s.TestStatement(ctx, Caller{}, statementTestRequest(ModeQuick))
	wantCode(t, err, CodeInvalidRequest)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.TestStatement(cancelled, testCaller, statementTestRequest(ModeScenario))
	wantCode(t, err, CodeCancelled)
}

func TestStatementScenarioPathUsesHostPolicy(t *testing.T) {
	allowed, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(outside, "scenario.yaml")
	if err := os.WriteFile(path, []byte(statementTestRequest(ModeScenario).Scenario.Scenario.Text), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: PathPolicy{Roots: []string{allowed}}})
	defer s.Close()
	req := statementTestRequest(ModeScenario)
	req.Scenario = &ScenarioInput{Scenario: Source{Path: path}}
	_, err := s.TestStatement(context.Background(), testCaller, req)
	wantCode(t, err, CodeForbidden)
	if runs, err := allRuns(context.Background(), s, testCaller, RunFilter{}); err != nil || len(runs) != 0 {
		t.Fatal("forbidden scenario admitted a run", err)
	}
}

func TestStatementReturnsWithoutWaitingAndPreservesQueueErrors(t *testing.T) {
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			started := make(chan engine.Request, 1)
			s := New(Options{QueueCapacity: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				started <- req
				<-ctx.Done()
				return report(req, runresult.StatusCancelled), nil
			}})
			defer s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			out, err := s.TestStatement(ctx, testCaller, statementTestRequest(mode))
			if err != nil || out.Run == nil {
				t.Fatalf("submission = %+v, %v", out, err)
			}
			executing := receive(t, started)
			if executing.Config != "" || executing.ConfigYAML != out.Candidate.Proposed {
				t.Fatal("candidate was not submitted inline")
			}
			queued, err := s.TestStatement(ctx, testCaller, statementTestRequest(mode))
			if err != nil || queued.Run.State != StateQueued {
				t.Fatalf("queued = %+v, %v", queued, err)
			}
			_, err = s.TestStatement(ctx, testCaller, statementTestRequest(mode))
			wantCode(t, err, CodeResourceExhausted)
			if !err.(*Error).Retryable {
				t.Fatal("full queue lost retryability")
			}
		})
	}
}

// A statement test on a run reuses its recorded configuration, inputs and
// runtime in both recorded modes, and refuses anything that would replace
// them.
func TestStatementOnRunUsesItsRecordedInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := New(Options{})
	defer s.Close()
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			recorded := statementTestRequest(mode)
			run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: mode, Config: recorded.Candidate.Base, Quick: recorded.Quick, Scenario: recorded.Scenario})
			if err != nil {
				t.Fatal(err)
			}
			if rep, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil || rep.Assertions == nil || rep.Assertions.Outcome != assertion.Fail {
				t.Fatalf("recorded run = %+v, %v", rep, err)
			}
			req := StatementTestRequest{Candidate: CandidateRequest{Statement: recorded.Candidate.Statement}, RunID: run.ID}
			out, err := s.TestStatement(ctx, testCaller, req)
			if err != nil || !out.Candidate.Valid || out.Run == nil || out.Run.Mode != ModeScenario || !out.HasAssertions {
				t.Fatalf("test = %+v, %v", out, err)
			}
			if !strings.Contains(out.Candidate.Patch, statementTestDelete) {
				t.Fatalf("patch = %s", out.Candidate.Patch)
			}
			rep, err := s.GetRunReport(ctx, testCaller, out.Run.ID, true)
			if err != nil || rep.Assertions == nil || rep.Assertions.Outcome != assertion.Pass {
				t.Fatalf("tested run = %+v, %v", rep, err)
			}

			// Replacement assertions apply to the recorded inputs.
			req.Assertions = strings.Replace(statementTestAssertions, "equals: 1", "equals: 2", 1)
			if out, err = s.TestStatement(ctx, testCaller, req); err != nil || out.Run == nil {
				t.Fatalf("replaced = %+v, %v", out, err)
			}
			if rep, err = s.GetRunReport(ctx, testCaller, out.Run.ID, true); err != nil || rep.Assertions == nil || rep.Assertions.Outcome != assertion.Fail {
				t.Fatalf("replaced assertions = %+v, %v", rep, err)
			}

			// The test ran the recorded inputs inlined in one scenario, with
			// other assertions, so it compares with the run record by record.
			c, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: run.ID, After: out.Run.ID})
			if err != nil || !c.InputsIdentical || c.Records == nil || slices.ContainsFunc(c.Reasons, func(r ComparisonReason) bool { return r.Code == ComparisonInputsDiffer }) {
				t.Fatalf("comparison = %+v, %v", c, err)
			}
		})
	}

	run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeScenario, Config: Source{Text: statementTestConfig}, Scenario: statementTestRequest(ModeScenario).Scenario})
	if err != nil {
		t.Fatal(err)
	}
	edit := statementTestRequest(ModeQuick).Candidate.Statement
	for name, tc := range map[string]struct {
		req  StatementTestRequest
		code Code
	}{
		"base":     {StatementTestRequest{RunID: run.ID, Candidate: CandidateRequest{Base: Source{Text: statementTestConfig}, Statement: edit}}, CodeInvalidRequest},
		"mode":     {StatementTestRequest{RunID: run.ID, Candidate: CandidateRequest{Statement: edit}, Mode: ModeScenario}, CodeInvalidRequest},
		"scenario": {StatementTestRequest{RunID: run.ID, Candidate: CandidateRequest{Statement: edit}, Scenario: statementTestRequest(ModeScenario).Scenario}, CodeInvalidRequest},
		"unknown":  {StatementTestRequest{RunID: "run-missing", Candidate: CandidateRequest{Statement: edit}}, CodeNotFound},
	} {
		if out, err := s.TestStatement(ctx, testCaller, tc.req); ErrorCode(err) != tc.code || out != nil {
			t.Fatalf("%s: %+v, %v", name, out, err)
		}
	}
}
