// Package contract is the behavior every artifact.Repository shares. An
// implementation's tests call Run with a way to open fresh storage.
package contract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Backend opens one storage location. Each call opens the same data again,
// so a test can close a repository and reopen what it wrote.
type Backend func(limits artifact.Limits) (artifact.Repository, error)

// Run runs the contract. fresh returns a Backend on new, empty storage; it
// is called once per case.
func Run(t *testing.T, fresh func(t *testing.T) Backend) {
	cases := []struct {
		name string
		run  func(*testing.T, Backend)
	}{
		{"artifacts keep exact revisions", artifactRevisions},
		{"stale updates conflict", staleUpdates},
		{"projects are required and fixed", artifactProjects},
		{"lists filter by project and page", listsByProject},
		{"runs keep bundles and evidence", runLifecycle},
		{"runs page newest first by filter", runPages},
		{"unindexed runs are indexed once", runIndexing},
		{"runs list what a server opens on", runsInFlight},
		{"evaluations belong to their run", evaluations},
		{"suites list runs of their project", suites},
		{"deleted runs never return", deletion},
		{"retention evicts the oldest unprotected runs", retention},
		{"retention evicts by storage", retentionStorage},
		{"prune removes the selected unprotected runs", prune},
		{"queue claims by runtime, priority and admission", queueOrder},
		{"claims never share a run", queueExclusive},
		{"cancellation dequeues or flags", queueCancel},
		{"expired leases requeue once per lease", queueLeases},
		{"released runs return to the queue without spending an attempt", queueRelease},
		{"runtime catalog keeps names and confirmations", runtimeCatalog},
		{"runtime catalog keeps the binary of a server build", runtimeBinaries},
		{"runtime catalog records a suspect binary until the entry is saved again", suspectBinaries},
		{"runtime catalog keeps the default runtime and refuses to remove its entry", defaultRuntime},
		{"runs keep the binary they were admitted with", runBinaries},
		{"pool claims take runs pinned to their platform", binaryClaims},
		{"a pinned binary stays on the assignment through the queue", binaryLeases},
		{"a run keeps its binary after the catalog entry changes", binaryOutlivesEntry},
		{"worker records count until they expire", workerPresence},
		{"change feed records writes for other repositories", changeFeed},
		{"live events keep the latest attempt", liveEvents},
		{"interactive jobs are claimed once and answered", interactiveJobs},
		{"builds queue with their inputs within limits", buildAdmission},
		{"builders hold builds under leases and reuse done ones", buildLeases},
		{"expired build leases requeue until the attempts are used", buildExpiry},
		{"build cancellation dequeues or flags", buildCancel},
		{"build logs keep the last lines", buildLogs},
		{"builds page newest first and trim", buildPages},
		{"captures keep segments", captures},
		{"limits refuse without publishing", limits},
		{"audit appends and pages newest first", auditLog},
		{"audit retention bounds count and age", auditRetention},
		{"tokens are found by hash and revoked", tokens},
		{"role bindings change at the expected revision", roleBindings},
		{"concurrent role binding writes at one revision serialize", roleBindingRace},
		{"projects change at the expected revision", projectSet},
		{"project sets refuse beyond their bounds", projectLimits},
		{"concurrent project writes at one revision serialize", projectRace},
		{"project retention replaces the installation's for its runs", projectRetention},
		{"sessions are found by hash and ended", sessions},
		{"sign-ins in progress are taken once", logins},
		{"closed repositories refuse", closed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, fresh(t)) })
	}
}

var ctx = context.Background()

