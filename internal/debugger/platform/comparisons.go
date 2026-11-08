package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/comparison"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
)

// Run comparison under platform names; the definitions live in
// internal/debugger/comparison.
type (
	Comparison           = comparison.Comparison
	ComparisonOutcome    = comparison.Outcome
	ComparisonReason     = comparison.Reason
	ComparisonRun        = comparison.RunInfo
	ComparisonExporter   = comparison.Exporter
	ComparisonRecords    = comparison.Records
	ComparisonRecord     = comparison.Record
	ComparisonFate       = comparison.Fate
	ComparisonStep       = comparison.Step
	ComparisonChange     = comparison.FieldChange
	ComparisonRoute      = comparison.Route
	ComparisonError      = comparison.ErrorCount
	ComparisonAssertion  = comparison.Assertion
	ComparisonIgnoreRule = comparison.Ignored
	// ComparisonRecommendations pairs the runs' recommendations.
	ComparisonRecommendations    = comparison.Recommendations
	ComparisonRecommendation     = comparison.RecommendationDiff
	ComparisonRecommendationSide = comparison.RecommendationSide
)

// Comparison outcomes and the reason codes of an inconclusive one.
const (
	ComparisonSame         = comparison.Same
	ComparisonDifferent    = comparison.Different
	ComparisonInconclusive = comparison.Inconclusive

	ComparisonBeforeIncomplete = comparison.ReasonBeforeIncomplete
	ComparisonAfterIncomplete  = comparison.ReasonAfterIncomplete
	ComparisonBeforeTruncated  = comparison.ReasonBeforeTruncated
	ComparisonAfterTruncated   = comparison.ReasonAfterTruncated
	ComparisonBuildsDiffer     = comparison.ReasonBuildsDiffer
	ComparisonInputsDiffer     = comparison.ReasonInputsDiffer

	ComparisonRecommendationResolved = comparison.RecommendationResolved
	ComparisonRecommendationNew      = comparison.RecommendationNew
	ComparisonRecommendationPersists = comparison.RecommendationPersists
)

// CompareRequest names two runs to compare. Ignore lists OTTL paths
// (log.attributes["request.id"]) whose differences are counted per rule but
// do not make the runs different.
type CompareRequest struct {
	Before, After string
	Ignore        []string
}

// CompareRuns compares two terminal runs from their reports. It is a
// pure read: nothing executes and nothing is persisted, so the result can be
// derived again from the same runs. A run that is queued or running, or has
// no report, is a conflict; an unknown run is not_found; an ignore rule that
// does not parse is invalid_request. Whether the inputs are identical is
// decided from each run's captured bundle: by its inputs digest, or else by
// the self-contained scenario its inputs derive to (inputIdentity).
func (s *Service) CompareRuns(ctx context.Context, caller Caller, req CompareRequest) (_ *Comparison, err error) {
	defer s.audit(&caller, "CompareRuns", &err)()
	var sides [2]comparison.Side
	var parts [2]*engine.BundleParts
	for i, id := range []string{req.Before, req.After} {
		r, err := s.lookup(ctx, caller, authz.RunRead, id)
		if err != nil {
			return nil, err
		}
		v := r.View(true)
		if v.State != StateDone && v.State != StateCancelled && v.State != StateError {
			return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run %s is %s", id, v.State)}
		}
		if v.Report == nil || v.Report.Result == nil {
			return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run %s has no report", id)}
		}
		side := comparison.Side{Run: v.evidence()}
		raw, err := s.getRunBundle(ctx, id)
		switch {
		case errors.Is(err, ErrBundleUnavailable):
			// Without its bundle the run's inputs and build are unknown.
		case err != nil:
			return nil, err
		default:
			p, err := engine.ReadBundleParts(raw)
			if err != nil {
				e := newError(CodeStorageError, err)
				e.Message = "cannot read captured execution bundle"
				return nil, e
			}
			side.Mode, side.InputsDigest, side.Build = p.Mode, p.InputsDigest(), p.Build
			parts[i] = p
		}
		sides[i] = side
	}
	if parts[0] != nil && parts[1] != nil && sides[0].InputsDigest != sides[1].InputsDigest {
		sides[0].InputIdentity, sides[1].InputIdentity = inputIdentity(parts[0]), inputIdentity(parts[1])
	}
	out, err := comparison.Compare(comparison.Request{Before: sides[0], After: sides[1], Ignore: req.Ignore})
	if err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	return out, nil
}

