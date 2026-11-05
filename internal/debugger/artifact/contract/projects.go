package contract

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

func project(id string) artifact.ProjectRecord {
	return artifact.ProjectRecord{ID: id, Name: "Team " + id, CreatedBy: "user:admin", Created: epoch}
}

// projectSet: every write raises the revision and records a change, a
// stale revision conflicts, saving replaces the project of the same ID,
// audiences and availabilities are set and removed, invalid records are
// refused, and the set survives reopening in ID order.
func projectSet(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if set, err := r.ProjectSet(ctx); err != nil || set.Revision != 0 || len(set.Projects)+len(set.Audiences)+len(set.Availabilities) != 0 {
		t.Fatalf("set before a write = %+v, %v", set, err)
	}
	head, err := r.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payments := project("payments")
	set, err := r.SaveProject(ctx, 0, payments)
	if err != nil || set.Revision != 1 || len(set.Projects) != 1 || set.Projects[0] != payments {
		t.Fatalf("first project = %+v, %v", set, err)
	}
	if _, err := r.SaveProject(ctx, 0, project("checkout")); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("project at a stale revision = %v", err)
	}
	if set, err = r.SaveProject(ctx, 1, project("checkout")); err != nil || set.Revision != 2 {
		t.Fatalf("second project = %+v, %v", set, err)
	}
	if got := []string{set.Projects[0].ID, set.Projects[1].ID}; got[0] != "checkout" || got[1] != "payments" {
		t.Errorf("projects = %v; want ID order", got)
	}
	changed := payments
	changed.Name, changed.Archived, changed.DefaultRuntime = "Payments", true, "otelcol-0.162"
	changed.StorageLimit, changed.RetentionRuns, changed.RetentionAge = 1<<30, 50, 36*time.Hour+time.Nanosecond
	changed.UpdatedBy, changed.Updated = "user:lead", epoch.Add(time.Hour)
	if set, err = r.SaveProject(ctx, 2, changed); err != nil || set.Revision != 3 || len(set.Projects) != 2 {
		t.Fatalf("updated project = %+v, %v", set, err)
	}
	if got, _ := set.Project("payments"); got != changed {
		t.Errorf("updated project = %+v, want %+v", got, changed)
	}

	audience := artifact.AudienceRecord{ArtifactID: "art-1", Projects: []string{"checkout", "payments"}, SetBy: "user:admin", Set: epoch}
	if set, err = r.SetAudience(ctx, 3, audience); err != nil || set.Revision != 4 || len(set.Audiences) != 1 || !reflect.DeepEqual(set.Audiences[0], audience) {
		t.Fatalf("audience = %+v, %v", set, err)
	}
	all := artifact.AudienceRecord{ArtifactID: "art-1", Projects: []string{artifact.AllProjects}, SetBy: "user:admin", Set: epoch.Add(time.Minute)}
	if set, err = r.SetAudience(ctx, 4, all); err != nil || len(set.Audiences) != 1 || !reflect.DeepEqual(set.Audiences[0], all) {
		t.Fatalf("replaced audience = %+v, %v", set, err)
	}
	available := artifact.AvailabilityRecord{Runtime: "otelcol-0.162", Projects: []string{"payments"}, SetBy: "user:admin", Set: epoch}
	if set, err = r.SetAvailability(ctx, 5, available); err != nil || set.Revision != 6 || len(set.Availabilities) != 1 || !reflect.DeepEqual(set.Availabilities[0], available) {
		t.Fatalf("availability = %+v, %v", set, err)
	}
	if set, err = r.SetAudience(ctx, 6, artifact.AudienceRecord{ArtifactID: "art-1", Set: epoch}); err != nil || set.Revision != 7 || len(set.Audiences) != 0 {
		t.Fatalf("removed audience = %+v, %v", set, err)
	}
	if set, err = r.SetAvailability(ctx, 7, artifact.AvailabilityRecord{Runtime: "absent", Set: epoch}); err != nil || set.Revision != 8 || len(set.Availabilities) != 1 {
		t.Errorf("removing an absent availability = %+v, %v", set, err)
	}

	for name, bad := range map[string]artifact.ProjectRecord{
		"bad ID":           {ID: "Payments", Created: epoch},
		"long name":        {ID: "x", Name: string(make([]byte, artifact.MaxProjectName+1)), Created: epoch},
		"control chars":    {ID: "x", Name: "a\a", Created: epoch},
		"negative setting": {ID: "x", StorageLimit: -1, Created: epoch},
		"no time":          {ID: "x"},
	} {
		if _, err := r.SaveProject(ctx, 8, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v", name, err)
		}
	}
	for name, list := range map[string][]string{
		"all and more": {artifact.AllProjects, "payments"},
		"duplicate":    {"payments", "payments"},
		"bad ID":       {"Payments"},
	} {
		if _, err := r.SetAudience(ctx, 8, artifact.AudienceRecord{ArtifactID: "art-2", Projects: list, Set: epoch}); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("audience with %s = %v", name, err)
		}
		if _, err := r.SetAvailability(ctx, 8, artifact.AvailabilityRecord{Runtime: "r", Projects: list, Set: epoch}); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("availability with %s = %v", name, err)
		}
	}
	if _, err := r.SetAudience(ctx, 8, artifact.AudienceRecord{Projects: []string{"payments"}, Set: epoch}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("audience without an artifact = %v", err)
	}

	changes, _, err := r.Changes(ctx, head, artifact.MaxChanges)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range changes {
		if c.Kind == artifact.ChangeProjects {
			n++
		}
	}
	if n != 8 {
		t.Errorf("project changes = %d of %+v; want 8", n, changes)
	}
	again, err := open(t, b, artifact.Limits{}).ProjectSet(ctx)
	if err != nil || !reflect.DeepEqual(again, set) {
		t.Errorf("reopened set = %+v, %v; want %+v", again, err, set)
	}
}

