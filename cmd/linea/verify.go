package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/client"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// verification is the outcome of verifying one recommendation, the same for
// a local run and a team server's.
type verification struct {
	index     int
	rule      string
	candidate string
	// confidence is verified_on_input or differs_on_input; empty when the
	// comparison was inconclusive, the candidate failed or the change
	// cannot be verified.
	confidence string
	reasons    []string
	// measured are the counts of the work the recommendation is about,
	// in the run and the candidate.
	measured []platform.Measure
	failed   string
	// refused is why the change cannot be verified; no candidate ran.
	refused string
}

// verifyAll verifies, one at a time, every recommendation that suggests a
// change, and raises its confidence in rep when the candidate settled it.
// A change the platform cannot verify (invalid_request) is reported, not an
// error; any other error stops.
func verifyAll(rep *platform.Report, verify func(n int) (verification, error)) ([]verification, error) {
	var out []verification
	for i, r := range rep.Recommendations {
		if r.Change == nil {
			continue
		}
		v, err := verify(i)
		if err != nil {
			return out, fmt.Errorf("verifying recommendation %d (%s): %w", i, r.Rule, err)
		}
		v.index, v.rule = i, r.Rule
		if v.confidence != "" {
			rep.Recommendations[i].Confidence = platform.Confidence(v.confidence)
		}
		out = append(out, v)
	}
	return out, nil
}

// verifyLocal verifies on the in-process service.
func verifyLocal(ctx context.Context, s *platform.Service, run string) func(int) (verification, error) {
	return func(n int) (verification, error) {
		v, err := s.VerifyRecommendation(ctx, cliCaller, platform.VerifyRecommendationRequest{Run: run, Recommendation: n})
		if platform.ErrorCode(err) == platform.CodeInvalidRequest {
			return verification{refused: errorMessage(err)}, nil
		}
		if err != nil {
			return verification{}, err
		}
		return verification{candidate: v.Candidate.ID, confidence: string(v.Confidence), reasons: reasonsOf(v.Comparison), measured: v.Measured, failed: v.Error}, nil
	}
}

// verifyConnected verifies on a team server.
func verifyConnected(ctx context.Context, c *client.Client, run string) func(int) (verification, error) {
	return func(n int) (verification, error) {
		v, err := c.VerifyRecommendation(ctx, run, n)
		var e *client.Error
		if errors.As(err, &e) && e.Code == string(platform.CodeInvalidRequest) {
			return verification{refused: e.Message}, nil
		}
		if err != nil {
			return verification{}, err
		}
		return verificationOf(v), nil
	}
}

func verificationOf(v *api.RecommendationVerification) verification {
	return verification{candidate: v.Candidate.ID, confidence: string(v.Confidence), reasons: reasonsOf(v.Comparison), measured: v.Measured, failed: v.Error}
}

func reasonsOf(c *platform.Comparison) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, r := range c.Reasons {
		out = append(out, r.Message)
	}
	return out
}

func errorMessage(err error) string {
	var e *platform.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// writeVerifications prints what each verification showed, after a blank
// line that sets it apart from the report or text before it.
func writeVerifications(w io.Writer, vs []verification) error {
	if len(vs) == 0 {
		return nil
	}
	t := newTextOut(w)
	t.wrote = true
	t.heading("verifications")
	words := make([]string, len(vs))
	for i, v := range vs {
		switch {
		case v.refused != "":
			words[i] = "unverifiable"
		case v.failed != "":
			words[i] = "failed"
		case v.confidence == string(platform.ConfidenceVerifiedOnInput):
			words[i] = "same"
		case v.confidence == string(platform.ConfidenceDiffersOnInput):
			words[i] = "different"
		default:
			words[i] = "inconclusive"
		}
	}
	width := statusWidth(words, func(s string) string { return s })
	for i, v := range vs {
		head := fmt.Sprintf("%s  recommendation %d %s: ", t.status(words[i], width), v.index, v.rule)
		switch words[i] {
		case "unverifiable":
			t.line(1, "%snot verifiable: %s", head, v.refused)
			continue
		case "failed":
			t.line(1, "%scandidate run %s failed: %s", head, v.candidate, v.failed)
			continue
		case "same":
			head += "same output on this input"
		case "different":
			head += "the output differs on this input"
		default:
			head += "inconclusive"
		}
		t.line(1, "%s (candidate run %s)", head, v.candidate)
		for _, r := range v.reasons {
			t.line(2, "reason: %s", r)
		}
		for _, m := range v.measured {
			t.line(2, "%s of %s: %d -> %d", m.Metric, m.Subject, m.Before, m.After)
		}
	}
	return t.err()
}
