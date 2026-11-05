package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// TestRunJournalSizeIsExact checks that the size saveRun keeps by delta is
// what encoding the whole journal measures, through admissions, updates and
// terminal publications.
func TestRunJournalSizeIsExact(t *testing.T) {
	ctx := context.Background()
	s := Open(NewData(), artifact.Limits{})
	if err := s.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 20; n++ {
		id := fmt.Sprintf("run-%016x", n)
		r, err := s.SaveRun(ctx, id, artifact.DefaultProject, 0, false, artifact.RunIndex{Result: "queued"}, []byte(`{"n":"<`+id+`>"}`), nil)
		if err != nil {
			t.Fatal(err)
		}
		if n%2 == 0 {
			if _, err := s.SaveRun(ctx, id, artifact.DefaultProject, r.Generation, true, artifact.RunIndex{Scenario: "sé", Result: "completed"}, []byte(`{}`), []byte("e")); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := s.d.runsSize, encodedSize(*s.d.runs); got != want {
			t.Fatalf("after run %d: size %d, encoded %d", n, got, want)
		}
	}
}
