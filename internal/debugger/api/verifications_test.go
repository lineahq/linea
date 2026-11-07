package api

import (
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/recommend"
)

// A recommendation is verified by position: the candidate runs and is
// compared with the run; an unknown position or a non-number is refused.
func TestVerifyRecommendationRoute(t *testing.T) {
	_, srv := newServer(t)
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "samples", "logs-cleanup"))
	if err != nil {
		t.Fatal(err)
	}
	run := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", Request{ConfigYAML: readText(t, filepath.Join(dir, "config.yaml")),
		ScenarioYAML: scenarioText(t, filepath.Join(dir, "scenario.yaml"))}), http.StatusAccepted)
	rep := decodeBody[engine.Report](t, get(t, srv.URL+"/api/v1/runs/"+run.ID+"/report?wait=true"), http.StatusOK)
	n := slices.IndexFunc(rep.Recommendations, func(r recommend.Recommendation) bool { return r.Rule == "filter.move_earlier" })
	if n < 0 {
		t.Fatalf("no filter.move_earlier in %+v", rep.Recommendations)
	}
	base := srv.URL + "/api/v1/runs/" + run.ID + "/recommendations/"
	v := decodeBody[RecommendationVerification](t, post(t, base+strconv.Itoa(n)+"/verify", struct{}{}), http.StatusOK)
	if v.Confidence != recommend.ConfidenceVerifiedOnInput || v.Candidate.ID == "" || v.Candidate.ID == run.ID || v.Comparison == nil || len(v.Overrides) != 1 {
		t.Fatalf("verification = %+v", v)
	}
	decodeBody[Error](t, post(t, base+"99/verify", struct{}{}), http.StatusNotFound)
	decodeBody[Error](t, post(t, base+"x/verify", struct{}{}), http.StatusBadRequest)
}
