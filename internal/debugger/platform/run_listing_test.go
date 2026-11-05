package platform

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestListRunsSummarizesEachReportAndFilters(t *testing.T) {
	reports := map[string]*engine.Report{
		"built-in pass": {Scenario: "errors", Runtime: &engine.ReportRuntime{Kind: engine.RuntimeBuiltIn, CollectorVersion: "0.162.0", Verified: true},
			Result:     &runresult.Result{Status: runresult.StatusCompleted, Inputs: []runresult.InputResult{{Injected: true}, {Injected: true}}},
			Assertions: &assertion.Report{Outcome: assertion.Fail, Results: []assertion.Result{{Outcome: assertion.Pass}, {Outcome: assertion.Fail}}}},
		"custom partial": {Scenario: "sampling", Runtime: &engine.ReportRuntime{Kind: "container", Name: "acme", CollectorVersion: "0.162.0"},
			Result: &runresult.Result{Status: runresult.StatusPartial, Inputs: []runresult.InputResult{{Injected: true}, {}}, Truncation: &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget}}},
	}
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		rep, ok := reports[req.FixtureJSON]
		if !ok {
			return nil, errors.New("collector exited")
		}
		out := *rep
		out.Schema = engine.ReportSchemaVersion
		result := *rep.Result
		result.RunID = req.RunID
		out.Result = &result
		return &out, nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	ids := map[string]string{}
	for _, label := range []string{"built-in pass", "custom partial", "failed"} {
		r, err := s.SubmitRun(ctx, testCaller, quickRun(label))
		if err != nil {
			t.Fatal(err)
		}
		h, _ := s.Get(r.ID)
		waitRun(t, h)
		ids[label] = r.ID
	}

	all, err := allRuns(ctx, s, testCaller, RunFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("runs = %v, %v", all, err)
	}
	byID := map[string]Run{}
	for _, r := range all {
		if r.Report != nil {
			t.Fatalf("run %s lists its report", r.ID)
		}
		byID[r.ID] = r
	}
	pass := byID[ids["built-in pass"]].Listing
	if pass == nil || pass.Scenario != "errors" || pass.Status != runresult.StatusCompleted || pass.Inputs != 2 || pass.Injected != 2 || pass.Truncated ||
		*pass.Runtime != (RunListingRuntime{Kind: engine.RuntimeBuiltIn, Name: BuiltInRuntime, CollectorVersion: "0.162.0", Verified: true}) ||
		*pass.Assertions != (AssertionSummary{Outcome: assertion.Fail, Pass: 1, Fail: 1}) {
		t.Fatalf("built-in listing = %+v", pass)
	}
	partial := byID[ids["custom partial"]].Listing
	if partial == nil || partial.Runtime.Name != "acme" || partial.Injected != 1 || partial.Inputs != 2 || !partial.Truncated || partial.Assertions != nil {
		t.Fatalf("custom listing = %+v", partial)
	}
	if failed := byID[ids["failed"]]; failed.Listing != nil || failed.State != StateError {
		t.Fatalf("a run without a report = %+v", failed)
	}

	for _, tc := range []struct {
		name   string
		filter RunFilter
		want   []string
	}{
		{"scenario", RunFilter{Scenario: "errors"}, []string{"built-in pass"}},
		{"built-in runtime", RunFilter{Runtime: BuiltInRuntime}, []string{"built-in pass"}},
		{"named runtime", RunFilter{Runtime: "acme"}, []string{"custom partial"}},
		{"execution result", RunFilter{Result: "partial"}, []string{"custom partial"}},
		{"run state without a report", RunFilter{Result: "error"}, []string{"failed"}},
		{"combined", RunFilter{Scenario: "errors", Result: "partial"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs, err := allRuns(ctx, s, testCaller, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			var got, want []string
			for _, r := range runs {
				got = append(got, r.ID)
			}
			for _, label := range tc.want {
				want = append(want, ids[label])
			}
			if !slices.Equal(got, want) {
				t.Fatalf("runs = %v, want %v", got, want)
			}
		})
	}

	_, err = allRuns(ctx, s, testCaller, RunFilter{Result: "passed"})
	wantCode(t, err, CodeInvalidRequest)
}

