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

// writeVerifications prints what each verification showed.
func writeVerifications(w io.Writer, vs []verification) error {
	if len(vs) == 0 {
		return nil
	}
	var errs []error
	printf := func(format string, a ...any) {
		_, err := fmt.Fprintf(w, format, a...)
		errs = append(errs, err)
	}
	printf("\nverifications:\n")
	for _, v := range vs {
		printf("  recommendation %d %s: ", v.index, v.rule)
		switch {
		case v.refused != "":
			printf("not verifiable: %s\n", v.refused)
			continue
		case v.failed != "":
			printf("candidate run %s failed: %s\n", v.candidate, v.failed)
			continue
		case v.confidence == string(platform.ConfidenceVerifiedOnInput):
			printf("same output on this input")
		case v.confidence == string(platform.ConfidenceDiffersOnInput):
			printf("the output differs on this input")
		default:
			printf("inconclusive")
		}
		printf(" (candidate run %s)\n", v.candidate)
		for _, r := range v.reasons {
			printf("      reason: %s\n", r)
		}
		for _, m := range v.measured {
			printf("      %s of %s: %d -> %d\n", m.Metric, m.Subject, m.Before, m.After)
		}
	}
	return errors.Join(errs...)
}
