package analysis

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden analyses of events without objects")

// stageDiff returns the diff of a stage: its first input's, or its
// reconciliation when it buffers.
func stageDiff(t *testing.T, stages []Stage, component string) *Diff {
	t.Helper()
	for _, s := range stages {
		if s.Component != component {
			continue
		}
		if s.Reconciliation != nil {
			return s.Reconciliation
		}
		if len(s.Inputs) > 0 && s.Inputs[0].Diff != nil {
			return s.Inputs[0].Diff
		}
	}
	t.Fatalf("no diff for %s", component)
	return nil
}

// outputConfidences returns, per lineage, the confidence of its stage
// output hop and how many exporters it reached or where it stopped.
func outputConfidences(lineage []RecordLineage) []string {
	var out []string
	for _, l := range lineage {
		s := "?"
		for _, h := range l.Hops {
			if h.Kind == "stage.output" {
				s = string(h.Confidence)
			}
		}
		if len(l.Deliveries) == 1 {
			s += " delivered"
			if l.Deliveries[0].Changed {
				s += " changed"
			}
		}
		for _, st := range l.Stops {
			s += " " + string(st.End)
		}
		out = append(out, s)
	}
	return out
}

// TestStagesFollowRecordsByObject: records a stage passes on in place are
// paired by object whatever it changed, unchanged ones count as exact, a
// removed record is the one whose object is gone, buffered records are
// reconciled by object, and a copying stage falls back to content.
func TestStagesFollowRecordsByObject(t *testing.T) {
	tests := []struct {
		fixture, stage string
		matching       Matching
		changes        int
		removed        []string
		lineage        []string
	}{
		{"hash", "attributes/hash", Matching{Object: 4}, 4, nil,
			slices.Repeat([]string{"object delivered changed"}, 4)},
		{"filter", "transform/drop", Matching{Object: 2, UnmatchedBefore: 1}, 2, []string{"resource[0].scope[0].log_record[1]"},
			[]string{"object delivered changed", "? dropped", "object delivered changed"}},
		{"batch", "batch", Matching{Object: 4}, 4, nil,
			slices.Repeat([]string{"object delivered changed"}, 4)},
		{"pass", "filter/none", Matching{Exact: 2}, 0, nil,
			[]string{"exact delivered", "exact delivered"}},
		{"copy", "groupbyattrs", Matching{Exact: 1, High: 1}, 1, nil,
			[]string{"high delivered changed", "exact delivered"}},
	}
	fixtures := objectFixtures(t)
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			events, snaps := fixtures[tt.fixture].events(false)
			stages, err := Analyze(events, snaps, true)
			if err != nil {
				t.Fatal(err)
			}
			d := stageDiff(t, stages, tt.stage)
			if d.Matching == nil || *d.Matching != tt.matching {
				t.Errorf("matching = %+v, want %+v", d.Matching, tt.matching)
			}
			if len(d.Unresolved) > 0 || len(d.Added) > 0 {
				t.Errorf("unresolved %v, added %v", d.Unresolved, d.Added)
			}
			var removed []string
			for _, r := range d.Removed {
				removed = append(removed, r.Ref)
			}
			if !slices.Equal(removed, tt.removed) {
				t.Errorf("removed = %v, want %v", removed, tt.removed)
			}
			if len(d.Changes) != tt.changes {
				t.Errorf("%d changes, want %d: %v", len(d.Changes), tt.changes, changeKeys(*d))
			}
			for _, c := range d.Changes {
				if tt.matching.Object > 0 && (c.Path != `attributes["enduser.id"]` || c.Confidence != ConfidenceObject) {
					t.Errorf("change %+v", c)
				}
			}
			lineage, err := Lineage(events, snaps, stages, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := outputConfidences(lineage); !slices.Equal(got, tt.lineage) {
				t.Errorf("lineage = %v, want %v", got, tt.lineage)
			}
		})
	}
}

// TestEventsWithoutObjectsAnalyzeAsBefore: events recorded before event
// schema 0.5 carry no objects; their stages and lineage are those the
// analysis gave before it read objects (the golden files were written by
// it, at f962b11), apart from the added matching count.
func TestEventsWithoutObjectsAnalyzeAsBefore(t *testing.T) {
	for name, r := range objectFixtures(t) {
		t.Run(name, func(t *testing.T) {
			events, snaps := r.events(true)
			stages, err := Analyze(events, snaps, true)
			if err != nil {
				t.Fatal(err)
			}
			lineage, err := Lineage(events, snaps, stages, nil, true)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(map[string]any{"stages": stages, "lineage": lineage}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "without-objects", name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("analysis differs from %s:\n%s", path, got)
			}
		})
	}
}