// TestListRunsPagesFromTheRepository pages a repository's runs newest first
// with and without a filter, and refuses limits out of bounds and cursors of
// another query.
func TestListRunsPagesFromTheRepository(t *testing.T) {
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		status := runresult.StatusCompleted
		if req.FixtureJSON == "partial" {
			status = runresult.StatusPartial
		}
		return report(req, status), nil
	}
	s, err := Open(memory.Open(memory.NewData(), artifact.Limits{}), Options{Prepare: unprepared, Execute: execute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	var ids, partial []string
	for i := range 5 {
		label := "completed"
		if i%2 == 1 {
			label = "partial"
		}
		h := waitRun(t, submit(t, s, request(label)))
		ids = append([]string{h.ID}, ids...)
		if label == "partial" {
			partial = append([]string{h.ID}, partial...)
		}
	}
	pages := func(q RunQuery) [][]string {
		var got [][]string
		for {
			page, err := s.ListRuns(ctx, testCaller, q)
			if err != nil {
				t.Fatal(err)
			}
			var listed []string
			for _, r := range page.Runs {
				if r.Listing == nil || r.Report != nil {
					t.Fatalf("listed run = %+v", r)
				}
				listed = append(listed, r.ID)
			}
			got = append(got, listed)
			if page.NextCursor == "" {
				return got
			}
			q.Cursor = page.NextCursor
		}
	}
	if got, want := pages(RunQuery{Limit: 2}), [][]string{ids[:2], ids[2:4], ids[4:]}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
	if got, want := pages(RunQuery{RunFilter: RunFilter{Result: "partial"}, Limit: 1}), [][]string{partial[:1], partial[1:]}; !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("partial pages = %v, want %v", got, want)
	}
	first, err := s.ListRuns(ctx, testCaller, RunQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []RunQuery{{Limit: -1}, {Limit: MaxRunPage + 1}, {RunFilter: RunFilter{Result: "partial"}, Cursor: first.NextCursor}, {Cursor: "x"}} {
		_, err := s.ListRuns(ctx, testCaller, q)
		wantCode(t, err, CodeInvalidRequest)
	}
}

// unindexedRepository lists every run not indexed through it as stored
// before run indexes existed, and records the indexes the platform sets.
type unindexedRepository struct {
	artifact.Repository
	indexed map[string]artifact.RunIndex
}

func (r *unindexedRepository) ListRuns(ctx context.Context, q artifact.RunQuery) (*artifact.RunPage, error) {
	if !q.Unindexed {
		return r.Repository.ListRuns(ctx, q)
	}
	runs, err := r.Repository.Runs(ctx)
	page := &artifact.RunPage{}
	for _, run := range runs {
		if _, ok := r.indexed[run.ID]; !ok {
			run.Index = nil
			page.Runs = append(page.Runs, run)
		}
	}
	return page, err
}

func (r *unindexedRepository) IndexRun(ctx context.Context, id string, index artifact.RunIndex) error {
	r.indexed[id] = index
	return r.Repository.IndexRun(ctx, id, index)
}

// TestOpenIndexesRunsStoredWithoutAnIndex indexes, on recovery, runs a
// repository holds without an index, as the run's own publications would.
func TestOpenIndexesRunsStoredWithoutAnIndex(t *testing.T) {
	data := memory.NewData()
	execute := func(_ context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusFailed)
		rep.Scenario = "checkout"
		return rep, nil
	}
	s, err := Open(memory.Open(data, artifact.Limits{}), Options{Prepare: unprepared, Execute: execute})
	if err != nil {
		t.Fatal(err)
	}
	run := waitRun(t, submit(t, s, request("x")))
	s.Close()
	repository := &unindexedRepository{Repository: memory.Open(data, artifact.Limits{}), indexed: map[string]artifact.RunIndex{}}
	s, err = Open(repository, Options{Prepare: unprepared, Execute: execute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if got := repository.indexed[run.ID]; got != (artifact.RunIndex{Scenario: "checkout", Result: "failed"}) {
		t.Fatalf("index = %+v", repository.indexed)
	}
}

// allRuns lists every page of the runs f matches, newest first.
func allRuns(ctx context.Context, s *Service, caller Caller, f RunFilter) ([]Run, error) {
	q := RunQuery{RunFilter: f, Limit: MaxRunPage}
	runs := []Run{}
	for {
		page, err := s.ListRuns(ctx, caller, q)
		if err != nil {
			return nil, err
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs, nil
		}
		q.Cursor = page.NextCursor
	}
}
