package platform

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// RerunSuite admits a new suite with an earlier suite's recorded inputs.
// Each member is rebuilt from its run's captured bundle: the
// configuration's root text and the self-contained scenario that
// CreateScenarioFromRun derives, so no scenario or input file is read from the
// host again. The runtime is the one a member's report names. Nothing new is
// persisted. A member that was never admitted, or whose configuration read
// environment values, refuses the rerun with a conflict.
func (s *Service) RerunSuite(ctx context.Context, caller Caller, id string) (_ *Suite, err error) {
	defer s.audit(&caller, "RerunSuite", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.suiteIn(caller, authz.RunRead, id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	suite, exists := s.suites[id]
	if !exists {
		err := s.unknownSuiteLocked(id)
		s.mu.Unlock()
		return nil, err
	}
	suite = cloneSuite(suite)
	s.mu.Unlock()
	req, err := s.rerunRequest(ctx, suite)
	if err != nil {
		return nil, err
	}
	return s.SubmitSuite(ctx, caller, req)
}

// rerunRequest rebuilds a suite request from its members' recorded inputs.
func (s *Service) rerunRequest(ctx context.Context, suite Suite) (SubmitSuiteRequest, error) {
	req := SubmitSuiteRequest{Members: make([]ScenarioInput, 0, len(suite.Members))}
	var config []byte
	runtimeKnown := false
	for _, member := range suite.Members {
		if member.RunID == "" {
			return SubmitSuiteRequest{}, rerunConflict(member, "was never admitted, so it recorded no inputs")
		}
		raw, err := s.getRunBundle(ctx, member.RunID)
		if err != nil {
			return SubmitSuiteRequest{}, err
		}
		parts, err := engine.ReadBundleParts(raw)
		if err != nil {
			e := newError(CodeStorageError, err)
			e.Message = "cannot read captured execution bundle"
			return SubmitSuiteRequest{}, e
		}
		if len(parts.HostEnvironment) != 0 {
			return SubmitSuiteRequest{}, rerunConflict(member, "read environment values ("+strings.Join(parts.HostEnvironment, ", ")+"), which are never recorded")
		}
		switch {
		case config == nil:
			config = parts.Config
			req.Config = Source{Text: string(parts.Config), Label: parts.ConfigName}
		case !bytes.Equal(config, parts.Config):
			return SubmitSuiteRequest{}, rerunConflict(member, "ran another configuration than the first member")
		}
		yaml, err := selfContainedScenario(parts, "", nil)
		if err != nil {
			return SubmitSuiteRequest{}, err
		}
		req.Members = append(req.Members, ScenarioInput{Scenario: Source{Text: string(yaml), Label: member.Label}})
		if !runtimeKnown {
			if rt, ok := s.reportRuntime(member.RunID); ok {
				req.Runtime, runtimeKnown = rt, true
			}
		}
	}
	if !runtimeKnown {
		return SubmitSuiteRequest{}, &Error{Code: CodeConflict, Message: "suite cannot be rerun: no member has a report naming its runtime"}
	}
	if err := ctx.Err(); err != nil {
		return SubmitSuiteRequest{}, classify(err, CodeCancelled)
	}
	return req, nil
}

func rerunConflict(member SuiteMember, why string) error {
	return &Error{Code: CodeConflict, Message: fmt.Sprintf("suite cannot be rerun: member %s %s", member.Label, why)}
}

// reportRuntime selects the runtime a run's report names: empty for the
// built-in runtime, else its name or image ID.
func (s *Service) reportRuntime(runID string) (string, bool) {
	s.mu.Lock()
	h, ok := s.runs[runID]
	s.mu.Unlock()
	if !ok {
		return "", false
	}
	rep := h.View(true).Report
	if rep == nil || rep.Runtime == nil {
		return "", false
	}
	if rep.Runtime.Kind == engine.RuntimeBuiltIn {
		return "", true
	}
	name := cmp.Or(rep.Runtime.Name, rep.Runtime.ImageID)
	return name, name != ""
}

// GetSuiteJUnit encodes a finished suite's test report as JUnit XML: one
// testsuite per member and one testcase per assertion. An unfinished
// suite is a conflict, as GetSuiteReport without waiting.
func (s *Service) GetSuiteJUnit(ctx context.Context, caller Caller, id string) (_ []byte, err error) {
	defer s.audit(&caller, "GetSuiteJUnit", &err)()
	rep, err := s.GetSuiteReport(ctx, caller, id, false)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := rep.WriteJUnit(&out); err != nil {
		return nil, classify(err, CodeInternal)
	}
	return out.Bytes(), nil
}
