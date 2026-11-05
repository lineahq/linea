package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
)

// TestStatement validates an unsaved candidate and submits it through the normal
// run path. Assertions are evaluated only by that run; this operation never waits.
func (s *Service) TestStatement(ctx context.Context, caller Caller, req StatementTestRequest) (_ *StatementTest, err error) {
	defer s.audit(&caller, "TestStatement", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if req.Candidate.Save {
		return nil, classify(errors.New("statement tests cannot save candidates"), CodeInvalidRequest)
	}
	if req.RunID != "" {
		if err := s.runIn(ctx, caller, authz.RunRead, req.RunID); err != nil {
			return nil, err
		}
		if err := s.useRecordedInputs(ctx, &req); err != nil {
			return nil, err
		}
	}
	switch req.Mode {
	case ModeQuick:
		if req.Quick == nil || req.Scenario != nil {
			return nil, classify(errMode, CodeInvalidRequest)
		}
		if req.Assertions != "" {
			return nil, classify(errors.New("quick assertions belong in Quick.Assertions"), CodeInvalidRequest)
		}
	case ModeScenario:
		if req.Scenario == nil || req.Quick != nil {
			return nil, classify(errMode, CodeInvalidRequest)
		}
	default:
		return nil, classify(errMode, CodeInvalidRequest)
	}
	candidate, err := s.CreateCandidate(ctx, caller, req.Candidate)
	if err != nil {
		return nil, err
	}
	out := &StatementTest{Candidate: candidate}
	if !candidate.Valid {
		return out, nil
	}
	runReq := RunRequest{Mode: req.Mode, Config: Source{Text: candidate.Proposed}, Quick: req.Quick, Runtime: req.Candidate.Runtime}
	if req.Mode == ModeQuick {
		out.HasAssertions = req.Quick.Assertions != ""
	} else {
		input := *req.Scenario
		if _, err := s.resolveSource(ctx, caller, &input.Scenario, artifact.Scenario, "scenario"); err != nil {
			return nil, err
		}
		raw, dir, err := engine.ReadScenarioSource(ctx, input.Scenario.Path, input.Scenario.Text, input.Dir, s.policy)
		if err != nil {
			return nil, classify(err, CodeValidationFailed)
		}
		if req.Assertions != "" {
			raw, err = scenario.ReplaceAssertions(raw, []byte(req.Assertions))
			if err != nil {
				return nil, classify(err, CodeInvalidRequest)
			}
		}
		parsed, err := scenario.Parse(raw, dir)
		if err != nil {
			return nil, classify(fmt.Errorf("%w: scenario_yaml: %v", engine.ErrInvalidRequest, err), CodeInvalidRequest)
		}
		out.HasAssertions = len(parsed.Assertions) != 0
		runReq.Scenario = &ScenarioInput{Scenario: Source{Text: string(raw)}, Dir: dir}
	}
	run, err := s.SubmitRun(ctx, caller, runReq)
	if err != nil {
		return nil, err
	}
	out.Run = &run
	return out, nil
}

// useRecordedInputs fills a statement test from a retained run's bundle: its
// configuration is the candidate's base and its inputs, as the self-contained
// scenario a rerun would use, are the test's input. Environment values are
// never recorded, so a run that read them cannot be repeated.
func (s *Service) useRecordedInputs(ctx context.Context, req *StatementTestRequest) error {
	base := req.Candidate.Base
	if base.Path != "" || base.Text != "" || base.Artifact != nil || base.Label != "" || req.Mode != "" || req.Quick != nil || req.Scenario != nil {
		return classify(errors.New("a run's recorded configuration and inputs replace the configuration, mode and input"), CodeInvalidRequest)
	}
	raw, err := s.getRunBundle(ctx, req.RunID)
	if err != nil {
		return err
	}
	parts, err := engine.ReadBundleParts(raw)
	if err != nil {
		e := newError(CodeStorageError, err)
		e.Message = "cannot read captured execution bundle"
		return e
	}
	if len(parts.HostEnvironment) != 0 {
		return &Error{Code: CodeConflict, Message: "run read environment values (" + strings.Join(parts.HostEnvironment, ", ") + "), which are never recorded"}
	}
	yaml, err := selfContainedScenario(parts, "", nil)
	if err != nil {
		return err
	}
	if req.Candidate.Runtime == "" {
		runtime, ok := s.reportRuntime(req.RunID)
		if !ok {
			return &Error{Code: CodeConflict, Message: "run has no report naming its runtime; name the runtime to test on"}
		}
		req.Candidate.Runtime = runtime
	}
	req.Candidate.Base = Source{Text: string(parts.Config), Label: parts.ConfigName}
	req.Mode = ModeScenario
	req.Scenario = &ScenarioInput{Scenario: Source{Text: string(yaml)}}
	return nil
}
