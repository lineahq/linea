package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// TestFilteringNamesTheConditionThatDroppedEachRecord runs samples/logs-cleanup
// on the real filter processor: the health check log is dropped by the one
// legacy log_record condition, and the evaluation reproduces the output.
func TestFilteringNamesTheConditionThatDroppedEachRecord(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Filtering) != 1 {
		t.Fatalf("filtering = %d entries, want 1", len(rep.Filtering))
	}
	f := rep.Filtering[0]
	if f.Processor != "filter/health" || f.Pipeline != "logs" || f.Signal != "logs" || f.Output == nil {
		t.Fatalf("filtering = %+v", f)
	}
	if f.Verification != analysis.FilterVerified || f.Records != 4 || f.Dropped != 1 {
		t.Fatalf("filtering = %s, %d records, %d dropped, notes %v", f.Verification, f.Records, f.Dropped, f.Notes)
	}
	if len(f.Conditions) != 1 || f.Conditions[0].Source != "logs.log_record[0]" || f.Conditions[0].Evaluated != 4 || f.Conditions[0].Matched != 1 {
		t.Fatalf("conditions = %+v", f.Conditions)
	}
	if len(f.Drops) != 1 || len(f.Drops[0].Removed) != 1 {
		t.Fatalf("drops = %+v", f.Drops)
	}
	d := f.Drops[0]
	if d.Ref != "resource[0].scope[0].log_record[2]" || d.Condition != 0 || d.Records != 1 {
		t.Fatalf("drop = %+v", d)
	}
	if r := d.Removed[0]; r.Name != "healthcheck ok" || r.Ref != d.Ref || r.Lineage == "" {
		t.Fatalf("removed record = %+v", r)
	}
	if !slices.ContainsFunc(rep.Lineage, func(l analysis.RecordLineage) bool { return l.ID == d.Removed[0].Lineage }) {
		t.Fatalf("lineage %q is not in the report", d.Removed[0].Lineage)
	}
}
