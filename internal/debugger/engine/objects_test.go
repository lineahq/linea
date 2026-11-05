package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/instrument"
)

// TestExecuteFollowsRecordsByObject: records whose content the stages
// cannot tell apart are followed as the same records the stages passed on.
// Twenty logs sharing their timestamp, five bodies and every attribute but
// enduser.id, which attributes/scrub hashes, all reach the exporter changed;
// metric points renamed by transform/units are followed too.
func TestExecuteFollowsRecordsByObject(t *testing.T) {
	tests := []struct {
		name, sample, fixture string
		records               int
		object, path          string
	}{
		{"shared logs", "logs-cleanup", filepath.Join("..", "..", "..", "testdata", "lineage", "shared-timestamp-logs.json"), 20, "attributes/scrub", `attributes["enduser.id"]`},
		{"renamed points", "metrics-rename", filepath.Join("..", "..", "..", "samples", "metrics-rename", "input.json"), 0, "transform/units", "metric.name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{
				Config:  filepath.Join("..", "..", "..", "samples", tt.sample, "config.yaml"),
				Fixture: tt.fixture,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tt.records > 0 && len(rep.Lineage) != tt.records {
				t.Fatalf("%d lineages, want %d", len(rep.Lineage), tt.records)
			}
			byObject := 0
			for _, r := range rep.Lineage {
				for _, s := range r.Stops {
					if s.End == analysis.EndUnresolved {
						t.Errorf("%s unresolved at %s", r.ID, s.Component)
					}
				}
				for _, h := range r.Hops {
					if h.Kind != instrument.KindEgress || h.Component != tt.object || h.Confidence != analysis.ConfidenceObject {
						continue
					}
					byObject++
					if !slices.ContainsFunc(h.Changes, func(c analysis.Change) bool { return c.Path == tt.path }) {
						t.Errorf("%s changes at %s = %+v, want %s", r.ID, tt.object, h.Changes, tt.path)
					}
				}
				if tt.records > 0 && (len(r.Deliveries) == 0 || !r.Deliveries[0].Changed) {
					t.Errorf("%s deliveries = %+v", r.ID, r.Deliveries)
				}
			}
			if byObject == 0 {
				t.Errorf("no record followed by object through %s", tt.object)
			}
			if tt.records > 0 && byObject != tt.records {
				t.Errorf("%d of %d records followed by object through %s", byObject, tt.records, tt.object)
			}
		})
	}
}
