package platform

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

const retainedSuites = 20

func errUnknownSuite() error { return &Error{Code: CodeNotFound, Message: "unknown suite"} }

type suiteAdmission struct {
	remaining int
	handles   []*Handle
}

func cloneSuite(suite Suite) Suite {
	suite.Members = append([]SuiteMember(nil), suite.Members...)
	return suite
}

// SubmitSuite reserves capacity before preparing members in order. Accepted
// runs belong to the service once the immutable suite record is published.
func (s *Service) SubmitSuite(ctx context.Context, caller Caller, req SubmitSuiteRequest) (_ *Suite, err error) {
	defer s.audit(&caller, "SubmitSuite", &err)()
	suite, _, err := s.submitSuite(ctx, caller, authz.RunSubmit, req)
	if err != nil {
		return nil, err
	}
	return suite, err
}

func (s *Service) submitSuite(ctx context.Context, caller Caller, perm authz.Permission, req SubmitSuiteRequest) (*Suite, []*Handle, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, nil, err
	}
	if err := s.workIn(&caller, perm); err != nil {
		return nil, nil, err
	}
	n := len(req.Members)
	req.Members = append([]ScenarioInput(nil), req.Members...)
	configRef, err := s.resolveSource(ctx, caller, &req.Config, artifact.Configuration, "config")
	if err != nil {
		return nil, nil, err
	}
	memberRefs := make([]*ArtifactRef, n)
	for i := range req.Members {
		if memberRefs[i], err = s.resolveSource(ctx, caller, &req.Members[i].Scenario, artifact.Scenario, fmt.Sprintf("scenario %d", i+1)); err != nil {
			return nil, nil, err
		}
	}
	if n == 0 || n > MaxSuiteMembers || (req.Config.Path == "") == (req.Config.Text == "") {
		return nil, nil, classify(fmt.Errorf("%w: one config source and 1 to %d suite members are required", engine.ErrInvalidRequest, MaxSuiteMembers), CodeInvalidRequest)
	}
	name, err := s.projectRuntime(caller.Project, req.Runtime)
	if err != nil {
		return nil, nil, err
	}
	if s.selects(name) {
		// A missing runtime fails the suite, not each member.
		if _, err := s.bind(ctx, name); err != nil {
			return nil, nil, err
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, classify(ErrClosed, CodeUnavailable)
	}
	if err := s.admitsLocked(caller, n); err != nil {
		s.mu.Unlock()
		return nil, nil, classify(err, CodeResourceExhausted)
	}
	if s.repository != nil {
		if err := s.repository.CheckSuiteCapacity(ctx, s.suiteSubmitting); err != nil {
			s.mu.Unlock()
			return nil, nil, classify(err, CodeStorageError)
		}
	}
	s.reserved += n
	s.holdLocked(caller, n)
	s.suiteSubmitting++
	s.preparations.Add(1) // Close waits through the final suite publication.
	s.mu.Unlock()
	admission := &suiteAdmission{remaining: n}
	published := false
	slotReserved := true
	defer func() {
		s.mu.Lock()
		s.reserved -= admission.remaining
		s.holdLocked(caller, -admission.remaining)
		if slotReserved {
			s.suiteSubmitting--
		}
		if !published {
			for _, r := range admission.handles {
				delete(s.suiteRuns, r.ID())
			}
		}
		s.evictLocked()
		s.mu.Unlock()
		s.preparations.Done()
	}()
	config := cmp.Or(req.Config.Path, req.Config.Label, "inline")
	suite := Suite{ID: "suite-" + strings.TrimPrefix(instrument.NewRunID(), "run-"), Project: caller.Project, Caller: caller.Actor(), Created: time.Now().UTC(), Config: config, Members: make([]SuiteMember, 0, n)}
	caller.touch(suite.ID)
	handles := make([]*Handle, n)
	abort := func(err error) (*Suite, []*Handle, error) {
		for _, r := range admission.handles {
			_ = r.cancelWithCause(err)
		}
		// Private partial metadata lets the foreground wrapper retain admitted
		// evidence after cancellation. SubmitSuite still returns no suite.
		for i := len(suite.Members); i < n; i++ {
			label := memberLabel(req.Members[i], i)
			suite.Members = append(suite.Members, SuiteMember{Label: label, Error: suiteErrorMessage(&Run{State: StateCancelled})})
		}
		return &suite, handles, classify(err, CodeValidationFailed)
	}
	for i, member := range req.Members {
		if err := ctx.Err(); err != nil {
			return abort(err)
		}
		m := SuiteMember{Label: memberLabel(member, i)}
		er := engine.Request{Config: req.Config.Path, ConfigYAML: req.Config.Text, ConfigLabel: req.Config.Label,
			Scenario: member.Scenario.Path, ScenarioYAML: member.Scenario.Text, ScenarioDir: member.Dir, ScenarioLabel: member.Scenario.Label,
			Sources: sourceRefs(configRef, nil, memberRefs[i]), Runtime: runtimeSelection(req.Runtime)}
		r, err := s.submitReserved(ctx, caller, ModeScenario, er, time.Time{}, admission)
		if err != nil {
			m.Error = err.Error()
		} else {
			m.RunID, handles[i] = r.ID(), r
		}
		if ctx.Err() != nil && r == nil {
			m.Error = suiteErrorMessage(&Run{State: StateCancelled})
		}
		suite.Members = append(suite.Members, m)
		if ctx.Err() != nil {
			return abort(ctx.Err())
		}
		if errors.Is(err, ErrClosed) || errors.Is(err, ErrPersistence) {
			return abort(err)
		}
	}
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return abort(err)
	}
	if s.closed {
		s.mu.Unlock()
		return abort(ErrClosed)
	}
	if s.repository != nil {
		if err := s.persistSuite(ctx, suite); err != nil {
			s.mu.Unlock()
			// Publication failure leaves ordinary runs without a suite.
			if ctx.Err() != nil {
				return abort(ctx.Err())
			}
			return nil, nil, classify(err, CodeStorageError)
		}
	}
	s.suites[suite.ID] = suite
	s.suiteOrder = append(s.suiteOrder, suite.ID)
	published = true
	s.suiteSubmitting--
	slotReserved = false
	if s.repository == nil && len(s.suiteOrder) > retainedSuites {
		old := s.suites[s.suiteOrder[0]]
		delete(s.suites, old.ID)
		s.suiteOrder = s.suiteOrder[1:]
		for _, member := range old.Members {
			delete(s.suiteRuns, member.RunID)
		}
	}
	s.mu.Unlock()
	out := cloneSuite(suite)
	return &out, handles, nil
}

