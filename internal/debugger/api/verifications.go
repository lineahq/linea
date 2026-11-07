package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// RecommendationVerification is what running a recommendation's change on
// its run's inputs showed: the candidate run, its comparison with the run,
// and the confidence it earns.
type RecommendationVerification struct {
	Run            string `json:"run"`
	Recommendation int    `json:"recommendation"`
	Rule           string `json:"rule"`
	// Overrides are the provider overrides layered after the run's unchanged
	// configuration.
	Overrides []string `json:"overrides"`
	Candidate Run      `json:"candidate"`
	// Confidence is verified_on_input or differs_on_input; absent when the
	// comparison is inconclusive or the candidate failed.
	Confidence platform.Confidence  `json:"confidence,omitempty"`
	Comparison *platform.Comparison `json:"comparison,omitempty"`
	Error      string               `json:"error,omitempty"`
}

// verifyRecommendation runs one recommendation's change as a candidate on the
// run's inputs and waits for it; ending the request cancels the candidate.
func (s *Server) verifyRecommendation(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("the recommendation is its position in the report's recommendations"))
		return
	}
	v, err := s.runs.VerifyRecommendation(r.Context(), callerOf(r), platform.VerifyRecommendationRequest{Run: r.PathValue("id"), Recommendation: n})
	if err != nil {
		if r.Context().Err() == nil {
			writeFailure(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, RecommendationVerification{Run: v.Run, Recommendation: v.Recommendation, Rule: v.Rule, Overrides: v.Overrides,
		Candidate: runOf(v.Candidate), Confidence: v.Confidence, Comparison: v.Comparison, Error: v.Error})
}
