package comparison

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// Recommendation statuses: what became of a recommendation from the before
// run in the after run.
const (
	// RecommendationResolved: only the before run has it.
	RecommendationResolved = "resolved"
	// RecommendationNew: only the after run has it.
	RecommendationNew = "new"
	// RecommendationPersists: both runs have it.
	RecommendationPersists = "persists"
)

// Recommendations pairs the two runs' recommendations. It is derived
// analysis and never changes the outcome.
type Recommendations struct {
	Resolved int                  `json:"resolved"`
	New      int                  `json:"new"`
	Persists int                  `json:"persists"`
	Items    []RecommendationDiff `json:"items"`
}

// Counts words the counts: "2 resolved, 1 new, 3 persist".
func (r *Recommendations) Counts() string {
	return fmt.Sprintf("%d resolved, %d new, %d persist", r.Resolved, r.New, r.Persists)
}

// RecommendationDiff is one recommendation in one or both runs, identified
// by rule, pipeline, components and subject (recommend.Recommendation).
type RecommendationDiff struct {
	Rule       string   `json:"rule"`
	Pipeline   string   `json:"pipeline,omitempty"`
	Components []string `json:"components"`
	Subject    string   `json:"subject,omitempty"`
	// Status is resolved, new or persists.
	Status string `json:"status"`
	// Before and After are the recommendation in each run; nil where the
	// run does not have it.
	Before *RecommendationSide `json:"before,omitempty"`
	After  *RecommendationSide `json:"after,omitempty"`
}

// RecommendationSide is a recommendation as one run's report has it.
type RecommendationSide struct {
	// Index is its position in that run's report.recommendations.
	Index      int                  `json:"index"`
	Kind       recommend.Kind       `json:"kind"`
	Confidence recommend.Confidence `json:"confidence"`
	Summary    string               `json:"summary"`
	Benefit    *recommend.Benefit   `json:"benefit,omitempty"`
}

// recommendationsOmitted: a stored report cut for size keeps no
// recommendations (platform persistence lists them as omitted).
func recommendationsOmitted(r *engineReport) bool {
	return r.Result != nil && r.Result.Truncation != nil && slices.Contains(r.Result.Truncation.Omitted, "recommendations")
}

// compareRecommendations pairs recommendations with the same identity in
// report order; several with one identity pair in order. Items list the
// before run's in its order, then those only the after run has.
func (c *Comparison) compareRecommendations(before, after *engineReport) {
	if before == nil || after == nil {
		return
	}
	if recommendationsOmitted(before) || recommendationsOmitted(after) {
		c.Notes = append(c.Notes, "recommendations are not compared: a run's stored report was cut for size and keeps none")
		return
	}
	out := &Recommendations{Items: []RecommendationDiff{}}
	unpaired := map[string][]int{}
	for i, r := range after.Recommendations {
		k := recommendationKey(r)
		unpaired[k] = append(unpaired[k], i)
	}
	paired := make([]bool, len(after.Recommendations))
	for i, r := range before.Recommendations {
		d := diffOf(r)
		d.Before = sideOf(i, r)
		k := recommendationKey(r)
		if idx := unpaired[k]; len(idx) > 0 {
			d.After = sideOf(idx[0], after.Recommendations[idx[0]])
			paired[idx[0]] = true
			unpaired[k] = idx[1:]
			d.Status = RecommendationPersists
			out.Persists++
		} else {
			d.Status = RecommendationResolved
			out.Resolved++
		}
		out.Items = append(out.Items, d)
	}
	for i, r := range after.Recommendations {
		if paired[i] {
			continue
		}
		d := diffOf(r)
		d.After = sideOf(i, r)
		d.Status = RecommendationNew
		out.New++
		out.Items = append(out.Items, d)
	}
	if len(out.Items) == 0 {
		return
	}
	c.Recommendations = out
	if !c.InputsIdentical && out.Resolved+out.New > 0 {
		c.Notes = append(c.Notes, "the runs' inputs are not proven identical: a recommendation drawn from what a run observed may come or go with the input, not the configuration")
	}
}

func recommendationKey(r recommend.Recommendation) string {
	return strings.Join([]string{r.Rule, r.Pipeline, strings.Join(r.Components, "\x01"), r.Subject}, "\x00")
}

func diffOf(r recommend.Recommendation) RecommendationDiff {
	return RecommendationDiff{Rule: r.Rule, Pipeline: r.Pipeline, Components: slices.Clone(r.Components), Subject: r.Subject}
}

func sideOf(i int, r recommend.Recommendation) *RecommendationSide {
	return &RecommendationSide{Index: i, Kind: r.Kind, Confidence: r.Confidence, Summary: r.Summary, Benefit: r.Benefit}
}