func memberStatus(member SuiteMember, run *Run) SuiteMemberStatus {
	m := SuiteMemberStatus{SuiteMember: member}
	if member.Error != "" {
		m.Outcome, m.Message = assertion.Error, member.Error
		return m
	}
	m.State = run.State
	if run.Report != nil {
		m.Name = run.Report.Scenario
	}
	switch run.State {
	case StateQueued, StateAssigned, StateRunning:
		return m
	case StateDone:
		if run.Report.Assertions == nil {
			m.Outcome, m.Message = assertion.Error, suiteErrorMessage(run)
		} else {
			m.Outcome = run.Report.Assertions.Outcome
		}
	default:
		m.Outcome, m.Message = assertion.Error, suiteErrorMessage(run)
	}
	return m
}

// Assertion outcomes keep their report and no setup message, including error.
// Only members without assertion evidence need an execution-error fallback.
func suiteErrorMessage(run *Run) string {
	if run.Error != "" {
		return run.Error
	}
	if run.Interrupted {
		return "run interrupted"
	}
	switch run.State {
	case StateCancelled:
		return "run cancelled"
	case StateLost:
		return "run lost"
	case StateDone:
		return "scenario declares no assertions"
	default:
		return "run failed"
	}
}

// GetSuite derives outcomes from recorded runs without executing or saving.
func (s *Service) GetSuite(ctx context.Context, caller Caller, id string) (_ *SuiteStatus, err error) {
	defer s.audit(&caller, "GetSuite", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	suite, exists := s.suites[id]
	if !exists {
		return nil, s.unknownSuiteLocked(id)
	}
	if err := s.authorizeIn(caller, authz.RunRead, suite.Project); err != nil {
		return nil, err
	}
	out := &SuiteStatus{Suite: cloneSuite(suite), Done: true, Members: make([]SuiteMemberStatus, 0, len(suite.Members))}
	worst := assertion.Pass
	for _, member := range suite.Members {
		var run *Run
		if member.RunID != "" {
			r, ok := s.runs[member.RunID]
			if !ok {
				// Members are pinned while their suite is retained.
				return nil, classify(fmt.Errorf("%w: suite member run is missing", ErrPersistence), CodeStorageError)
			}
			v := r.View(false)
			run = &v
		}
		m := memberStatus(member, run)
		out.Members = append(out.Members, m)
		if m.Outcome == "" {
			out.Done = false
		} else {
			worst = assertion.Worst(worst, m.Outcome)
		}
	}
	if out.Done {
		out.Outcome = worst
	}
	return out, nil
}

// ListSuites returns independent admission metadata, newest first.
func (s *Service) ListSuites(ctx context.Context, caller Caller) (_ []Suite, err error) {
	defer s.audit(&caller, "ListSuites", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	projects, err := s.listIn(caller, authz.RunRead)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, classify(ErrClosed, CodeUnavailable)
	}
	out := make([]Suite, 0, len(s.suiteOrder))
	for i := len(s.suiteOrder) - 1; i >= 0; i-- {
		if suite := s.suites[s.suiteOrder[i]]; inProjects(projects, suite.Project) {
			out = append(out, cloneSuite(suite))
		}
	}
	return out, nil
}

// RunSuite preserves the CLI report and submits consecutive bounded chunks.
// Handles held here remain valid even when another suite evicts this chunk.
func (s *Service) RunSuite(ctx context.Context, caller Caller, req SuiteRequest) (_ *SuiteReport, err error) {
	defer s.audit(&caller, "RunSuite", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if req.Config == "" || len(req.Scenarios) == 0 {
		return nil, classify(fmt.Errorf("%w: config and at least one scenario are required", engine.ErrInvalidRequest), CodeInvalidRequest)
	}
	rep := &SuiteReport{Schema: engine.TestSchemaVersion, Config: req.Config, Outcome: assertion.Pass, Scenarios: []engine.ScenarioResult{}}
	for offset := 0; offset < len(req.Scenarios); {
		end := min(offset+MaxSuiteMembers, len(req.Scenarios))
		members := make([]ScenarioInput, end-offset)
		for i, path := range req.Scenarios[offset:end] {
			members[i] = ScenarioInput{Scenario: Source{Path: path}}
		}
		suite, handles, err := s.submitSuite(ctx, caller, authz.RunSubmit, SubmitSuiteRequest{Config: Source{Path: req.Config}, Members: members, Runtime: req.Runtime})
		if err != nil {
			if ctx.Err() == nil {
				return nil, err
			}
			if suite == nil {
				suite = &Suite{}
				handles = make([]*Handle, len(members))
				for _, path := range req.Scenarios[offset:end] {
					suite.Members = append(suite.Members, SuiteMember{Label: path, Error: suiteErrorMessage(&Run{State: StateCancelled})})
				}
			}
		}
		for _, result := range suiteResults(ctx, suite, handles) {
			rep.Scenarios = append(rep.Scenarios, result)
			rep.Outcome = assertion.Worst(rep.Outcome, result.Outcome)
		}
		offset = end
		if ctx.Err() != nil {
			for _, path := range req.Scenarios[offset:] {
				m := memberStatus(SuiteMember{Label: path}, &Run{State: StateCancelled})
				rep.Scenarios = append(rep.Scenarios, engine.ScenarioResult{Path: path, Outcome: m.Outcome, Error: m.Message})
			}
			if offset < len(req.Scenarios) {
				rep.Outcome = assertion.Error
			}
			break
		}
	}
	return rep, nil
}

// suiteResults waits using the admitted handles, independent of history or
// suite eviction. Foreground cancellation waits for every member's cleanup.
func suiteResults(ctx context.Context, suite *Suite, handles []*Handle) []engine.ScenarioResult {
	for _, r := range handles {
		if r != nil && r.Wait(ctx) != nil {
			for _, unfinished := range handles {
				if unfinished != nil {
					_ = unfinished.cancelWithCause(context.Cause(ctx))
				}
			}
			for _, unfinished := range handles {
				if unfinished != nil {
					_ = unfinished.Wait(context.Background())
				}
			}
			break
		}
	}
	results := make([]engine.ScenarioResult, 0, len(suite.Members))
	for i, member := range suite.Members {
		results = append(results, scenarioResult(member, handles[i]))
	}
	return results
}

// scenarioResult is a member's line of the suite report; h is nil for a
// member whose preparation failed.
func scenarioResult(member SuiteMember, h *Handle) engine.ScenarioResult {
	var run *Run
	if h != nil {
		v := h.View(true)
		run = &v
	}
	m := memberStatus(member, run)
	result := engine.ScenarioResult{Path: m.Label, Name: m.Name, RunID: m.RunID, Outcome: m.Outcome, Error: m.Message}
	if run != nil {
		if run.Started != nil && run.Finished != nil {
			result.Duration = run.Finished.Sub(*run.Started).Seconds()
		}
		if run.Report != nil {
			result.Assertions = run.Report.Assertions
		}
	}
	return result
}

// GetSuiteReport derives the report RunSuite returns from a suite's recorded
// runs. With wait it blocks until every member finishes; ending the wait
// never cancels them. Without wait an unfinished suite is a conflict.
func (s *Service) GetSuiteReport(ctx context.Context, caller Caller, id string, wait bool) (_ *SuiteReport, err error) {
	defer s.audit(&caller, "GetSuiteReport", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
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
	if err := s.authorizeIn(caller, authz.RunRead, suite.Project); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	suite = cloneSuite(suite)
	handles := make([]*Handle, len(suite.Members))
	for i, member := range suite.Members {
		if member.RunID == "" {
			continue
		}
		r, ok := s.runs[member.RunID]
		if !ok {
			s.mu.Unlock()
			return nil, classify(fmt.Errorf("%w: suite member run is missing", ErrPersistence), CodeStorageError)
		}
		handles[i] = r
	}
	s.mu.Unlock()
	for _, h := range handles {
		if h == nil {
			continue
		}
		if wait {
			if err := h.Wait(ctx); err != nil {
				return nil, classify(err, CodeCancelled)
			}
		} else if state := h.View(false).State; !state.Terminal() {
			return nil, &Error{Code: CodeConflict, Message: "suite is not finished"}
		}
	}
	rep := &SuiteReport{Schema: engine.TestSchemaVersion, Config: suite.Config, Outcome: assertion.Pass, Scenarios: make([]engine.ScenarioResult, 0, len(suite.Members))}
	for i, member := range suite.Members {
		result := scenarioResult(member, handles[i])
		rep.Scenarios = append(rep.Scenarios, result)
		rep.Outcome = assertion.Worst(rep.Outcome, result.Outcome)
	}
	return rep, nil
}

// memberLabel names a suite member by its path or label, else by position.
func memberLabel(member ScenarioInput, i int) string {
	return cmp.Or(member.Scenario.Path, member.Scenario.Label, fmt.Sprintf("inline scenario %d", i+1))
}