func open(t *testing.T, b Backend, limits artifact.Limits) artifact.Repository {
	t.Helper()
	r, err := b(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func openRuns(t *testing.T, b Backend) artifact.Repository {
	t.Helper()
	r := open(t, b, artifact.Limits{})
	if err := r.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

func create(t *testing.T, r artifact.Repository, project, content string) *artifact.Saved {
	t.Helper()
	saved, err := r.Save(ctx, artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: project, Kind: artifact.Scenario,
		Label: content, Actor: "user:test", Content: []byte(content)})
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func update(r artifact.Repository, m artifact.Metadata, content string) (*artifact.Saved, error) {
	return r.Save(ctx, artifact.SaveRequest{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, ExpectedGeneration: m.Generation,
		ExpectedHead: m.Head, Kind: m.Kind, Label: m.Label, Actor: "user:test", Content: []byte(content)})
}

func runID(n int) string { return fmt.Sprintf("run-%016x", n) }

func admission(id, project string, bundle []byte) artifact.Admission {
	return artifact.Admission{ID: id, Project: project, Metadata: []byte(`{}`), Bundle: bundle, Runtime: "builtin"}
}

func artifactRevisions(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	original := "exact\r\nbytes \xff"
	first := create(t, r, artifact.DefaultProject, original)
	second, err := update(r, first.Artifact, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision.Parent != first.Revision.Ref.RevisionID || second.Artifact.Head != second.Revision.Ref.RevisionID {
		t.Fatalf("revision chain: %+v", second)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	got, err := r.Revision(ctx, first.Revision.Ref)
	if err != nil || string(got.Content) != original || got.Revision.Actor != "user:test" {
		t.Fatalf("first revision = %+v, %v", got, err)
	}
	m, err := r.Get(ctx, first.Artifact.ID)
	if err != nil || m.Head != second.Revision.Ref.RevisionID || m.Project != artifact.DefaultProject {
		t.Fatalf("head = %+v, %v", m, err)
	}
	page, err := r.Revisions(ctx, artifact.RevisionsRequest{ArtifactID: m.ID})
	if err != nil || len(page.Items) != 2 || page.Items[0].Ref != second.Revision.Ref {
		t.Fatalf("revisions = %+v, %v", page, err)
	}
	if _, err := r.Get(ctx, "00000000000000000000000000000000"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown artifact: %v", err)
	}
}

func staleUpdates(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	first := create(t, r, artifact.DefaultProject, "a")
	if _, err := update(r, first.Artifact, "b"); err != nil {
		t.Fatal(err)
	}
	_, err := update(r, first.Artifact, "c")
	var conflict *artifact.Conflict
	if !errors.As(err, &conflict) || conflict.Current == nil || conflict.Current.Generation != first.Artifact.Generation+1 {
		t.Fatalf("stale update = %v", err)
	}
	m, _ := r.Get(ctx, first.Artifact.ID)
	m.Kind = artifact.Fixture
	if _, err := update(r, m, "d"); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("kind change = %v", err)
	}
}

func artifactProjects(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	for _, project := range []string{"", "Upper", "-lead", "a/b"} {
		_, err := r.Save(ctx, artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: project, Kind: artifact.Scenario, Actor: "user:test", Content: []byte("x")})
		if !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("project %q: %v", project, err)
		}
	}
	saved := create(t, r, "team-a", "x")
	req := artifact.SaveRequest{WorkspaceID: r.Info().ID, ArtifactID: saved.Artifact.ID, ExpectedGeneration: saved.Artifact.Generation,
		ExpectedHead: saved.Artifact.Head, Kind: artifact.Scenario, Project: "team-b", Actor: "user:test", Content: []byte("y")}
	if _, err := r.Save(ctx, req); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("project change = %v", err)
	}
	req.Project = ""
	if got, err := r.Save(ctx, req); err != nil || got.Artifact.Project != "team-a" {
		t.Errorf("update without project = %+v, %v", got, err)
	}
}

func listsByProject(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	for i := range 5 {
		project := artifact.DefaultProject
		if i%2 == 0 {
			project = "team-a"
		}
		create(t, r, project, fmt.Sprint(i))
	}
	all := func(req artifact.ListRequest) []artifact.Metadata {
		t.Helper()
		var out []artifact.Metadata
		for {
			page, err := r.List(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, page.Items...)
			if page.NextCursor == "" {
				return out
			}
			req.Cursor = page.NextCursor
		}
	}
	if n := len(all(artifact.ListRequest{Limit: 2})); n != 5 {
		t.Errorf("all projects = %d", n)
	}
	team := all(artifact.ListRequest{Projects: []string{"team-a"}, Limit: 1})
	if len(team) != 3 {
		t.Errorf("team-a = %d", len(team))
	}
	for _, m := range team {
		if m.Project != "team-a" {
			t.Errorf("filtered list has %s", m.Project)
		}
	}
	if n := len(all(artifact.ListRequest{Projects: []string{}})); n != 0 {
		t.Errorf("no projects = %d", n)
	}
	page, err := r.List(ctx, artifact.ListRequest{Projects: []string{"team-a"}, Limit: 1})
	if err != nil || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	if _, err := r.List(ctx, artifact.ListRequest{Limit: 1, Cursor: page.NextCursor}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("cursor reused with another filter: %v", err)
	}
}

func runLifecycle(t *testing.T, b Backend) {
	r := openRuns(t, b)
	bundle, evidence := []byte("private inputs \x00"), []byte("evidence")
	admitted, err := r.AdmitRun(ctx, admission(runID(1), "team-a", bundle))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdmitRun(ctx, admission(runID(1), "team-a", bundle)); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("second admission = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(1), "team-b", admitted.Generation, true, artifact.RunIndex{}, []byte(`{}`), evidence); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("project change = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(1), "team-a", admitted.Generation+1, true, artifact.RunIndex{}, []byte(`{}`), evidence); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("stale generation = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(1), "team-a", admitted.Generation, true, artifact.RunIndex{}, []byte(`{"done":true}`), evidence); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r = openRuns(t, b)
	runs, err := r.Runs(ctx)
	if err != nil || len(runs) != 1 || runs[0].Project != "team-a" || !runs[0].Terminal || string(runs[0].Metadata) != `{"done":true}` {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if got, err := r.RunBundle(ctx, runID(1)); err != nil || !bytes.Equal(got, bundle) {
		t.Errorf("bundle = %q, %v", got, err)
	}
	if got, err := r.RunEvidence(ctx, runID(1)); err != nil || !bytes.Equal(got, evidence) {
		t.Errorf("evidence = %q, %v", got, err)
	}
	if _, err := r.RunEvidence(ctx, runID(2)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown run = %v", err)
	}
}

// pagedRuns is how many runs runPages lists: the history a team server
// keeps by default.
const pagedRuns = 10000

// pagedIndex is run n's index in runPages: three scenarios (one empty), two
// runtimes and three results, so each filter combination is a distinct
// stride of the ordinals.
func pagedIndex(n int) artifact.RunIndex {
	return artifact.RunIndex{Scenario: []string{"checkout", "payments", ""}[n%3], Runtime: []string{"built-in", "edge"}[n%2],
		Result: []string{"completed", "failed", "partial"}[n%5%3]}
}

// pageAll follows q's cursors to the end and returns the listed run numbers.
func pageAll(t *testing.T, r artifact.Repository, q artifact.RunQuery) []int {
	t.Helper()
	var got []int
	for pages := 0; ; pages++ {
		if pages > pagedRuns {
			t.Fatal("paging does not end")
		}
		page, err := r.ListRuns(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		limit := q.Limit
		if limit == 0 {
			limit = artifact.DefaultRunPage
		}
		if len(page.Runs) > limit || page.NextCursor != "" && len(page.Runs) != limit {
			t.Fatalf("page of %d runs with limit %d, next %q", len(page.Runs), limit, page.NextCursor)
		}
		for _, run := range page.Runs {
			var n int
			if _, err := fmt.Sscanf(run.ID, "run-%x", &n); err != nil {
				t.Fatal(err)
			}
			got = append(got, n)
		}
		if page.NextCursor == "" {
			return got
		}
		q.Cursor = page.NextCursor
	}
}

func runPages(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{Runs: pagedRuns, CatalogBytes: 64 << 20})
	if err := r.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	project := func(n int) string { return []string{"team-a", "team-b"}[n%7%2] }
	for n := 1; n <= pagedRuns; n++ {
		if _, err := r.SaveRun(ctx, runID(n), project(n), 0, true, pagedIndex(n), []byte(`{}`), nil); err != nil {
			t.Fatal(n, err)
		}
	}
	want := func(match func(int) bool) []int {
		ns := []int{}
		for n := pagedRuns; n >= 1; n-- {
			if match(n) {
				ns = append(ns, n)
			}
		}
		return ns
	}
	cases := []struct {
		name  string
		q     artifact.RunQuery
		match func(int) bool
	}{
		{"all", artifact.RunQuery{Limit: artifact.MaxRunPage}, func(int) bool { return true }},
		{"default page", artifact.RunQuery{Projects: []string{"team-a"}}, func(n int) bool { return project(n) == "team-a" }},
		{"scenario", artifact.RunQuery{Scenario: "checkout", Limit: 97}, func(n int) bool { return n%3 == 0 }},
		{"runtime and result", artifact.RunQuery{Projects: []string{"team-a", "team-b"}, Runtime: "edge", Result: "failed", Limit: 64},
			func(n int) bool { return n%2 == 1 && n%5%3 == 1 }},
		{"every filter", artifact.RunQuery{Projects: []string{"team-b"}, Scenario: "payments", Runtime: "built-in", Result: "partial", Limit: 7},
			func(n int) bool { return project(n) == "team-b" && n%3 == 1 && n%2 == 0 && n%5%3 == 2 }},
		{"no match", artifact.RunQuery{Scenario: "absent"}, func(int) bool { return false }},
		{"no project", artifact.RunQuery{Projects: []string{}}, func(int) bool { return false }},
	}
	for _, c := range cases {
		if got, want := pageAll(t, r, c.q), want(c.match); !slices.Equal(got, want) {
			t.Errorf("%s: listed %d runs, want %d", c.name, len(got), len(want))
		}
	}

	first, err := r.ListRuns(ctx, artifact.RunQuery{Scenario: "checkout", Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	for _, q := range []artifact.RunQuery{
		{Scenario: "payments", Limit: 2, Cursor: first.NextCursor},
		{Projects: []string{"team-a"}, Scenario: "checkout", Limit: 2, Cursor: first.NextCursor},
		{Scenario: "checkout", Limit: 2, Cursor: "not a cursor"},
		{Limit: artifact.MaxRunPage + 1},
		{Limit: -1},
	} {
		if _, err := r.ListRuns(ctx, q); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("ListRuns(%+v) = %v", q, err)
		}
	}
	// A cursor carries the query, not the page size.
	if next, err := r.ListRuns(ctx, artifact.RunQuery{Scenario: "checkout", Limit: 3, Cursor: first.NextCursor}); err != nil || len(next.Runs) != 3 || next.Runs[0].ID != runID(pagedRuns-pagedRuns%3-6) {
		t.Errorf("next page = %+v, %v", next, err)
	}
	// The index persists: another repository on the same storage lists it.
	n := pagedRuns - pagedRuns%3
	r2 := openRuns(t, b)
	page, err := r2.ListRuns(ctx, artifact.RunQuery{Scenario: "checkout", Limit: 1})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].ID != runID(n) || page.Runs[0].Index == nil || *page.Runs[0].Index != pagedIndex(n) {
		t.Errorf("reopened page = %+v, %v", page, err)
	}
}

func runIndexing(t *testing.T, b Backend) {
	r := openRuns(t, b)
	queued, err := r.SaveRun(ctx, runID(1), "team-a", 0, false, artifact.RunIndex{Result: "queued"}, []byte(`{}`), nil)
	if err != nil || queued.Index == nil || queued.Index.Result != "queued" {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	if _, err := r.SaveRun(ctx, runID(1), "team-a", queued.Generation, true, artifact.RunIndex{Scenario: "checkout", Runtime: "edge", Result: "completed"}, []byte(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	// IndexRun only completes runs stored without an index.
	if err := r.IndexRun(ctx, runID(1), artifact.RunIndex{Result: "failed"}); err != nil {
		t.Fatal(err)
	}
	if run, err := r.Run(ctx, runID(1)); err != nil || run.Index == nil || run.Index.Result != "completed" || run.Index.Scenario != "checkout" {
		t.Errorf("indexed run = %+v, %v", run, err)
	}
	if page, err := r.ListRuns(ctx, artifact.RunQuery{Result: "completed", Runtime: "edge"}); err != nil || len(page.Runs) != 1 {
		t.Errorf("listed = %+v, %v", page, err)
	}
	if err := r.IndexRun(ctx, runID(2), artifact.RunIndex{}); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown run = %v", err)
	}
	if err := r.IndexRun(ctx, runID(1), artifact.RunIndex{Result: strings.Repeat("x", 65)}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("oversized index = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(3), "team-a", 0, false, artifact.RunIndex{Scenario: strings.Repeat("x", artifact.MaxRunIndexField+1)}, []byte(`{}`), nil); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("oversized scenario = %v", err)
	}
}

// runsInFlight lists the runs a server reads when it opens: unfinished ones
// and, after migration 005, unindexed ones.
func runsInFlight(t *testing.T, b Backend) {
	r := openRuns(t, b)
	for n := 1; n <= 5; n++ {
		if _, err := r.SaveRun(ctx, runID(n), "team-a", 0, n%2 == 0, artifact.RunIndex{Result: "completed"}, []byte(`{}`), nil); err != nil {
			t.Fatal(err)
		}
	}
	first, err := r.ListRuns(ctx, artifact.RunQuery{Unfinished: true, Limit: 2})
	if err != nil || len(first.Runs) != 2 || first.Runs[0].ID != runID(5) || first.Runs[1].ID != runID(3) || first.NextCursor == "" {
		t.Fatalf("first unfinished page = %+v, %v", first, err)
	}
	next, err := r.ListRuns(ctx, artifact.RunQuery{Unfinished: true, Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(next.Runs) != 1 || next.Runs[0].ID != runID(1) || next.NextCursor != "" {
		t.Fatalf("last unfinished page = %+v, %v", next, err)
	}
	if _, err := r.ListRuns(ctx, artifact.RunQuery{Limit: 2, Cursor: first.NextCursor}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("unfinished cursor on the whole list = %v", err)
	}
	if page, err := r.ListRuns(ctx, artifact.RunQuery{Unfinished: true, Result: "completed", Projects: []string{"team-b"}}); err != nil || len(page.Runs) != 0 {
		t.Errorf("unfinished runs of another project = %+v, %v", page, err)
	}
	// Every run published through the contract carries an index.
	if page, err := r.ListRuns(ctx, artifact.RunQuery{Unindexed: true}); err != nil || len(page.Runs) != 0 {
		t.Errorf("unindexed = %+v, %v", page, err)
	}
}

func terminalRun(t *testing.T, r artifact.Repository, n int, project string) {
	t.Helper()
	if _, err := r.SaveRun(ctx, runID(n), project, 0, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
}

func evaluations(t *testing.T, b Backend) {
	r := openRuns(t, b)
	terminalRun(t, r, 1, artifact.DefaultProject)
	id := "eval-" + runID(9)[4:]
	if _, err := r.SaveEvaluation(ctx, id, runID(1), []byte(`{}`), []byte(`{"outcome":"pass"}`), []byte("source")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveEvaluation(ctx, "eval-"+runID(8)[4:], runID(2), []byte(`{}`), []byte(`{}`), nil); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("evaluation of an unknown run = %v", err)
	}
	list, err := r.Evaluations(ctx, runID(1))
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].RunID != runID(1) {
		t.Fatalf("evaluations = %+v, %v", list, err)
	}
	record, result, err := r.EvaluationResult(ctx, id)
	if err != nil || record.ID != id || string(result) != `{"outcome":"pass"}` {
		t.Errorf("result = %+v %q, %v", record, result, err)
	}
}

// suiteMetadata is the platform's suite record, which repositories check:
// a caller, a creation time, a config and members naming runs.
func suiteMetadata(members ...string) []byte {
	var list bytes.Buffer
	for i, id := range members {
		if i > 0 {
			list.WriteByte(',')
		}
		fmt.Fprintf(&list, `{"label":"m%d","run_id":%q}`, i, id)
	}
	created := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	return fmt.Appendf(nil, `{"caller":{"principal":"user:test"},"created":%q,"config":"c","members":[%s]}`, created, list.String())
}

func suites(t *testing.T, b Backend) {
	r := openRuns(t, b)
	terminalRun(t, r, 1, "team-a")
	terminalRun(t, r, 2, "team-b")
	if err := r.CheckSuiteCapacity(ctx, 1); err != nil {
		t.Fatal(err)
	}
	id := "suite-" + runID(3)[4:]
	if _, err := r.SaveSuite(ctx, id, "team-a", suiteMetadata(runID(1), runID(2))); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("suite across projects = %v", err)
	}
	if _, err := r.SaveSuite(ctx, id, "team-a", suiteMetadata(runID(1))); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r = openRuns(t, b)
	list, err := r.Suites(ctx)
	if err != nil || len(list) != 1 || list[0].Project != "team-a" {
		t.Fatalf("suites = %+v, %v", list, err)
	}
	if err := r.DeleteRun(ctx, runID(1), time.Now()); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("deleting a suite member = %v", err)
	}
}

func deletion(t *testing.T, b Backend) {
	r := openRuns(t, b)
	if _, err := r.AdmitRun(ctx, admission(runID(1), artifact.DefaultProject, []byte("bundle"))); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteRun(ctx, runID(1), time.Now()); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("deleting an unfinished run = %v", err)
	}
	terminalRun(t, r, 2, artifact.DefaultProject)
	if err := r.DeleteRun(ctx, runID(2), time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r = openRuns(t, b)
	runs, deleted, err := r.Tombstones(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != runID(2) || len(deleted) != 0 {
		t.Fatalf("tombstones = %+v %+v, %v", runs, deleted, err)
	}
	if _, err := r.RunEvidence(ctx, runID(2)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("deleted evidence = %v", err)
	}
	if _, err := r.AdmitRun(ctx, admission(runID(2), artifact.DefaultProject, []byte("again"))); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("readmitting a deleted run = %v", err)
	}
}

// evicted lists the IDs and reasons of one EvictRuns call.
func evicted(t *testing.T, r artifact.Repository, p artifact.RetentionPolicy, now time.Time) []string {
	t.Helper()
	out, err := r.EvictRuns(ctx, p, now)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, e := range out {
		ids = append(ids, e.ID+" "+e.Reason)
	}
	return ids
}

func retention(t *testing.T, b Backend) {
	r := openRuns(t, b)
	for n := 1; n <= 6; n++ {
		terminalRun(t, r, n, artifact.DefaultProject)
	}
	if _, err := r.AdmitRun(ctx, admission(runID(7), artifact.DefaultProject, []byte("bundle"))); err != nil {
		t.Fatal(err)
	}
	// Run 2 is kept, run 3 is in a suite and run 4 is an artifact's origin.
	if err := r.KeepRun(ctx, runID(2), true); err != nil {
		t.Fatal(err)
	}
	if err := r.KeepRun(ctx, runID(9), true); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("keeping an unknown run = %v", err)
	}
	if _, err := r.SaveSuite(ctx, "suite-"+runID(8)[4:], artifact.DefaultProject, suiteMetadata(runID(3))); err != nil {
		t.Fatal(err)
	}
	saved, err := r.Save(ctx, artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: artifact.DefaultProject, Kind: artifact.Scenario,
		Actor: "user:test", Content: []byte("from run 4"), Origin: &artifact.Origin{Kind: "run", BaseDigest: strings.Repeat("a", 64), RunID: runID(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if run, err := r.Run(ctx, runID(2)); err != nil || !run.Kept {
		t.Fatalf("kept run = %+v, %v", run, err)
	}
	now := time.Now()
	// Seven runs against a bound of five: the two oldest evictable go.
	if got := evicted(t, r, artifact.RetentionPolicy{Runs: 5}, now); !slices.Equal(got, []string{runID(1) + " count", runID(5) + " count"}) {
		t.Fatalf("evicted by count = %v", got)
	}
	if got := evicted(t, r, artifact.RetentionPolicy{Runs: 5}, now); len(got) != 0 {
		t.Fatalf("evicted again = %v", got)
	}
	if _, err := r.Run(ctx, runID(1)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("evicted run = %v", err)
	}
	tombstones, _, err := r.Tombstones(ctx)
	if err != nil || len(tombstones) != 2 {
		t.Errorf("tombstones = %+v, %v", tombstones, err)
	}
	// Within the age every run stays; past it every evictable one goes.
	if got := evicted(t, r, artifact.RetentionPolicy{Age: time.Hour}, now); len(got) != 0 {
		t.Fatalf("evicted young runs = %v", got)
	}
	later := now.Add(2 * time.Hour)
	if got := evicted(t, r, artifact.RetentionPolicy{Age: time.Hour}, later); !slices.Equal(got, []string{runID(6) + " age"}) {
		t.Fatalf("evicted by age = %v", got)
	}
	// Unkept, and no longer the head revision's origin: evictable again.
	if err := r.KeepRun(ctx, runID(2), false); err != nil {
		t.Fatal(err)
	}
	if _, err := update(r, saved.Artifact, "edited"); err != nil {
		t.Fatal(err)
	}
	if got := evicted(t, r, artifact.RetentionPolicy{Age: time.Hour}, later); !slices.Equal(got, []string{runID(2) + " age", runID(4) + " age"}) {
		t.Fatalf("evicted once released = %v", got)
	}
	for _, n := range []int{3, 7} {
		if _, err := r.Run(ctx, runID(n)); err != nil {
			t.Errorf("protected run %d = %v", n, err)
		}
	}
}

func prune(t *testing.T, b Backend) {
	r := openRuns(t, b)
	// Runs 3 and 5 share their evidence, and run 6 shares run 1's.
	evidence := map[int]string{1: "one", 2: "two", 3: "shared", 4: "four", 5: "shared", 6: "one"}
	for n := 1; n <= 6; n++ {
		project, result := artifact.DefaultProject, "completed"
		if n == 4 {
			result = "failed"
		}
		if n == 6 {
			project = "team-b"
		}
		if _, err := r.SaveRun(ctx, runID(n), project, 0, true, artifact.RunIndex{Result: result}, []byte(`{}`), []byte(evidence[n])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.AdmitRun(ctx, admission(runID(7), artifact.DefaultProject, []byte("bundle"))); err != nil {
		t.Fatal(err)
	}
	if err := r.KeepRun(ctx, runID(2), true); err != nil {
		t.Fatal(err)
	}
	stats, err := r.RunStats(ctx)
	if err != nil || stats.Runs != 7 || stats.Kept != 1 || stats.Oldest.IsZero() {
		t.Fatalf("run stats = %+v, %v", stats, err)
	}
	now := time.Now()
	q := artifact.PruneQuery{Projects: []string{artifact.DefaultProject}, Result: "completed"}
	for name, q := range map[string]artifact.PruneQuery{
		"no project":     {Projects: []string{}},
		"admitted later": {Before: now.Add(-time.Hour)},
		"no such result": {Result: "lost"},
	} {
		if got, err := r.PruneRuns(ctx, q, false, now); err != nil || len(got.Runs) != 0 || got.Bytes != 0 {
			t.Errorf("%s: pruned %+v, %v", name, got, err)
		}
	}
	// Kept run 2 stays; run 1's evidence stays with run 6, the shared one goes.
	want := artifact.Pruned{Runs: []string{runID(1), runID(3), runID(5)}, Bytes: int64(len("shared"))}
	dry, err := r.PruneRuns(ctx, q, true, now)
	if err != nil || !slices.Equal(dry.Runs, want.Runs) || dry.Bytes != want.Bytes {
		t.Fatalf("dry run = %+v, %v; want %+v", dry, err, want)
	}
	if _, err := r.Run(ctx, runID(1)); err != nil {
		t.Fatalf("a dry run deleted run 1: %v", err)
	}
	pruned, err := r.PruneRuns(ctx, q, false, now)
	if err != nil || !slices.Equal(pruned.Runs, dry.Runs) || pruned.Bytes != dry.Bytes {
		t.Fatalf("prune = %+v, %v; dry run said %+v", pruned, err, dry)
	}
	for n := 1; n <= 7; n++ {
		_, err := r.Run(ctx, runID(n))
		if gone := n == 1 || n == 3 || n == 5; gone != errors.Is(err, artifact.ErrNotFound) {
			t.Errorf("run %d after prune = %v", n, err)
		}
	}
	if again, err := r.PruneRuns(ctx, q, false, now); err != nil || len(again.Runs) != 0 {
		t.Errorf("pruned again = %+v, %v", again, err)
	}
	if stats, err := r.RunStats(ctx); err != nil || stats.Runs != 4 || stats.Kept != 1 {
		t.Errorf("run stats after prune = %+v, %v", stats, err)
	}
}

func retentionStorage(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{StorageBytes: 4 << 20, CatalogBytes: 256 << 10, RunBytes: 128 << 10})
	if err := r.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 6; n++ {
		evidence := bytes.Repeat([]byte{byte('a' + n)}, 100<<10)
		if _, err := r.SaveRun(ctx, runID(n), artifact.DefaultProject, 0, true, artifact.RunIndex{}, []byte(`{}`), evidence); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	// Six runs of 100 KiB against a lowered limit of 350 KiB: the three
	// oldest go.
	r = open(t, b, artifact.Limits{StorageBytes: 350 << 10, CatalogBytes: 32 << 10, RunBytes: 128 << 10})
	if err := r.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		batch := evicted(t, r, artifact.RetentionPolicy{}, time.Now())
		if len(batch) == 0 {
			break
		}
		got = append(got, batch...)
	}
	if want := []string{runID(1) + " storage", runID(2) + " storage", runID(3) + " storage"}; !slices.Equal(got, want) {
		t.Fatalf("evicted by storage = %v, want %v", got, want)
	}
	if used, err := r.Usage(ctx); err != nil || used > 350<<10 {
		t.Errorf("usage = %d, %v", used, err)
	}
}

func captures(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if err := r.EnableCaptures(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := r.SaveCapture(ctx, "capture-1", "team-a", 0, false, false, []byte(`{}`), []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SaveCapture(ctx, "capture-1", "team-a", 0, false, false, []byte(`{}`), []byte("two")); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("stale capture = %v", err)
	}
	if _, err := r.SaveCapture(ctx, "capture-1", "team-b", first.Generation, false, false, []byte(`{}`), nil); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("project change = %v", err)
	}
	if _, err := r.SaveCapture(ctx, "capture-1", "team-a", first.Generation, true, false, []byte(`{"closed":true}`), []byte("two")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	if err := r.EnableCaptures(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := r.Captures(ctx)
	if err != nil || len(list) != 1 || list[0].Project != "team-a" || string(list[0].Metadata) != `{"closed":true}` {
		t.Fatalf("captures = %+v, %v", list, err)
	}
	segments, err := r.CaptureSegments(ctx, "capture-1")
	if err != nil || len(segments) != 2 || string(segments[0]) != "one" || string(segments[1]) != "two" {
		t.Fatalf("segments = %q, %v", segments, err)
	}
	if err := r.DeleteCapture(ctx, "capture-1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.Captures(ctx); len(list) != 0 {
		t.Errorf("deleted capture listed")
	}
}

func limits(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{BlobBytes: 8})
	_, err := r.Save(ctx, artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: artifact.DefaultProject, Kind: artifact.Scenario,
		Actor: "user:test", Content: []byte("more than eight")})
	if !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("oversized blob = %v", err)
	}
	if page, err := r.List(ctx, artifact.ListRequest{}); err != nil || len(page.Items) != 0 {
		t.Errorf("refused save published: %+v, %v", page, err)
	}
	if r.Limits().BlobBytes != 8 {
		t.Errorf("limits = %+v", r.Limits())
	}
}

func closed(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	saved := create(t, r, artifact.DefaultProject, "x")
	r.Close()
	if _, err := r.Get(ctx, saved.Artifact.ID); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("get after close = %v", err)
	}
	_, err := r.Save(ctx, artifact.SaveRequest{WorkspaceID: saved.Artifact.WorkspaceID, Project: artifact.DefaultProject, Kind: artifact.Scenario, Actor: "user:test"})
	if !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("save after close = %v", err)
	}
	if _, err := r.AppendAudit(ctx, auditEvent("user:test", time.Now())); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("audit after close = %v", err)
	}
	if _, err := r.AuditEvents(ctx, artifact.AuditQuery{Limit: 1}); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("audit query after close = %v", err)
	}
	if err := r.SaveToken(ctx, token(1, "user:test")); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("token after close = %v", err)
	}
	if _, err := r.TokenByHash(ctx, token(1, "user:test").Hash); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("token lookup after close = %v", err)
	}
	if _, err := r.SessionByHash(ctx, fmt.Sprintf("%064x", 1)); !errors.Is(err, artifact.ErrClosed) {
		t.Errorf("session lookup after close = %v", err)
	}
}

// tokenCreated is fixed once, so a token built twice compares equal.
var tokenCreated = time.Now().Add(-time.Minute).UTC().Truncate(time.Second)

func token(n int, principal string) artifact.TokenRecord {
	created := tokenCreated
	return artifact.TokenRecord{ID: fmt.Sprintf("%032x", n), Hash: fmt.Sprintf("%064x", n), Principal: principal, Kind: artifact.TokenUser,
		Name: "Test", Groups: []string{"platform"}, Label: fmt.Sprintf("laptop %d", n), CreatedBy: principal, Created: created, Expires: created.Add(time.Hour)}
}

// tokens: a token is found by its hash after reopening, lists newest first
// by principal, and stays listed once revoked; duplicates and invalid
// records are refused.
func tokens(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	for i, who := range []string{"user:alice", "service:ci", "user:alice"} {
		if err := r.SaveToken(ctx, token(i+1, who)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveToken(ctx, token(1, "user:bob")); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	bad := token(9, "user:bob")
	bad.Expires = bad.Created
	if err := r.SaveToken(ctx, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("expiry before creation = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	got, err := r.TokenByHash(ctx, token(2, "service:ci").Hash)
	if want := token(2, "service:ci"); err != nil || got.ID != want.ID || got.Principal != "service:ci" || !got.Expires.Equal(want.Expires) || !slices.Equal(got.Groups, want.Groups) {
		t.Errorf("by hash = %+v, %v", got, err)
	}
	if _, err := r.TokenByHash(ctx, fmt.Sprintf("%064x", 99)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown hash = %v", err)
	}
	alice, err := r.Tokens(ctx, "user:alice")
	if err != nil || len(alice) != 2 || alice[0].ID != token(3, "").ID || alice[1].ID != token(1, "").ID {
		t.Errorf("alice's tokens = %+v, %v", alice, err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	revoked, err := r.RevokeToken(ctx, token(1, "").ID, at)
	if err != nil || revoked.Revoked == nil || !revoked.Revoked.Equal(at) {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	if again, err := r.RevokeToken(ctx, token(1, "").ID, at.Add(time.Hour)); err != nil || !again.Revoked.Equal(at) {
		t.Errorf("revoke again = %+v, %v", again, err)
	}
	if _, err := r.RevokeToken(ctx, token(99, "").ID, at); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("revoke unknown = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	all, err := r.Tokens(ctx, "")
	if err != nil || len(all) != 3 || all[2].Revoked == nil || !all[2].Revoked.Equal(at) || all[0].Revoked != nil {
		t.Errorf("all tokens = %+v, %v", all, err)
	}
}

// sessions: a session is found by its hash after reopening until it is
// ended; an expired session is dropped, and duplicates and invalid records
// are refused.
func sessions(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	now := time.Now().UTC().Truncate(time.Second)
	session := func(n int, expires time.Time) artifact.SessionRecord {
		return artifact.SessionRecord{Hash: fmt.Sprintf("%064x", n), Principal: "user:alice", Name: "Alice", Groups: []string{"platform"},
			Created: expires.Add(-time.Hour), Expires: expires}
	}
	for _, s := range []artifact.SessionRecord{session(1, now.Add(time.Hour)), session(2, now.Add(-time.Second)), session(3, now.Add(time.Hour))} {
		if err := r.SaveSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveSession(ctx, session(1, now.Add(time.Hour))); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	bad := session(9, now)
	bad.Created = bad.Expires
	if err := r.SaveSession(ctx, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("expiry before creation = %v", err)
	}
	if err := r.DeleteSession(ctx, session(3, now).Hash); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSession(ctx, session(3, now).Hash); err != nil {
		t.Errorf("ending an ended session = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	got, err := r.SessionByHash(ctx, session(1, now).Hash)
	if err != nil || got.Principal != "user:alice" || !got.Expires.Equal(now.Add(time.Hour)) || !slices.Equal(got.Groups, []string{"platform"}) {
		t.Errorf("by hash = %+v, %v", got, err)
	}
	if _, err := r.SessionByHash(ctx, session(3, now).Hash); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("ended session = %v", err)
	}
	if _, err := r.SessionByHash(ctx, session(2, now).Hash); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("expired session kept = %v", err)
	}
}

func logins(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	now := time.Now().UTC().Truncate(time.Second)
	login := func(n int, expires time.Time) artifact.LoginRecord {
		return artifact.LoginRecord{Hash: fmt.Sprintf("%064x", n), Verifier: "verifier", Nonce: "nonce", ReturnTo: "/runs", Expires: expires}
	}
	for _, l := range []artifact.LoginRecord{login(1, now.Add(time.Hour)), login(2, now.Add(-time.Second))} {
		if err := r.SaveLogin(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveLogin(ctx, login(1, now.Add(time.Hour))); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("duplicate = %v", err)
	}
	bad := login(9, now.Add(time.Hour))
	bad.ReturnTo = "https://evil.example.com/"
	if err := r.SaveLogin(ctx, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("off-site return = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	got, err := r.TakeLogin(ctx, login(1, now).Hash)
	if want := login(1, now.Add(time.Hour)); err != nil || got.Verifier != want.Verifier || got.Nonce != want.Nonce || got.ReturnTo != want.ReturnTo || !got.Expires.Equal(want.Expires) {
		t.Errorf("take = %+v, %v", got, err)
	}
	if _, err := r.TakeLogin(ctx, login(1, now).Hash); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("second take = %v", err)
	}
	if _, err := r.TakeLogin(ctx, login(2, now).Hash); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("expired take = %v", err)
	}
	for n := range artifact.MaxLogins {
		if err := r.SaveLogin(ctx, login(100+n, now.Add(time.Hour))); err != nil {
			t.Fatalf("login %d: %v", n, err)
		}
	}
	if err := r.SaveLogin(ctx, login(99, now.Add(time.Hour))); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("beyond the bound = %v", err)
	}
}

func auditEvent(principal string, at time.Time) artifact.AuditEvent {
	return artifact.AuditEvent{Time: at, Principal: principal, PrincipalKind: "user", Client: "http", Operation: "GetRun",
		Permission: "run.read", Scope: "project:default", Resources: []string{runID(1)}, Outcome: artifact.AuditAllowed}
}

func appendAudit(t *testing.T, r artifact.Repository, e artifact.AuditEvent) artifact.AuditEvent {
	t.Helper()
	got, err := r.AppendAudit(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func seqs(events []artifact.AuditEvent) []uint64 {
	out := []uint64{}
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

func auditLog(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if events, err := r.AuditEvents(ctx, artifact.AuditQuery{Limit: 10}); err != nil || len(events) != 0 {
		t.Fatalf("empty log = %+v, %v", events, err)
	}
	now := time.Now()
	for i, who := range []string{"user:alice", "user:bob", "user:alice"} {
		e := auditEvent(who, now)
		if i == 1 {
			e.Outcome, e.Permission = artifact.AuditDenied, "run.delete"
		}
		if got := appendAudit(t, r, e); got.Seq != uint64(i+1) {
			t.Fatalf("seq = %d, want %d", got.Seq, i+1)
		}
	}
	invalid := auditEvent("user:alice", now)
	invalid.Outcome = "maybe"
	if _, err := r.AppendAudit(ctx, invalid); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("invalid outcome = %v", err)
	}
	invalid = auditEvent("user:alice\a", now)
	if _, err := r.AppendAudit(ctx, invalid); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("control character = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	for _, c := range []struct {
		q    artifact.AuditQuery
		want []uint64
	}{
		{artifact.AuditQuery{Limit: 10}, []uint64{3, 2, 1}},
		{artifact.AuditQuery{Limit: 2}, []uint64{3, 2}},
		{artifact.AuditQuery{Limit: 10, Before: 2}, []uint64{1}},
		{artifact.AuditQuery{Limit: 10, Principal: "user:alice"}, []uint64{3, 1}},
		{artifact.AuditQuery{Limit: 10, Outcome: artifact.AuditDenied}, []uint64{2}},
		{artifact.AuditQuery{Limit: 10, Operation: "DeleteRun"}, []uint64{}},
		{artifact.AuditQuery{Limit: 10, Since: now.Add(time.Minute)}, []uint64{}},
	} {
		events, err := r.AuditEvents(ctx, c.q)
		if err != nil || !slices.Equal(seqs(events), c.want) {
			t.Errorf("%+v = %v, %v; want %v", c.q, seqs(events), err, c.want)
		}
	}
	events, _ := r.AuditEvents(ctx, artifact.AuditQuery{Limit: 1})
	want := auditEvent("user:alice", now)
	want.Seq = 3
	if len(events) != 1 || !events[0].Time.Equal(now) || events[0].Resources[0] != runID(1) || events[0].Operation != want.Operation ||
		events[0].Scope != want.Scope || events[0].Permission != want.Permission || events[0].Client != want.Client {
		t.Errorf("event = %+v", events)
	}
	if got := appendAudit(t, r, auditEvent("user:carol", now)); got.Seq != 4 {
		t.Errorf("seq after reopen = %d", got.Seq)
	}
}

func auditRetention(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{AuditEvents: 3, AuditAge: time.Hour})
	old := time.Now().Add(-2 * time.Hour)
	appendAudit(t, r, auditEvent("user:old", old))
	for range 4 {
		appendAudit(t, r, auditEvent("user:new", time.Now()))
	}
	events, err := r.AuditEvents(ctx, artifact.AuditQuery{Limit: 10})
	if err != nil || !slices.Equal(seqs(events), []uint64{5, 4, 3}) {
		t.Errorf("by count = %v, %v", seqs(events), err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{AuditEvents: 10, AuditAge: time.Hour})
	events, err = r.AuditEvents(ctx, artifact.AuditQuery{Limit: 10})
	if err != nil || len(events) == 0 || events[len(events)-1].Principal == "user:old" {
		t.Errorf("by age = %+v, %v", events, err)
	}
}

func admit(t *testing.T, r artifact.Repository, n int, runtime string, priority int) {
	t.Helper()
	a := admission(runID(n), artifact.DefaultProject, []byte(fmt.Sprintf("bundle %d", n)))
	a.Runtime, a.Priority = runtime, priority
	if _, err := r.AdmitRun(ctx, a); err != nil {
		t.Fatal(err)
	}
}

func claimed(as []artifact.Assignment) []string {
	ids := []string{}
	for _, a := range as {
		ids = append(ids, a.RunID)
	}
	return ids
}

var epoch = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func queueOrder(t *testing.T, b Backend) {
	r := openRuns(t, b)
	admit(t, r, 1, "a", 0)
	admit(t, r, 2, "b", 0)
	admit(t, r, 3, "a", 0)
	admit(t, r, 4, "a", -1)
	if _, err := r.SaveRun(ctx, runID(5), artifact.DefaultProject, 0, false, artifact.RunIndex{}, []byte(`{}`), nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.Claim(ctx, "w1", "a", 2, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(4), runID(1)}) {
		t.Fatalf("claim = %v, %v", claimed(got), err)
	}
	if a := got[0]; a.Worker != "w1" || a.State != artifact.QueueAssigned || a.Attempt != 1 || !a.Expires.Equal(epoch.Add(time.Minute)) || a.Generation != 1 || a.Runtime != "a" {
		t.Errorf("assignment = %+v", a)
	}
	got, err = r.Claim(ctx, "w1", "a", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(3)}) {
		t.Fatalf("second claim = %v, %v", claimed(got), err)
	}
	if got, err := r.Claim(ctx, "w1", "a", 5, epoch); err != nil || len(got) != 0 {
		t.Errorf("empty claim = %v, %v", claimed(got), err)
	}
	if _, err := r.Assignment(ctx, runID(5)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unadmitted run's assignment = %v", err)
	}
	if a, err := r.Assignment(ctx, runID(2)); err != nil || a.State != artifact.QueueQueued || a.Worker != "" {
		t.Errorf("queued assignment = %+v, %v", a, err)
	}
	a := got[0]
	if err := r.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx, a); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("second start = %v", err)
	}
	if _, err := r.SaveRun(ctx, a.RunID, artifact.DefaultProject, a.Generation, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Assignment(ctx, a.RunID); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("terminal run's assignment = %v", err)
	}
	held, err := r.Renew(ctx, "w1", epoch.Add(2*time.Minute))
	if err != nil || !slices.Equal(claimed(held), []string{runID(1), runID(4)}) || !held[0].Expires.Equal(epoch.Add(2*time.Minute)) {
		t.Errorf("renew = %+v, %v", held, err)
	}
}

func queueExclusive(t *testing.T, b Backend) {
	r := openRuns(t, b)
	// Each unfinished run reserves a run's storage allowance, so ten fit
	// under the default limits.
	for n := 1; n <= 10; n++ {
		admit(t, r, n, "a", 0)
	}
	// A second connection to the same storage claims at the same time, as
	// another instance would.
	other := openRuns(t, b)
	results := make(chan []artifact.Assignment, 2)
	for i, repo := range []artifact.Repository{r, other} {
		go func() {
			var mine []artifact.Assignment
			for {
				got, err := repo.Claim(ctx, fmt.Sprintf("w%d", i), "a", 2, epoch.Add(time.Minute))
				if err != nil || len(got) == 0 {
					results <- mine
					return
				}
				mine = append(mine, got...)
			}
		}()
	}
	seen := map[string]bool{}
	for range 2 {
		for _, a := range <-results {
			if seen[a.RunID] {
				t.Errorf("%s claimed twice", a.RunID)
			}
			seen[a.RunID] = true
		}
	}
	if len(seen) != 10 {
		t.Errorf("claimed %d of 10 runs", len(seen))
	}
}

func queueCancel(t *testing.T, b Backend) {
	r := openRuns(t, b)
	admit(t, r, 1, "a", 0)
	admit(t, r, 2, "a", 1)
	a, err := r.RequestCancel(ctx, runID(2))
	if err != nil || a.Worker != "" || !a.CancelRequested {
		t.Fatalf("cancel queued = %+v, %v", a, err)
	}
	if _, err := r.Assignment(ctx, runID(2)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("cancelled run still queued: %v", err)
	}
	got, err := r.Claim(ctx, "w1", "a", 5, epoch.Add(time.Minute))
	if err != nil || !slices.Equal(claimed(got), []string{runID(1)}) {
		t.Fatalf("claim = %v, %v", claimed(got), err)
	}
	if a, err := r.RequestCancel(ctx, runID(1)); err != nil || a.Worker != "w1" || !a.CancelRequested {
		t.Errorf("cancel assigned = %+v, %v", a, err)
	}
	held, err := r.Renew(ctx, "w1", epoch.Add(time.Minute))
	if err != nil || len(held) != 1 || !held[0].CancelRequested {
		t.Errorf("renew after cancel = %+v, %v", held, err)
	}
	if _, err := r.RequestCancel(ctx, runID(9)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("cancel unknown = %v", err)
	}
	terminalRun(t, r, 3, artifact.DefaultProject)
	if _, err := r.RequestCancel(ctx, runID(3)); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("cancel terminal = %v", err)
	}
}

func queueLeases(t *testing.T, b Backend) {
	r := openRuns(t, b)
	admit(t, r, 1, "a", 0)
	got, err := r.Claim(ctx, "w1", "a", 1, epoch.Add(time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("claim = %v, %v", got, err)
	}
	first := got[0]
	if expired, err := r.Expired(ctx, epoch.Add(time.Minute)); err != nil || len(expired) != 0 {
		t.Errorf("expired at the lease's end = %v, %v", claimed(expired), err)
	}
	if _, err := r.Requeue(ctx, first, epoch); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("requeue of a live lease = %v", err)
	}
	later := epoch.Add(time.Minute + time.Second)
	expired, err := r.Expired(ctx, later)
	if err != nil || !slices.Equal(claimed(expired), []string{runID(1)}) {
		t.Fatalf("expired = %v, %v", claimed(expired), err)
	}
	generation, err := r.Requeue(ctx, expired[0], later)
	if err != nil || generation != first.Generation+1 {
		t.Fatalf("requeue = %d, %v", generation, err)
	}
	if _, err := r.Requeue(ctx, expired[0], later); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("second requeue = %v", err)
	}
	// The former holder can neither start nor publish the run.
	if err := r.Start(ctx, first); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("stale start = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(1), artifact.DefaultProject, first.Generation, true, artifact.RunIndex{}, []byte(`{}`), nil); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("stale publication = %v", err)
	}
	got, err = r.Claim(ctx, "w2", "a", 1, later.Add(time.Minute))
	if err != nil || len(got) != 1 || got[0].Attempt != 2 || got[0].Generation != generation {
		t.Fatalf("reclaim = %+v, %v", got, err)
	}
	if held, err := r.Renew(ctx, "w1", later.Add(time.Minute)); err != nil || len(held) != 0 {
		t.Errorf("former holder renews %v, %v", claimed(held), err)
	}
}

func queueRelease(t *testing.T, b Backend) {
	r := openRuns(t, b)
	admit(t, r, 1, "a", 0)
	got, err := r.Claim(ctx, "w1", "a", 1, epoch.Add(time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("claim = %v, %v", got, err)
	}
	first := got[0]
	if err := r.Start(ctx, first); err != nil {
		t.Fatal(err)
	}
	other := first
	other.Worker = "w2"
	if _, err := r.Release(ctx, other); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("release by another worker = %v", err)
	}
	generation, err := r.Release(ctx, first)
	if err != nil || generation != first.Generation+1 {
		t.Fatalf("release = %d, %v", generation, err)
	}
	if _, err := r.Release(ctx, first); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("second release = %v", err)
	}
	if record, err := r.Run(ctx, runID(1)); err != nil || record.Generation != generation {
		t.Errorf("record generation = %d, %v", record.Generation, err)
	}
	if err := r.Start(ctx, first); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("stale start = %v", err)
	}
	got, err = r.Claim(ctx, "w2", "a", 1, epoch.Add(time.Minute))
	if err != nil || len(got) != 1 || got[0].Attempt != 2 || got[0].Released != 1 || got[0].Generation != generation {
		t.Fatalf("reclaim = %+v, %v", got, err)
	}
	if a, err := r.Assignment(ctx, runID(1)); err != nil || a.Released != 1 {
		t.Errorf("assignment = %+v, %v", a, err)
	}
}

func runtimeIdentity(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func runtimeEntry(name string, n int) artifact.RuntimeEntry {
	return artifact.RuntimeEntry{Name: name, Identity: runtimeIdentity(n), Image: "registry.example/linea-runtime:" + name,
		Digest: fmt.Sprintf("sha256:%064x", 1000+n), Hello: []byte(fmt.Sprintf(`{"runtime":%q}`, runtimeIdentity(n))),
		RegisteredBy: "admin", Registered: epoch}
}

func runtimeCatalog(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if _, err := r.Runtime(ctx, "edge"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("unknown runtime = %v", err)
	}
	for _, e := range []artifact.RuntimeEntry{runtimeEntry("gateway", 1), runtimeEntry("edge", 1), runtimeEntry("agent", 2)} {
		if err := r.SaveRuntime(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	bad := runtimeEntry("bad", 3)
	bad.Identity = "sha256:ABC"
	if err := r.SaveRuntime(ctx, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("invalid identity = %v", err)
	}
	if err := r.ConfirmRuntimes(ctx, runtimeIdentity(1), epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.ConfirmRuntimes(ctx, runtimeIdentity(1), epoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, err := r.RuntimeEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range all {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"agent", "edge", "gateway"}) {
		t.Fatalf("entries = %v", names)
	}
	if !all[0].Confirmed.IsZero() || !all[1].Confirmed.Equal(epoch.Add(time.Minute)) || !all[2].Confirmed.Equal(epoch.Add(time.Minute)) {
		t.Errorf("confirmations = %v, %v, %v", all[0].Confirmed, all[1].Confirmed, all[2].Confirmed)
	}
	want := runtimeEntry("edge", 1)
	want.Confirmed = epoch.Add(time.Minute)
	if got, err := r.Runtime(ctx, "edge"); err != nil || got.Identity != want.Identity || got.Image != want.Image || got.Digest != want.Digest ||
		!bytes.Equal(got.Hello, want.Hello) || got.RegisteredBy != want.RegisteredBy || !got.Registered.Equal(want.Registered) || !got.Confirmed.Equal(want.Confirmed) {
		t.Errorf("edge = %+v, %v", got, err)
	}
	// Registering a name again replaces its entry, confirmation included.
	if err := r.SaveRuntime(ctx, runtimeEntry("edge", 2)); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Runtime(ctx, "edge"); err != nil || got.Identity != runtimeIdentity(2) || !got.Confirmed.IsZero() {
		t.Errorf("re-registered edge = %+v, %v", got, err)
	}
	if err := r.DeleteRuntime(ctx, "edge"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteRuntime(ctx, "edge"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
	for n := len(all) - 1; n < artifact.MaxRuntimeEntries; n++ {
		if err := r.SaveRuntime(ctx, runtimeEntry(fmt.Sprintf("r%03d", n), n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.SaveRuntime(ctx, runtimeEntry("one-too-many", 9999)); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("entry beyond the bound = %v", err)
	}
	if err := r.SaveRuntime(ctx, runtimeEntry("agent", 7)); err != nil {
		t.Errorf("replacing an entry at the bound = %v", err)
	}
	r.Close()
	if got, err := open(t, b, artifact.Limits{}).Runtime(ctx, "gateway"); err != nil || got.Identity != runtimeIdentity(1) {
		t.Errorf("gateway after reopening = %+v, %v", got, err)
	}
}

// runtimeBinaries: an entry with a binary, its build and its manifest
// round-trips, replaces and reopens whole; an entry with one of binary and
// build only, an invalid binary or an oversized manifest is refused and
// leaves the catalog unchanged.
func runtimeBinaries(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	built := runtimeEntry("built", 1)
	built.Image, built.Digest = "", ""
	built.Binary = &artifact.RuntimeBinary{Digest: digestOf("worker binary"), Bytes: 13, OS: "linux", Arch: "arm64"}
	built.Build = buildID(7)
	built.Manifest = []byte("dist:\n  name: built\nprocessors:\n  - gomod: example.com/stamp v0.1.0\n")
	manifest := slices.Clone(built.Manifest)
	want := *built.Binary
	if err := r.SaveRuntime(ctx, built); err != nil {
		t.Fatal(err)
	}
	check := func(r artifact.Repository, when string) {
		t.Helper()
		got, err := r.Runtime(ctx, "built")
		if err != nil || got.Binary == nil || *got.Binary != want || got.Build != built.Build || got.Identity != built.Identity ||
			!bytes.Equal(got.Manifest, manifest) {
			t.Errorf("%s: built = %+v, %v; want binary %+v build %q manifest %q", when, got, err, want, built.Build, manifest)
		}
		all, err := r.RuntimeEntries(ctx)
		if err != nil || len(all) != 1 || all[0].Binary == nil || *all[0].Binary != want || all[0].Build != built.Build ||
			!bytes.Equal(all[0].Manifest, manifest) {
			t.Errorf("%s: entries = %+v, %v", when, all, err)
		}
	}
	check(r, "saved")
	// The caller's entry is not shared with the store.
	built.Binary.Arch = "amd64"
	built.Manifest[0] = 'X'
	check(r, "after the caller changed its copy")
	built.Binary.Arch = want.Arch
	built.Manifest = manifest
	r.Close()
	check(open(t, b, artifact.Limits{}), "reopened")

	r = open(t, b, artifact.Limits{})
	bad := func(name string, edit func(*artifact.RuntimeEntry)) {
		t.Helper()
		e := built
		bin := *built.Binary
		e.Binary = &bin
		e.Name = "bad"
		edit(&e)
		if err := r.SaveRuntime(ctx, e); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v; want ErrInvalidRequest", name, err)
		}
	}
	bad("binary without a build", func(e *artifact.RuntimeEntry) { e.Build = "" })
	bad("build without a binary", func(e *artifact.RuntimeEntry) { e.Binary = nil })
	bad("binary digest", func(e *artifact.RuntimeEntry) { e.Binary.Digest = "sha256:ABC" })
	bad("empty binary", func(e *artifact.RuntimeEntry) { e.Binary.Bytes = 0 })
	bad("oversized binary", func(e *artifact.RuntimeEntry) { e.Binary.Bytes = artifact.MaxRuntimeBinary + 1 })
	bad("binary OS", func(e *artifact.RuntimeEntry) { e.Binary.OS = "Linux" })
	bad("binary architecture", func(e *artifact.RuntimeEntry) { e.Binary.Arch = "" })
	bad("oversized manifest", func(e *artifact.RuntimeEntry) { e.Manifest = bytes.Repeat([]byte("a"), artifact.MaxRuntimeManifest+1) })
	if _, err := r.Runtime(ctx, "bad"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("refused entry = %v; want it absent", err)
	}
	// Registering the name again as an image drops the binary and the manifest.
	image := runtimeEntry("built", 2)
	if err := r.SaveRuntime(ctx, image); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Runtime(ctx, "built"); err != nil || got.Binary != nil || got.Build != "" || got.Image != image.Image || got.Manifest != nil {
		t.Errorf("re-registered = %+v, %v", got, err)
	}
}

// defaultRuntime: the default is unset until one is recorded; a write
// returns the default it replaced, reports a change and survives reopening.
// With entry the name must be an entry, and the entry the default names is
// not removed.
func defaultRuntime(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if _, err := r.DefaultRuntime(ctx); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("default before one was set = %v", err)
	}
	if err := r.SaveRuntime(ctx, runtimeEntry("edge", 1)); err != nil {
		t.Fatal(err)
	}
	head, err := r.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	builtIn := artifact.DefaultRuntime{Name: "built-in", SetBy: "admin", Set: epoch}
	if prev, err := r.SetDefaultRuntime(ctx, builtIn, false); err != nil || prev != (artifact.DefaultRuntime{}) {
		t.Fatalf("first default = %+v, %v", prev, err)
	}
	if _, err := r.SetDefaultRuntime(ctx, artifact.DefaultRuntime{Name: "gateway", SetBy: "admin", Set: epoch}, true); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("default naming no entry = %v", err)
	}
	if _, err := r.SetDefaultRuntime(ctx, artifact.DefaultRuntime{Name: "edge", SetBy: "admin"}, true); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("default without a time = %v", err)
	}
	edge := artifact.DefaultRuntime{Name: "edge", SetBy: "ops", Set: epoch.Add(time.Minute)}
	if prev, err := r.SetDefaultRuntime(ctx, edge, true); err != nil || prev != builtIn {
		t.Fatalf("second default = %+v, %v", prev, err)
	}
	changes, _, err := r.Changes(ctx, head, artifact.MaxChanges)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range changes {
		if c.Kind == artifact.ChangeDefaultRuntime && c.ID == "default" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("default runtime changes = %d of %+v; want 2", n, changes)
	}
	if err := r.DeleteRuntime(ctx, "edge"); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("removing the default's entry = %v", err)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	if got, err := r.DefaultRuntime(ctx); err != nil || got != edge {
		t.Errorf("default after reopening = %+v, %v", got, err)
	}
	if _, err := r.Runtime(ctx, "edge"); err != nil {
		t.Errorf("the default's entry = %v", err)
	}
	if _, err := r.SetDefaultRuntime(ctx, builtIn, false); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteRuntime(ctx, "edge"); err != nil {
		t.Errorf("removing an entry that is no longer the default = %v", err)
	}
}

func roleBinding(n int, group string) artifact.RoleBindingRecord {
	return artifact.RoleBindingRecord{ID: fmt.Sprintf("%032x", n), Group: group, Role: "engineer", Scope: "*", CreatedBy: "user:admin",
		Created: epoch.Add(time.Duration(n) * time.Minute)}
}

// roleBindings: writes raise the revision and record a change, a stale
// revision, a duplicate grant or an existing ID conflicts, removing an
// unknown binding is not found, invalid records are refused, the bound is
// enforced, and the set survives reopening, oldest first.
func roleBindings(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	if set, err := r.RoleBindings(ctx); err != nil || set.Revision != 0 || len(set.Bindings) != 0 {
		t.Fatalf("bindings before a write = %+v, %v", set, err)
	}
	head, err := r.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	platform := roleBinding(1, "platform")
	set, err := r.AddRoleBinding(ctx, 0, platform)
	if err != nil || set.Revision != 1 || len(set.Bindings) != 1 || set.Bindings[0] != platform {
		t.Fatalf("first binding = %+v, %v", set, err)
	}
	ci := artifact.RoleBindingRecord{ID: fmt.Sprintf("%032x", 2), Principal: "service:ci", Role: "viewer", Scope: "project:default",
		CreatedBy: "user:admin", Created: epoch}
	if _, err := r.AddRoleBinding(ctx, 0, ci); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("binding at a stale revision = %v", err)
	}
	if set, err = r.AddRoleBinding(ctx, 1, ci); err != nil || set.Revision != 2 {
		t.Fatalf("second binding = %+v, %v", set, err)
	}
	if got := []string{set.Bindings[0].ID, set.Bindings[1].ID}; got[0] != ci.ID || got[1] != platform.ID {
		t.Errorf("bindings = %v; want oldest first", got)
	}
	again := platform
	again.ID = fmt.Sprintf("%032x", 3)
	if _, err := r.AddRoleBinding(ctx, 2, again); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("duplicate grant = %v", err)
	}
	sameID := roleBinding(1, "other")
	if _, err := r.AddRoleBinding(ctx, 2, sameID); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("existing ID = %v", err)
	}
	for name, bad := range map[string]artifact.RoleBindingRecord{
		"no subject":    {ID: fmt.Sprintf("%032x", 4), Role: "viewer", Scope: "*", Created: epoch},
		"two subjects":  {ID: fmt.Sprintf("%032x", 4), Group: "g", Principal: "user:x", Role: "viewer", Scope: "*", Created: epoch},
		"bad ID":        {ID: "x", Group: "g", Role: "viewer", Scope: "*", Created: epoch},
		"no role":       {ID: fmt.Sprintf("%032x", 4), Group: "g", Scope: "*", Created: epoch},
		"no scope":      {ID: fmt.Sprintf("%032x", 4), Group: "g", Role: "viewer", Created: epoch},
		"control chars": {ID: fmt.Sprintf("%032x", 4), Group: "g\a", Role: "viewer", Scope: "*", Created: epoch},
		"no time":       {ID: fmt.Sprintf("%032x", 4), Group: "g", Role: "viewer", Scope: "*"},
	} {
		if _, err := r.AddRoleBinding(ctx, 2, bad); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := r.RemoveRoleBinding(ctx, 2, fmt.Sprintf("%032x", 9)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("removing an unknown binding = %v", err)
	}
	if _, err := r.RemoveRoleBinding(ctx, 1, ci.ID); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("removing at a stale revision = %v", err)
	}
	if set, err = r.RemoveRoleBinding(ctx, 2, ci.ID); err != nil || set.Revision != 3 || len(set.Bindings) != 1 || set.Bindings[0].ID != platform.ID {
		t.Fatalf("after removal = %+v, %v", set, err)
	}
	changes, _, err := r.Changes(ctx, head, artifact.MaxChanges)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range changes {
		if c.Kind == artifact.ChangeRoleBindings {
			n++
		}
	}
	if n != 3 {
		t.Errorf("role binding changes = %d of %+v; want 3", n, changes)
	}
	r.Close()
	r = open(t, b, artifact.Limits{})
	if set, err := r.RoleBindings(ctx); err != nil || set.Revision != 3 || len(set.Bindings) != 1 || set.Bindings[0] != platform {
		t.Errorf("bindings after reopening = %+v, %v", set, err)
	}
	revision := uint64(3)
	for n := 1; n < artifact.MaxRoleBindings; n++ {
		set, err := r.AddRoleBinding(ctx, revision, roleBinding(100+n, fmt.Sprintf("group-%d", n)))
		if err != nil {
			t.Fatalf("binding %d = %v", n, err)
		}
		revision = set.Revision
	}
	if _, err := r.AddRoleBinding(ctx, revision, roleBinding(5000, "one-too-many")); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("binding beyond the bound = %v", err)
	}
}

// suspectBinaries: MarkSuspect marks every entry naming the binary, keeps
// the first suspicion, survives reopening and leaves other entries alone;
// saving an entry again clears its mark, and an invalid suspicion is
// refused.
func suspectBinaries(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	bin := artifact.RuntimeBinary{Digest: digestOf("suspect binary"), Bytes: 13, OS: "linux", Arch: "amd64"}
	entry := func(name string, n int, bin artifact.RuntimeBinary) artifact.RuntimeEntry {
		e := runtimeEntry(name, n)
		e.Image, e.Digest, e.Binary, e.Build = "", "", &bin, buildID(n)
		return e
	}
	other := bin
	other.Digest = digestOf("another binary")
	for _, e := range []artifact.RuntimeEntry{entry("edge", 1, bin), entry("edge-alias", 1, bin), entry("other", 2, other), runtimeEntry("image", 3)} {
		if err := r.SaveRuntime(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	first := artifact.Suspicion{Cause: "runtime unavailable: runtime binary announced another runtime", Pool: artifact.PoolIdentity("linux/amd64"), At: epoch}
	if n, err := r.MarkSuspect(ctx, bin.Digest, first); err != nil || n != 2 {
		t.Fatalf("MarkSuspect = %d, %v; want 2 entries", n, err)
	}
	later := first
	later.Cause, later.At = "a later failure", epoch.Add(time.Minute)
	if n, err := r.MarkSuspect(ctx, bin.Digest, later); err != nil || n != 0 {
		t.Fatalf("MarkSuspect again = %d, %v; want none", n, err)
	}
	if n, err := r.MarkSuspect(ctx, digestOf("unknown"), first); err != nil || n != 0 {
		t.Fatalf("MarkSuspect of an unknown binary = %d, %v", n, err)
	}
	check := func(r artifact.Repository, when string) {
		t.Helper()
		for _, name := range []string{"edge", "edge-alias"} {
			got, err := r.Runtime(ctx, name)
			if err != nil || got.Suspect == nil || *got.Suspect != first {
				t.Errorf("%s: %s suspicion = %+v, %v; want %+v", when, name, got.Suspect, err, first)
			}
		}
		for _, name := range []string{"other", "image"} {
			if got, err := r.Runtime(ctx, name); err != nil || got.Suspect != nil {
				t.Errorf("%s: %s = %+v, %v; want no suspicion", when, name, got.Suspect, err)
			}
		}
		all, err := r.RuntimeEntries(ctx)
		if err != nil || len(all) != 4 || all[0].Name != "edge" || all[0].Suspect == nil || *all[0].Suspect != first {
			t.Errorf("%s: entries = %+v, %v", when, all, err)
		}
	}
	check(r, "marked")
	r.Close()
	r = open(t, b, artifact.Limits{})
	check(r, "reopened")

	// Saving an entry again, even with a suspicion, clears its mark only.
	again := entry("edge", 1, bin)
	again.Suspect = &later
	if err := r.SaveRuntime(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Runtime(ctx, "edge"); err != nil || got.Suspect != nil {
		t.Errorf("saved again: suspicion = %+v, %v", got.Suspect, err)
	}
	if got, err := r.Runtime(ctx, "edge-alias"); err != nil || got.Suspect == nil {
		t.Errorf("saving edge cleared edge-alias: %+v, %v", got.Suspect, err)
	}

	for name, m := range map[string]artifact.Suspicion{
		"no cause":      {Pool: first.Pool, At: epoch},
		"long cause":    {Cause: strings.Repeat("x", artifact.MaxSuspicionCause+1), Pool: first.Pool, At: epoch},
		"control cause": {Cause: "bad\x00line", Pool: first.Pool, At: epoch},
		"no pool":       {Cause: "x", At: epoch},
		"not a pool":    {Cause: "x", Pool: runtimeIdentity(1), At: epoch},
		"no time":       {Cause: "x", Pool: first.Pool},
	} {
		if _, err := r.MarkSuspect(ctx, bin.Digest, m); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v; want ErrInvalidRequest", name, err)
		}
	}
}

// readChanges reads the feed from cursor to its end, in pages of limit.
func readChanges(t *testing.T, r artifact.Repository, cursor artifact.ChangeCursor, limit int) ([]string, artifact.ChangeCursor) {
	t.Helper()
	out := []string{}
	for {
		page, next, err := r.Changes(ctx, cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range page {
			out = append(out, string(c.Kind)+" "+c.ID)
		}
		cursor = next
		if len(page) < limit {
			return out, cursor
		}
	}
}

func changeFeed(t *testing.T, b Backend) {
	writer, reader := openRuns(t, b), openRuns(t, b)
	hints := reader.ChangeHints()
	head, err := reader.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admit(t, writer, 1, "a", 0)
	if _, err := writer.RequestCancel(ctx, runID(1)); err != nil {
		t.Fatal(err)
	}
	terminalRun(t, writer, 2, artifact.DefaultProject)
	if _, err := writer.SaveEvaluation(ctx, "eval-"+runID(9)[4:], runID(2), []byte(`{}`), []byte(`{}`), []byte("source")); err != nil {
		t.Fatal(err)
	}
	terminalRun(t, writer, 3, artifact.DefaultProject)
	if err := writer.DeleteRun(ctx, runID(3), time.Now()); err != nil {
		t.Fatal(err)
	}
	want := []string{"run " + runID(1), "run " + runID(1), "run " + runID(2), "evaluation " + runID(2), "run " + runID(3), "deletion " + runID(3)}
	got, end := readChanges(t, reader, head, artifact.MaxChanges)
	if !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
	if paged, _ := readChanges(t, reader, head, 2); !slices.Equal(paged, want) {
		t.Errorf("paged changes = %q", paged)
	}
	if rest, _ := readChanges(t, reader, end, artifact.MaxChanges); len(rest) != 0 {
		t.Errorf("changes after the end = %q", rest)
	}
	if _, _, err := reader.Changes(ctx, head, 0); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("zero limit = %v", err)
	}
	select {
	case <-hints:
	case <-time.After(10 * time.Second):
		t.Error("no change hint")
	}
	record, err := reader.Run(ctx, runID(2))
	if err != nil || record.ID != runID(2) || !record.Terminal {
		t.Errorf("run = %+v, %v", record, err)
	}
	if _, err := reader.Run(ctx, runID(3)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("deleted run = %v", err)
	}
}

func batch(attempt, first, last int) artifact.EventBatch {
	return artifact.EventBatch{Attempt: attempt, First: first, Last: last, Events: fmt.Appendf(nil, "%d:%d-%d", attempt, first, last)}
}

func batches(bs []artifact.EventBatch) []string {
	out := []string{}
	for _, b := range bs {
		out = append(out, string(b.Events))
	}
	return out
}

func liveEvents(t *testing.T, b Backend) {
	r := openRuns(t, b)
	admitted, err := r.AdmitRun(ctx, admission(runID(1), artifact.DefaultProject, []byte("bundle")))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []artifact.EventBatch{batch(1, 1, 2), batch(1, 3, 3)} {
		if err := r.AppendRunEvents(ctx, runID(1), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.AppendRunEvents(ctx, runID(1), batch(1, 2, 4)); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("overlapping batch = %v", err)
	}
	if err := r.AppendRunEvents(ctx, runID(1), artifact.EventBatch{Attempt: 1, First: 5, Last: 4, Events: []byte("x")}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("invalid batch = %v", err)
	}
	if err := r.AppendRunEvents(ctx, runID(2), batch(1, 1, 1)); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("unknown run = %v", err)
	}
	if got, err := r.RunEvents(ctx, runID(1), 2); err != nil || !slices.Equal(batches(got), []string{"1:3-3"}) {
		t.Errorf("events after 2 = %q, %v", batches(got), err)
	}
	if err := r.AppendRunEvents(ctx, runID(1), batch(2, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got, err := r.RunEvents(ctx, runID(1), 0); err != nil || !slices.Equal(batches(got), []string{"2:1-1"}) {
		t.Errorf("events of a later attempt = %q, %v", batches(got), err)
	}
	if err := r.AppendRunEvents(ctx, runID(1), batch(1, 4, 4)); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("batch of an earlier attempt = %v", err)
	}
	if _, err := r.SaveRun(ctx, runID(1), artifact.DefaultProject, admitted.Generation, true, artifact.RunIndex{}, []byte(`{}`), []byte("evidence")); err != nil {
		t.Fatal(err)
	}
	if got, err := r.RunEvents(ctx, runID(1), 0); err != nil || len(got) != 0 {
		t.Errorf("events of a terminal run = %q, %v", batches(got), err)
	}
	if err := r.AppendRunEvents(ctx, runID(1), batch(2, 2, 2)); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("batch of a terminal run = %v", err)
	}
}

func interactiveJobs(t *testing.T, b Backend) {
	requester, claimer := openRuns(t, b), openRuns(t, b)
	head, err := claimer.ChangeHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	job := func(id, runtime string) artifact.JobRecord {
		return artifact.JobRecord{ID: id, Runtime: runtime, Kind: artifact.JobInspect, Input: []byte(`{"id":"` + id + `"}`)}
	}
	for _, j := range []artifact.JobRecord{job("job-1", "rt-a"), job("job-2", "rt-b"), job("job-3", "rt-a"), job("job-4", "rt-a")} {
		if err := requester.PostJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := requester.PostJob(ctx, job("job-1", "rt-a")); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("duplicate job = %v", err)
	}
	if err := requester.PostJob(ctx, artifact.JobRecord{ID: "job-5", Runtime: "rt-a", Kind: "run", Input: []byte("x")}); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("unknown kind = %v", err)
	}

	claimed, err := claimer.ClaimJobs(ctx, "worker-1", "rt-a", 2)
	if err != nil || len(claimed) != 2 || claimed[0].ID != "job-1" || claimed[1].ID != "job-3" {
		t.Fatalf("claimed = %+v, %v", claimed, err)
	}
	if c := claimed[0]; c.State != artifact.JobClaimed || c.Worker != "worker-1" || c.Kind != artifact.JobInspect || string(c.Input) != `{"id":"job-1"}` {
		t.Errorf("claimed job = %+v", c)
	}
	if again, err := requester.ClaimJobs(ctx, "worker-2", "rt-a", 5); err != nil || len(again) != 1 || again[0].ID != "job-4" {
		t.Errorf("second claim = %+v, %v", again, err)
	}
	if _, err := claimer.ClaimJobs(ctx, "worker-1", "rt-a", 0); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("zero claim = %v", err)
	}

	if err := claimer.FinishJob(ctx, "job-1", "worker-2", []byte("result"), nil); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("finish by another worker = %v", err)
	}
	if err := claimer.FinishJob(ctx, "job-1", "worker-1", []byte("result"), []byte("failure")); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("finish with both answers = %v", err)
	}
	if err := claimer.FinishJob(ctx, "job-1", "worker-1", []byte("result"), nil); err != nil {
		t.Fatal(err)
	}
	if err := claimer.FinishJob(ctx, "job-3", "worker-1", nil, []byte("failure")); err != nil {
		t.Fatal(err)
	}
	if err := claimer.FinishJob(ctx, "job-1", "worker-1", []byte("again"), nil); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("second finish = %v", err)
	}
	if done, err := requester.Job(ctx, "job-1"); err != nil || done.State != artifact.JobDone || string(done.Result) != "result" || len(done.Error) != 0 {
		t.Errorf("answered job = %+v, %v", done, err)
	}
	if failed, err := requester.Job(ctx, "job-3"); err != nil || failed.State != artifact.JobDone || len(failed.Result) != 0 || string(failed.Error) != "failure" {
		t.Errorf("failed job = %+v, %v", failed, err)
	}

	if err := requester.WithdrawJob(ctx, "job-4"); !errors.Is(err, artifact.ErrConflict) {
		t.Errorf("withdraw a claimed job = %v", err)
	}
	if err := requester.WithdrawJob(ctx, "job-2"); err != nil {
		t.Fatal(err)
	}
	if err := requester.WithdrawJob(ctx, "job-2"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("withdraw twice = %v", err)
	}
	if err := requester.DeleteJob(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := requester.Job(ctx, "job-1"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("deleted job = %v", err)
	}
	if err := claimer.FinishJob(ctx, "job-1", "worker-1", []byte("late"), nil); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("finish a deleted job = %v", err)
	}
	if err := requester.TrimJobs(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := requester.Job(ctx, "job-4"); !errors.Is(err, artifact.ErrNotFound) {
		t.Errorf("trimmed job = %v", err)
	}

	// Posting and answering are changes; claiming is not.
	want := []string{"job job-1", "job job-2", "job job-3", "job job-4", "job job-1", "job job-3"}
	if got, _ := readChanges(t, claimer, head, artifact.MaxChanges); !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
}

func workerPresence(t *testing.T, b Backend) {
	a, other := open(t, b, artifact.Limits{}), open(t, b, artifact.Limits{})
	w := func(id string, n, capacity int) artifact.PresentWorker {
		return artifact.PresentWorker{ID: id, Identity: runtimeIdentity(n), Capacity: capacity}
	}
	counts := func(now time.Time) map[string]artifact.WorkerCount {
		t.Helper()
		c, err := other.WorkerCounts(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := counts(epoch); len(c) != 0 {
		t.Fatalf("empty counts = %v", c)
	}
	if err := a.SetWorkers(ctx, "instance-a", []artifact.PresentWorker{w("w1", 1, 2), w("w2", 1, 3), w("w3", 2, 1)}, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := other.SetWorkers(ctx, "instance-b", []artifact.PresentWorker{w("w1", 1, 4)}, epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want := map[string]artifact.WorkerCount{runtimeIdentity(1): {Workers: 3, Slots: 9}, runtimeIdentity(2): {Workers: 1, Slots: 1}}
	if c := counts(epoch); !maps.Equal(c, want) {
		t.Fatalf("counts = %v, want %v", c, want)
	}
	// A record replaces the instance's previous one.
	if err := a.SetWorkers(ctx, "instance-a", []artifact.PresentWorker{w("w2", 1, 3)}, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	want = map[string]artifact.WorkerCount{runtimeIdentity(1): {Workers: 2, Slots: 7}}
	if c := counts(epoch); !maps.Equal(c, want) {
		t.Fatalf("counts after a replacement = %v, want %v", c, want)
	}
	// An expired record no longer counts.
	want = map[string]artifact.WorkerCount{runtimeIdentity(1): {Workers: 1, Slots: 4}}
	if c := counts(epoch.Add(time.Minute)); !maps.Equal(c, want) {
		t.Fatalf("counts after instance-a expired = %v, want %v", c, want)
	}
	// No workers removes the record.
	if err := other.SetWorkers(ctx, "instance-b", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	want = map[string]artifact.WorkerCount{runtimeIdentity(1): {Workers: 1, Slots: 3}}
	if c := counts(epoch); !maps.Equal(c, want) {
		t.Fatalf("counts after instance-b left = %v, want %v", c, want)
	}
	// Pools are recorded under their platform, with the runtimes they hold
	// running; an instance reads the warm runtimes of the others only.
	pool := func(id string, warm ...int) artifact.PresentWorker {
		p := artifact.PresentWorker{ID: id, Identity: artifact.PoolIdentity("linux/amd64"), Capacity: 2}
		for _, n := range warm {
			p.Warm = append(p.Warm, runtimeIdentity(n))
		}
		return p
	}
	// A pool that runs its own build also counts as a worker of it.
	own := pool("p2")
	own.Own = runtimeIdentity(4)
	if err := a.SetWorkers(ctx, "instance-a", []artifact.PresentWorker{w("w2", 1, 3), pool("p1", 1, 2), own}, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := other.SetWorkers(ctx, "instance-b", []artifact.PresentWorker{pool("p1", 3)}, epoch.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	want = map[string]artifact.WorkerCount{runtimeIdentity(1): {Workers: 1, Slots: 3}, runtimeIdentity(4): {Workers: 1, Slots: 2, OwnWorkers: 1, OwnSlots: 2},
		artifact.PoolIdentity("linux/amd64"): {Workers: 3, Slots: 6}}
	if c := counts(epoch); !maps.Equal(c, want) {
		t.Fatalf("counts with pools = %v, want %v", c, want)
	}
	warm := func(r artifact.Repository, now time.Time, except string) map[string]bool {
		t.Helper()
		got, err := r.WarmRuntimes(ctx, now, except)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, c := range []struct {
		now    time.Time
		except string
		want   map[string]bool
	}{
		{epoch, "instance-b", map[string]bool{runtimeIdentity(1): true, runtimeIdentity(2): true}},
		{epoch, "instance-a", map[string]bool{runtimeIdentity(3): true}},
		{epoch, "", map[string]bool{runtimeIdentity(1): true, runtimeIdentity(2): true, runtimeIdentity(3): true}},
		{epoch.Add(time.Minute), "instance-b", map[string]bool{}},
	} {
		if got := warm(other, c.now, c.except); !maps.Equal(got, c.want) {
			t.Errorf("warm at %v except %q = %v, want %v", c.now.Sub(epoch), c.except, got, c.want)
		}
	}
	if err := other.SetWorkers(ctx, "instance-b", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetWorkers(ctx, "instance-a", []artifact.PresentWorker{w("w2", 1, 3)}, epoch.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	tooWarm := pool("p9")
	for n := range artifact.MaxPresentWarm + 1 {
		tooWarm.Warm = append(tooWarm.Warm, runtimeIdentity(n+1))
	}
	if err := a.SetWorkers(ctx, "instance-a", []artifact.PresentWorker{tooWarm}, epoch.Add(time.Minute)); !errors.Is(err, artifact.ErrLimit) {
		t.Errorf("a pool with too many warm runtimes = %v", err)
	}
	warmWorker := w("w9", 1, 1)
	warmWorker.Warm = []string{runtimeIdentity(2)}
	ownWorker := w("w9", 1, 1)
	ownWorker.Own = runtimeIdentity(2)
	ownless := pool("p9")
	ownless.Own = "edge"
	for name, workers := range map[string][]artifact.PresentWorker{
		"no identity":       {{ID: "w9", Identity: "edge", Capacity: 1}},
		"no capacity":       {w("w9", 1, 0)},
		"no ID":             {w("", 1, 1)},
		"no platform":       {{ID: "p9", Identity: "pool:linux", Capacity: 1}},
		"warm, not a pool":  {warmWorker},
		"warm, no identity": {{ID: "p9", Identity: artifact.PoolIdentity("linux/amd64"), Capacity: 1, Warm: []string{"edge"}}},
		"own, not a pool":   {ownWorker},
		"own, no identity":  {ownless},
	} {
		if err := a.SetWorkers(ctx, "instance-a", workers, epoch.Add(time.Minute)); !errors.Is(err, artifact.ErrInvalidRequest) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if err := a.SetWorkers(ctx, "", nil, epoch); !errors.Is(err, artifact.ErrInvalidRequest) {
		t.Errorf("no instance = %v", err)
	}
}

// roleBindingRace: writers racing at the same revision, adding and
// removing, produce exactly one change; the others conflict, and the set
// holds what that one write left.
func roleBindingRace(t *testing.T, b Backend) {
	r := open(t, b, artifact.Limits{})
	set, err := r.AddRoleBinding(ctx, 0, roleBinding(1, "admins"))
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
			if i == 0 {
				_, errs[i] = r.RemoveRoleBinding(ctx, set.Revision, set.Bindings[0].ID)
				return
			}
			_, errs[i] = r.AddRoleBinding(ctx, set.Revision, roleBinding(i+1, fmt.Sprintf("group-%d", i)))
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
	after, err := r.RoleBindings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if won != 1 || after.Revision != set.Revision+1 {
		t.Fatalf("%d writers won, revision %d after %d", won, after.Revision, set.Revision)
	}
	if errs[0] == nil {
		if len(after.Bindings) != 0 {
			t.Errorf("the removal won but the set is %+v", after.Bindings)
		}
	} else if len(after.Bindings) != 2 || after.Bindings[0].ID != set.Bindings[0].ID {
		t.Errorf("an add won but the set is %+v", after.Bindings)
	}
}