// projectLimits: a new project, audience or availability beyond its bound
// is ErrLimit, and replacing an existing one at the bound succeeds.
func projectLimits(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	var revision uint64
	for i := range artifact.MaxAvailabilities {
		set, err := r.SetAvailability(ctx, revision, artifact.AvailabilityRecord{Runtime: fmt.Sprintf("runtime-%03d", i), Projects: []string{"p"}, Set: epoch})
		if err != nil {
			t.Fatalf("availability %d = %v", i, err)
		}
		revision = set.Revision
	}
	if _, err := r.SetAvailability(ctx, revision, artifact.AvailabilityRecord{Runtime: "one-more", Projects: []string{"p"}, Set: epoch}); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("availability beyond the bound = %v", err)
	}
	if _, err := r.SetAvailability(ctx, revision, artifact.AvailabilityRecord{Runtime: "runtime-000", Projects: []string{"q"}, Set: epoch}); err != nil {
		t.Errorf("replacing at the bound = %v", err)
	}
}

// projectRace: writers racing at the same revision produce exactly one
// change; the others conflict.
func projectRace(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	set, err := r.SaveProject(ctx, 0, project("base"))
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, errs[i] = r.SaveProject(ctx, set.Revision, project(fmt.Sprintf("team-%d", i)))
			case 1:
				_, errs[i] = r.SetAudience(ctx, set.Revision, artifact.AudienceRecord{ArtifactID: fmt.Sprintf("art-%d", i), Projects: []string{"base"}, Set: epoch})
			default:
				_, errs[i] = r.SetAvailability(ctx, set.Revision, artifact.AvailabilityRecord{Runtime: fmt.Sprintf("rt-%d", i), Projects: []string{"base"}, Set: epoch})
			}
		}()
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, artifact.ErrConflict):
			t.Errorf("writer %d = %v, want a conflict or success", i, err)
		}
	}
	after, err := r.ProjectSet(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if won != 1 || after.Revision != set.Revision+1 || len(after.Projects)+len(after.Audiences)+len(after.Availabilities) != 2 {
		t.Fatalf("%d writers won; set after %d = %+v", won, set.Revision, after)
	}
}

// projectRetention: a project's own run count and age replace the
// installation's for its runs, and its byte bound evicts its oldest runs;
// the other projects share the installation's policy.
func projectRetention(t *testing.T, b Backend) {
	r := openRuns(t, b)
	for n := 1; n <= 4; n++ {
		terminalRun(t, r, n, "team-a")
	}
	for n := 5; n <= 7; n++ {
		terminalRun(t, r, n, artifact.DefaultProject)
	}
	for n := 8; n <= 9; n++ {
		if _, err := r.SaveRun(ctx, runID(n), "team-b", 0, true, artifact.RunIndex{}, []byte(`{}`), []byte(fmt.Sprintf("ten bytes%d", n))); err != nil {
			t.Fatal(err)
		}
	}
	p := artifact.RetentionPolicy{Runs: 5, Projects: []artifact.ProjectRetention{{Project: "team-a", Runs: 2}, {Project: "team-b", Bytes: 15}}}
	now := time.Now()
	if got, want := evicted(t, r, p, now), []string{runID(1) + " count", runID(2) + " count", runID(8) + " storage"}; !slices.Equal(got, want) {
		t.Fatalf("evicted = %v, want %v", got, want)
	}
	if got := evicted(t, r, p, now); len(got) != 0 {
		t.Fatalf("evicted again = %v", got)
	}
	// A project's age bound applies to its runs only.
	aged := artifact.RetentionPolicy{Projects: []artifact.ProjectRetention{{Project: "team-a", Age: time.Hour}}}
	if got, want := evicted(t, r, aged, now.Add(2*time.Hour)), []string{runID(3) + " age", runID(4) + " age"}; !slices.Equal(got, want) {
		t.Fatalf("evicted by a project's age = %v, want %v", got, want)
	}
}
