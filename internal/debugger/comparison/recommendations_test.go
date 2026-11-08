package comparison

import (
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/runresult"
)

// TestFixedConfigurationResolvesItsRecommendation runs broken-routing as
// shipped and with its where clause fixed: the statement that never matched
// is resolved, and the outcome is decided by the output alone.
func TestFixedConfigurationResolvesItsRecommendation(t *testing.T) {
	config := sample(t, "broken-routing", "config.yaml")
	fixed := strings.Replace(config, `log.severity_text == "error"`, `log.severity_text == "ERROR"`, 1)
	if fixed == config {
		t.Fatal("the sample's where clause moved")
	}
	before := execute(t, "broken-routing", config, true)
	after := execute(t, "broken-routing", fixed, true)
	c := compare(t, run("before", before, "d"), run("after", after, "d"))
	r := c.Recommendations
	if r == nil || r.Resolved == 0 {
		t.Fatalf("recommendations = %+v", r)
	}
	d := r.Items[0]
	if d.Status != RecommendationResolved || d.Rule != "ottl.statement_never_matched" || !strings.Contains(d.Subject, `"error"`) || d.Before == nil || d.After != nil {
		t.Fatalf("item = %+v", d)
	}
	if c.Outcome != Different {
		t.Fatalf("outcome = %s", c.Outcome)
	}
	if md := c.Markdown(); !strings.Contains(md, "## Recommendations") || !strings.Contains(md, "| resolved | `ottl.statement_never_matched`") {
		t.Fatalf("markdown:\n%s", md)
	}
}

// TestRecommendationsPairByIdentity: recommendations pair by rule,
// pipeline, components and subject, several of one identity in order; the
// summary and benefit may differ. A report cut for size compares none.
func TestRecommendationsPairByIdentity(t *testing.T) {
	rec := func(rule, subject string, count int) recommend.Recommendation {
		return recommend.Recommendation{Rule: rule, Pipeline: "logs", Components: []string{"processor/transform"}, Subject: subject,
			Kind: recommend.KindRedundancy, Confidence: recommend.ConfidenceObserved, Summary: rule + " " + subject,
			Benefit: &recommend.Benefit{Metric: recommend.MetricEvaluationsWithoutEffect, Count: count}}
	}
	before := &engine.Report{Recommendations: []recommend.Recommendation{
		rec("ottl.statement_never_matched", `set(a, 1)`, 4),
		rec("ottl.duplicate_statement", `set(b, 2)`, 3),
		rec("ottl.duplicate_statement", `set(b, 2)`, 3),
		rec("ottl.statement_no_change", `set(c, 3)`, 2),
	}}
	after := &engine.Report{Recommendations: []recommend.Recommendation{
		rec("ottl.statement_no_change", `set(c, 3)`, 1),
		rec("ottl.duplicate_statement", `set(b, 2)`, 3),
		rec("ottl.statement_never_matched", `set(d, 4)`, 4),
	}}
	c := &Comparison{InputsIdentical: true}
	c.compareRecommendations(before, after)
	r := c.Recommendations
	if r == nil || r.Resolved != 2 || r.New != 1 || r.Persists != 2 || len(r.Items) != 5 {
		t.Fatalf("recommendations = %+v", r)
	}
	var got []string
	for _, d := range r.Items {
		got = append(got, d.Status+" "+d.Subject)
	}
	want := []string{"resolved set(a, 1)", "persists set(b, 2)", "resolved set(b, 2)", "persists set(c, 3)", "new set(d, 4)"}
	if !slices.Equal(got, want) {
		t.Fatalf("items = %q", got)
	}
	if p := r.Items[3]; p.Before.Index != 3 || p.After.Index != 0 || p.Before.Benefit.Count != 2 || p.After.Benefit.Count != 1 {
		t.Fatalf("persists = %+v %+v", p.Before, p.After)
	}
	if n := r.Items[4]; n.Before != nil || n.After.Index != 2 {
		t.Fatalf("new = %+v", n)
	}
	if len(c.Notes) != 0 {
		t.Fatalf("notes = %q", c.Notes)
	}

	c = &Comparison{}
	c.compareRecommendations(before, after)
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "not proven identical") {
		t.Fatalf("notes = %q", c.Notes)
	}

	cut := &engine.Report{Result: &runresult.Result{Truncation: &runresult.Truncation{Omitted: []string{"recommendations"}}}}
	c = &Comparison{InputsIdentical: true}
	c.compareRecommendations(before, cut)
	if c.Recommendations != nil || len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "cut for size") {
		t.Fatalf("cut: %+v notes %q", c.Recommendations, c.Notes)
	}

	c = &Comparison{InputsIdentical: true}
	c.compareRecommendations(&engine.Report{}, &engine.Report{})
	if c.Recommendations != nil || len(c.Notes) != 0 {
		t.Fatalf("none: %+v notes %q", c.Recommendations, c.Notes)
	}
}