// inputIdentity digests what a run injected, as the self-contained scenario
// its captured inputs derive to (scenario.InputIdentity), so a run and a test
// of it on those inputs inlined compare as identical inputs. It is empty, and
// proves nothing, when the inputs cannot be derived or the run read
// environment values, which are never recorded.
func inputIdentity(parts *engine.BundleParts) string {
	if len(parts.HostEnvironment) != 0 {
		return ""
	}
	raw, err := selfContainedScenario(parts, "", nil)
	if err != nil {
		return ""
	}
	id, err := scenario.InputIdentity(raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(id)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ComparisonRequest runs two configurations on one input: the input
// of a quick run or a scenario, as a RunRequest names it.
type ComparisonRequest struct {
	Before, After Source
	Mode          InputMode
	Quick         *QuickInput
	Scenario      *ScenarioInput
	// Runtime is the before run's runtime and AfterRuntime the after run's;
	// an empty AfterRuntime is Runtime, so a pair can compare one
	// configuration on two Collector releases.
	Runtime, AfterRuntime string
}

// ComparisonRuns are the two runs a configuration comparison admitted, in
// queue order.
type ComparisonRuns struct {
	Before, After Run
}

// SubmitComparison prepares the before run as SubmitRun does, then prepares
// the after run on a copy of the before run's captured inputs, capturing only
// its configuration, so no input is read twice; both configurations run
// unmodified. Capacity for both is reserved first and both are prepared
// before either is admitted: a full queue, or a failure to prepare either
// run, admits neither. It returns without waiting; CompareRuns compares the
// two once they are terminal.
func (s *Service) SubmitComparison(ctx context.Context, caller Caller, req ComparisonRequest) (_ *ComparisonRuns, err error) {
	defer s.audit(&caller, "SubmitComparison", &err)()
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	run, refs, err := s.resolveRun(ctx, caller, RunRequest{Mode: req.Mode, Config: req.Before, Quick: req.Quick, Scenario: req.Scenario, Runtime: req.Runtime})
	if err != nil {
		return nil, err
	}
	afterConfig := req.After
	afterRef, err := s.resolveSource(ctx, caller, &afterConfig, artifact.Configuration, "after config")
	if err != nil {
		return nil, err
	}
	before, err := run.engine()
	if err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	before.Sources = refs
	run.Config = afterConfig
	if req.AfterRuntime != "" {
		run.Runtime = req.AfterRuntime
	}
	after, err := run.engine()
	if err != nil {
		return nil, classify(err, CodeInvalidRequest)
	}
	var inputs SourceRefs
	if refs != nil {
		inputs = *refs
	}
	after.Sources = sourceRefs(afterRef, inputs.Fixture, inputs.Scenario)
	handles, err := s.submitPair(ctx, caller, req.Mode, before, after)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	return &ComparisonRuns{Before: handles[0].View(false), After: handles[1].View(false)}, nil
}

// submitPair reserves two queue slots, prepares both requests (the second on
// the first's captured inputs) and admits both together, or neither.
func (s *Service) submitPair(ctx context.Context, caller Caller, mode InputMode, first, second engine.Request) ([2]*Handle, error) {
	var handles [2]*Handle
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return handles, ErrClosed
	}
	if err := s.admitsLocked(caller, 2); err != nil {
		s.mu.Unlock()
		return handles, err
	}
	s.reserved += 2
	s.holdLocked(caller, 2)
	s.mu.Unlock()
	slots := &suiteAdmission{remaining: 2}
	entered, preparing := 0, 0
	defer func() {
		s.mu.Lock()
		s.reserved -= slots.remaining
		s.preparing -= preparing
		s.holdLocked(caller, -slots.remaining-preparing)
		s.mu.Unlock()
		for range entered {
			s.preparations.Done()
		}
	}()
	for _, req := range []engine.Request{first, second} {
		if err := s.enterPreparation(&caller, req, slots); err != nil {
			return handles, err
		}
		entered++
		preparing++
	}
	p1, err := s.prepareRun(ctx, caller.Project, first)
	if err != nil {
		return handles, err
	}
	if p1.req.Prepared() {
		if second, err = engine.ShareInputs(second, p1.req); err != nil {
			return handles, err
		}
	}
	p2, err := s.prepareRun(ctx, caller.Project, second)
	if err != nil {
		return handles, err
	}
	s.mu.Lock()
	s.preparing -= preparing
	s.holdLocked(caller, -preparing)
	preparing = 0
	switch {
	case s.closed:
		err = ErrClosed
	case ctx.Err() != nil:
		err = ctx.Err()
	}
	if err == nil {
		handles[0], err = s.admitLocked(caller, mode, p1, time.Time{}, nil)
	}
	if err == nil {
		handles[1], err = s.admitLocked(caller, mode, p2, time.Time{}, nil)
	}
	s.mu.Unlock()
	if err != nil {
		// Only a failed journal publication of the second run can leave the
		// first admitted; it is cancelled before it reports anything.
		if handles[0] != nil {
			_ = handles[0].cancelWithCause(err)
		}
		return [2]*Handle{}, err
	}
	return handles, nil
}
