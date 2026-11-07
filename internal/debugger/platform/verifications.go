package platform

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/comparison"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/recommend"
)

// VerifyRecommendationRequest names one recommendation of a finished run by
// its position in the report's recommendations.
type VerifyRecommendationRequest struct {
	Run            string
	Recommendation int
}

// RecommendationVerification is what running a recommendation's change on
// its run's inputs showed.
type RecommendationVerification struct {
	// Run and Recommendation name the verified recommendation; Rule is its
	// rule ID.
	Run            string `json:"run"`
	Recommendation int    `json:"recommendation"`
	Rule           string `json:"rule"`
	// Overrides are the provider overrides the candidate layered after the
	// run's unchanged configuration.
	Overrides []string `json:"overrides"`
	// Candidate is the run of the changed configuration on the same inputs:
	// an ordinary run.
	Candidate Run `json:"candidate"`
	// Confidence is verified_on_input when the candidate's output is the
	// same as the run's, differs_on_input when it differs, and empty when
	// the comparison is inconclusive or the candidate failed.
	Confidence recommend.Confidence `json:"confidence,omitempty"`
	// Comparison compares the run (before) with the candidate (after);
	// absent when the candidate failed.
	Comparison *Comparison `json:"comparison,omitempty"`
	// Error is why the candidate run failed.
	Error string `json:"error,omitempty"`
}

// VerifyRecommendation runs a recommendation's change as a candidate: the
// run's captured bundle with the change layered as provider overrides on its
// configuration snapshot, on the same inputs and runtime, never reading the
// host. The user's configuration is not changed. It waits for the candidate
// and compares it with the run (CompareRuns); when ctx ends first, it cancels
// the candidate. Verification is on demand: each call is one more run.
func (s *Service) VerifyRecommendation(ctx context.Context, caller Caller, req VerifyRecommendationRequest) (_ *RecommendationVerification, err error) {
	defer s.audit(&caller, "VerifyRecommendation", &err)()
	r, err := s.lookup(ctx, caller, authz.RunRead, req.Run)
	if err != nil {
		return nil, err
	}
	v := r.View(false)
	if v.State != StateDone || v.Report == nil {
		return nil, &Error{Code: CodeConflict, Message: fmt.Sprintf("run %s is %s; only a finished run's recommendations can be verified", req.Run, v.State)}
	}
	if req.Recommendation < 0 || req.Recommendation >= len(v.Report.Recommendations) {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("run %s has no recommendation %d", req.Run, req.Recommendation)}
	}
	rec := v.Report.Recommendations[req.Recommendation]
	if err := engine.Verifiable(rec); err != nil {
		return nil, newError(CodeInvalidRequest, err)
	}
	if err := s.begin(ctx, caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	if err := s.workIn(&caller, authz.RunSubmit); err != nil {
		return nil, err
	}
	raw, err := s.getRunBundle(ctx, req.Run)
	if err != nil {
		return nil, err
	}
	parts, err := engine.ReadBundleParts(raw)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	original, err := engine.BundleRuntime(raw)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	overrides, err := engine.CandidateOverrides(rec, raw)
	if errors.Is(err, engine.ErrNotVerifiable) {
		return nil, newError(CodeInvalidRequest, err)
	} else if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	candidate, err := engine.CandidateRequest(ctx, raw, overrides)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	// The candidate binds its runtime by name as any run does; it must
	// be the build the run ran on.
	name := BuiltInRuntime
	if original != nil && original.Name != "" {
		name = original.Name
	}
	candidate.Runtime = runtimeSelection(name)
	h, err := s.submit(ctx, caller, InputMode(parts.Mode), candidate, time.Time{})
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	out := &RecommendationVerification{Run: req.Run, Recommendation: req.Recommendation, Rule: rec.Rule, Overrides: overrides}
	if err := s.sameRuntime(ctx, h.id, original); err != nil {
		_, _ = s.CancelRun(context.WithoutCancel(ctx), caller, h.id)
		return nil, err
	}
	_, err = s.GetRunReport(ctx, caller, h.id, true)
	if err != nil && ctx.Err() != nil {
		_, _ = s.CancelRun(context.WithoutCancel(ctx), caller, h.id)
		return nil, ctx.Err()
	}
	out.Candidate = h.View(false)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeRunFailed {
			return nil, err
		}
		out.Error = e.Message
		return out, nil
	}
	cmp, err := s.CompareRuns(ctx, caller, CompareRequest{Before: req.Run, After: h.id})
	if err != nil {
		return nil, err
	}
	out.Comparison = cmp
	switch cmp.Outcome {
	case comparison.Same:
		out.Confidence = recommend.ConfidenceVerifiedOnInput
	case comparison.Different:
		out.Confidence = recommend.ConfidenceDiffersOnInput
	}
	return out, nil
}

// sameRuntime refuses a candidate bound to another runtime build than the
// run it verifies.
func (s *Service) sameRuntime(ctx context.Context, id string, original *engine.RuntimeTarget) error {
	raw, err := s.getRunBundle(ctx, id)
	if err != nil {
		return err
	}
	bound, err := engine.BundleRuntime(raw)
	if err != nil {
		return classify(err, CodeStorageError)
	}
	switch {
	case original == nil && bound == nil:
		return nil
	case original != nil && bound != nil && original.ID == bound.ID:
		return nil
	}
	describe := func(t *engine.RuntimeTarget) string {
		if t == nil {
			return "the built-in runtime"
		}
		return fmt.Sprintf("runtime %s (%s)", t.Name, t.ID)
	}
	return &Error{Code: CodeConflict, Message: fmt.Sprintf("the run ran on %s, but its candidate would run on %s; the comparison would not isolate the change",
		describe(original), describe(bound))}
}
