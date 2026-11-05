package comparison

import (
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// A changed record names its lineage and path in each run and where the runs
// part, so it can be followed in both.
func TestChangedRecordsCanBeFollowedInBothRuns(t *testing.T) {
	cleanup := sample(t, "logs-cleanup", "config.yaml")
	base := execute(t, "logs-cleanup", cleanup, false)
	lineage := func(rep *engine.Report, id string) *analysis.RecordLineage {
		for i := range rep.Lineage {
			if rep.Lineage[i].ID == id {
				return &rep.Lineage[i]
			}
		}
		return nil
	}
	step := func(p []Step, component string) (Step, bool) {
		i := slices.IndexFunc(p, func(s Step) bool { return s.Component == component })
		if i < 0 {
			return Step{}, false
		}
		return p[i], true
	}

	t.Run("dropped record", func(t *testing.T) {
		after := execute(t, "logs-cleanup", strings.Replace(cleanup, "filter/health, ", "", 1), false)
		c := compare(t, run("a", base, "d"), run("b", after, "d"))
		i := slices.IndexFunc(c.Records.Items, func(r Record) bool { return r.FateChanged })
		if i < 0 {
			t.Fatalf("records = %+v", c.Records)
		}
		r := c.Records.Items[i]
		if lineage(base, r.Before.LineageID) == nil || lineage(after, r.After.LineageID) == nil {
			t.Fatalf("lineage IDs %q, %q are not in the reports", r.Before.LineageID, r.After.LineageID)
		}
		if r.DivergesAt != "filter/health (logs)" {
			t.Fatalf("diverges at %q", r.DivergesAt)
		}
		if s, ok := step(r.Before.Path, "filter/health"); !ok || s.End != analysis.EndDropped || s.Pipeline != "logs" {
			t.Fatalf("before path = %+v", r.Before.Path)
		}
		if _, ok := step(r.After.Path, "filter/health"); ok {
			t.Fatalf("after path = %+v", r.After.Path)
		}
		if s, ok := step(r.After.Path, "otlp/backend"); !ok || s.End != analysis.EndDelivered {
			t.Fatalf("after path = %+v", r.After.Path)
		}
		if r.Before.Path[0].Component != "otlp" || r.After.Path[0].Component != "otlp" {
			t.Fatalf("paths start at %+v and %+v", r.Before.Path[0], r.After.Path[0])
		}
	})

	t.Run("changed fields", func(t *testing.T) {
		config := strings.Replace(cleanup, "      - key: user.password\n        action: delete\n", "", 1)
		c := compare(t, run("a", base, "d"), run("b", execute(t, "logs-cleanup", config, false), "d"))
		r := c.Records.Items[0]
		if r.FateChanged || r.DivergesAt != "attributes/scrub (logs)" {
			t.Fatalf("record = %+v", r)
		}
		b, _ := step(r.Before.Path, "attributes/scrub")
		a, _ := step(r.After.Path, "attributes/scrub")
		if b.Changes != a.Changes+1 {
			t.Fatalf("attributes/scrub changes: before %+v, after %+v", b, a)
		}
	})
}

func TestDivergenceIsTheFirstDifferingStep(t *testing.T) {
	recv := Step{Component: "otlp"}
	filter := Step{Component: "filter/x", Pipeline: "logs"}
	dropped := Step{Component: "filter/x", Pipeline: "logs", End: analysis.EndDropped}
	export := Step{Component: "otlp/out", End: analysis.EndDelivered}
	changed := Step{Component: "transform/x", Pipeline: "logs", Changes: 2}
	unchanged := Step{Component: "transform/x", Pipeline: "logs"}
	for _, tc := range []struct {
		name          string
		before, after []Step
		want          string
	}{
		{"same paths", []Step{recv, filter, export}, []Step{recv, filter, export}, ""},
		{"dropped in before", []Step{recv, dropped}, []Step{recv, filter, export}, "filter/x (logs)"},
		{"missing in after", []Step{recv, filter, export}, []Step{recv, export}, "filter/x (logs)"},
		{"only in after", []Step{recv, export}, []Step{recv, filter, export}, "filter/x (logs)"},
		{"other changes", []Step{recv, changed, export}, []Step{recv, unchanged, export}, "transform/x (logs)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := divergence(tc.before, tc.after); got != tc.want {
				t.Fatalf("divergence = %q, want %q", got, tc.want)
			}
		})
	}
}
