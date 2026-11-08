package evidence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/recommend"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// recordedStage returns a completed report whose processor stage
// transform/logs received one batch of the given log bodies.
func recordedStage(bodies ...string) *engine.Report {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	sl := rl.ScopeLogs().AppendEmpty()
	sl.Scope().SetName("app")
	for _, b := range bodies {
		sl.LogRecords().AppendEmpty().Body().SetStr(b)
	}
	rec := instrument.NewRecorderWithID("run")
	seq := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindIngress, Component: "transform", Pipeline: "logs"}, telemetry.FromLogs(ld))
	return &engine.Report{
		Result: &runresult.Result{Status: runresult.StatusCompleted, Events: rec.Events(), Snapshots: rec.Snapshots()},
		Stages: []analysis.Stage{{Component: "transform", Pipeline: "logs", Signal: "logs", Inputs: []analysis.StageInput{{Seq: seq}}}},
	}
}

var recordsQuery = Query{Family: FamilyRecords, Pipeline: "logs", Component: "transform", Side: SideBefore}

// pageAll reads every page of q and returns the refs in order.
func pageAll(t *testing.T, run Run, q Query, items, bytes int) ([]string, []*Result) {
	t.Helper()
	var refs []string
	var pages []*Result
	cursor := ""
	for range 1000 {
		res, err := QueryRun(run, q, Page{Cursor: cursor, Items: items, Bytes: bytes})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, res)
		for _, it := range res.Items {
			refs = append(refs, it.Ref)
		}
		if res.NextCursor == "" {
			return refs, pages
		}
		cursor = res.NextCursor
	}
	t.Fatal("paging did not end")
	return nil, nil
}

func TestManySmallRecordsPageWithStableCursors(t *testing.T) {
	var bodies []string
	for i := range 25 {
		bodies = append(bodies, fmt.Sprintf("line %d", i))
	}
	run := Run{ID: "run", End: EndDone, Report: recordedStage(bodies...)}
	refs, pages := pageAll(t, run, recordsQuery, 10, 1<<20)
	if len(refs) != 25 || len(pages) != 3 || refs[0] != "1:resource[0].scope[0].log_record[0]" || refs[24] != "1:resource[0].scope[0].log_record[24]" {
		t.Fatalf("refs = %v over %d pages", refs, len(pages))
	}
	for i, p := range pages {
		last := i == len(pages)-1
		if p.Total != 25 || p.Complete != last || (p.TruncatedReason == ReasonPageItems) == last {
			t.Fatalf("page %d: total %d, complete %t, reason %q", i, p.Total, p.Complete, p.TruncatedReason)
		}
	}
	var first RecordItem
	if err := json.Unmarshal(pages[0].Items[0].Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Fields["body"] != "line 0" || first.Resource.Attributes["service.name"] != "checkout" || first.Scope.Name != "app" || first.Signal != "logs" {
		t.Fatalf("record = %+v", first)
	}

	// The same cursor gives the same page.
	again, err := QueryRun(run, recordsQuery, Page{Cursor: pages[0].NextCursor, Items: 10, Bytes: 1 << 20})
	if err != nil || again.Items[0].Ref != pages[1].Items[0].Ref || again.NextCursor != pages[1].NextCursor {
		t.Fatalf("cursor not stable: %v", err)
	}

	// A byte bound cuts pages too, and never skips an item.
	size := len(pages[0].Items[0].Data)
	byBytes, bytePages := pageAll(t, run, recordsQuery, 100, 3*size+size/2)
	if !slices.Equal(byBytes, refs) || bytePages[0].TruncatedReason != ReasonPageBytes || len(bytePages[0].Items) != 3 {
		t.Fatalf("byte pages: %d items first, reason %q", len(bytePages[0].Items), bytePages[0].TruncatedReason)
	}
}

func TestOneHugeRecordIsAnOversizeEntry(t *testing.T) {
	run := Run{ID: "run", End: EndDone, Report: recordedStage("small", strings.Repeat("x", 4096), "after")}
	refs, pages := pageAll(t, run, recordsQuery, 100, 1024)
	if len(refs) != 3 {
		t.Fatalf("refs = %v", refs)
	}
	var items []Item
	for _, p := range pages {
		items = append(items, p.Items...)
	}
	huge := items[1]
	if huge.Data != nil || huge.Oversize == nil || huge.Oversize.Bytes <= 4096 || huge.Ref != "1:resource[0].scope[0].log_record[1]" {
		t.Fatalf("huge item = %+v", huge)
	}
	if items[0].Data == nil || items[2].Data == nil {
		t.Fatal("items around the oversize entry were not returned")
	}
}

func TestCursorsBelongToOneRunFamilyAndQuery(t *testing.T) {
	var bodies []string
	for i := range 5 {
		bodies = append(bodies, fmt.Sprint(i))
	}
	rep := recordedStage(bodies...)
	run := Run{ID: "run", End: EndDone, Report: rep}
	first, err := QueryRun(run, recordsQuery, Page{Items: 2, Bytes: 1 << 20})
	if err != nil || first.NextCursor == "" {
		t.Fatal(err)
	}
	after := recordsQuery
	after.Side = SideAfter
	for name, call := range map[string]func() error{
		"another run": func() error {
			_, err := QueryRun(Run{ID: "other", End: EndDone, Report: rep}, recordsQuery, Page{Cursor: first.NextCursor, Items: 2, Bytes: 1 << 20})
			return err
		},
		"another family": func() error {
			_, err := QueryRun(run, Query{Family: FamilyEvents}, Page{Cursor: first.NextCursor, Items: 2, Bytes: 1 << 20})
			return err
		},
		"other parameters": func() error {
			_, err := QueryRun(run, after, Page{Cursor: first.NextCursor, Items: 2, Bytes: 1 << 20})
			return err
		},
		"garbage": func() error {
			_, err := QueryRun(run, recordsQuery, Page{Cursor: "not-a-cursor!", Items: 2, Bytes: 1 << 20})
			return err
		},
		"past the end": func() error {
			_, err := QueryRun(run, recordsQuery, Page{Cursor: encodeCursor(cursor{V: 1, Run: "run", Family: FamilyRecords, Params: recordsQuery.digest(), Offset: 99}), Items: 2, Bytes: 1 << 20})
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%s: err = %v, want invalid query", name, err)
		}
	}
}

func TestQueriesValidateTheirFamilyParameters(t *testing.T) {
	run := Run{ID: "run", End: EndDone, Report: recordedStage("a")}
	for _, q := range []Query{
		{Family: "unknown"},
		{Family: FamilyRecords, Pipeline: "logs", Component: "transform"},
		{Family: FamilyRecords, Pipeline: "logs", Component: "transform", Side: "middle"},
		{Family: FamilyStageChanges, Pipeline: "logs"},
		{Family: FamilyStageChanges, Pipeline: "logs", Component: "transform", Side: SideBefore},
		{Family: FamilyJourney},
		{Family: FamilyJourney, Entity: "1:x", Component: "transform"},
		{Family: FamilyStatements},
		{Family: FamilyEvents, FromSeq: 5, ToSeq: 2},
		{Family: FamilyEvents, FromSeq: -1},
		{Family: FamilyEvents, Entity: "1:x"},
	} {
		if _, err := QueryRun(run, q, Page{Items: 10, Bytes: 1024}); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("%+v: err = %v, want invalid query", q, err)
		}
	}
	for _, q := range []Query{
		{Family: FamilyStageChanges, Pipeline: "logs", Component: "nope"},
		{Family: FamilyRecords, Pipeline: "traces", Component: "transform", Side: SideBefore},
		{Family: FamilyJourney, Entity: "9:resource[0]"},
		{Family: FamilyStatements, Component: "transform"},
	} {
		if _, err := QueryRun(run, q, Page{Items: 10, Bytes: 1024}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%+v: err = %v, want not found", q, err)
		}
	}
	if _, err := QueryRun(run, recordsQuery, Page{Items: 0, Bytes: 1024}); !errors.Is(err, ErrInvalidQuery) {
		t.Error("a zero page bound was accepted")
	}
	unreplayed := recordedStage("a")
	unreplayed.Stages[0].Kind, unreplayed.Stages[0].Type = "processor", "transform"
	res, err := QueryRun(Run{ID: "run", End: EndDone, Report: unreplayed}, Query{Family: FamilyStatements, Component: "transform"}, Page{Items: 10, Bytes: 1024})
	if err != nil || res.Total != 0 || !res.Complete {
		t.Fatalf("a transform stage without replays = %+v, %v", res, err)
	}
}

func TestIncompleteRunsSaySo(t *testing.T) {
	for _, tt := range []struct {
		name   string
		edit   func(*runresult.Result)
		family Query
		reason string
	}{
		{"partial", func(r *runresult.Result) { r.Status = runresult.StatusPartial }, recordsQuery, "partial"},
		{"cancelled", func(r *runresult.Result) { r.Status = runresult.StatusCancelled }, recordsQuery, "cancelled"},
		{"evidence budget", func(r *runresult.Result) {
			r.Status = runresult.StatusPartial
			r.Truncation = &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget, DroppedEvents: 3, DroppedBytes: 100}
		}, Query{Family: FamilyEvents}, runresult.TruncationEvidenceBudget},
		{"analysis omitted", func(r *runresult.Result) {
			r.Truncation = &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget, Omitted: []string{"stages"}}
		}, Query{Family: FamilyStageChanges, Pipeline: "logs", Component: "gone"}, ReasonAnalysisOmitted},
		{"events kept in full despite omitted analysis", func(r *runresult.Result) {
			r.Truncation = &runresult.Truncation{Reason: runresult.TruncationEvidenceBudget, Omitted: []string{"stages"}}
		}, Query{Family: FamilyEvents}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rep := recordedStage("a")
			tt.edit(rep.Result)
			res, err := QueryRun(Run{ID: "run", End: EndDone, Report: rep}, tt.family, Page{Items: 10, Bytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if res.TruncatedReason != tt.reason || res.Complete != (tt.reason == "") || res.NextCursor != "" {
				t.Fatalf("reason %q, complete %t", res.TruncatedReason, res.Complete)
			}
		})
	}
}

func TestStageChangesAreUncappedWithTheirEntities(t *testing.T) {
	var changes []analysis.Change
	for i := range 25 {
		changes = append(changes, analysis.Change{Entity: fmt.Sprintf("e%d", i), Path: fmt.Sprintf(`attributes["k%d"]`, i), Op: analysis.OpModified,
			Before: "x", After: "y", Confidence: analysis.ConfidenceExact})
	}
	changes = append(changes, analysis.Change{Entity: "e99", Path: changes[0].Path, Op: analysis.OpModified, Confidence: analysis.ConfidenceMedium})
	rep := recordedStage("a")
	rep.Stages[0].Inputs[0].Diff = &analysis.Diff{Changes: changes, Removed: []analysis.RecordRef{{Name: "gone"}}, Unresolved: []string{"merged"}}
	res, err := QueryRun(Run{ID: "run", End: EndDone, Report: rep}, Query{Family: FamilyStageChanges, Pipeline: "logs", Component: "transform"}, Page{Items: 100, Bytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 27 || res.Items[25].Ref != "removed:0" || res.Items[26].Ref != "unresolved:0" {
		t.Fatalf("total %d, refs %v", res.Total, res.Items)
	}
	var first ChangeItem
	if err := json.Unmarshal(res.Items[0].Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.Count != 2 || !slices.Equal(first.Entities, []string{"e0", "e99"}) || first.Confidence != analysis.ConfidenceMedium || first.Before != "x" {
		t.Fatalf("first group = %+v", first)
	}
}

var (
	sampleOnce sync.Once
	sampleRep  *engine.Report
	sampleErr  error
)

// sampleReport runs the logs-cleanup sample once.
func sampleReport(t *testing.T) *engine.Report {
	t.Helper()
	sampleOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		dir := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
		sampleRep, sampleErr = engine.Execute(ctx, engine.Request{Config: filepath.Join(dir, "config.yaml"), Fixture: filepath.Join(dir, "input.json")})
	})
	if sampleErr != nil {
		t.Fatal(sampleErr)
	}
	return sampleRep
}

func TestFamiliesProjectARecordedRun(t *testing.T) {
	rep := sampleReport(t)
	run := Run{ID: rep.Result.RunID, End: EndDone, Report: rep}
	query := func(q Query) *Result {
		t.Helper()
		res, err := QueryRun(run, q, Page{Items: 1000, Bytes: 1 << 20})
		if err != nil {
			t.Fatalf("%+v: %v", q, err)
		}
		if !res.Complete || res.NextCursor != "" {
			t.Fatalf("%+v: complete %t, reason %q", q, res.Complete, res.TruncatedReason)
		}
		return res
	}

	before := query(Query{Family: FamilyRecords, Pipeline: "logs", Component: "filter/health", Side: SideBefore})
	after := query(Query{Family: FamilyRecords, Pipeline: "logs", Component: "filter/health", Side: SideAfter})
	if before.Total != 4 || after.Total != 3 {
		t.Fatalf("filter/health records: %d before, %d after", before.Total, after.Total)
	}

	changes := query(Query{Family: FamilyStageChanges, Pipeline: "logs", Component: "attributes/scrub"})
	var paths []string
	for _, it := range changes.Items {
		if strings.HasPrefix(it.Ref, "change:") {
			var c ChangeItem
			if err := json.Unmarshal(it.Data, &c); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, string(c.Op)+" "+c.Path)
		}
	}
	if !slices.Contains(paths, `removed attributes["user.password"]`) {
		t.Fatalf("attributes/scrub changes = %v", paths)
	}

	statements := query(Query{Family: FamilyStatements, Component: "transform/severity"})
	if statements.Total < 2 || !strings.HasPrefix(statements.Items[0].Ref, "replay:") || !strings.HasPrefix(statements.Items[1].Ref, "group:") {
		t.Fatalf("statements = %+v", statements.Items)
	}
	var replay ReplayItem
	if err := json.Unmarshal(statements.Items[0].Data, &replay); err != nil || replay.Pipeline != "logs" || replay.Processor != "transform/severity" {
		t.Fatalf("replay = %+v (%v)", replay, err)
	}

	id := rep.Lineage[0].ID
	journey := query(Query{Family: FamilyJourney, Entity: id})
	var record JourneyRecord
	if err := json.Unmarshal(journey.Items[0].Data, &record); err != nil || journey.Items[0].Ref != "record" || record.ID != id || record.Hops != len(rep.Lineage[0].Hops) {
		t.Fatalf("journey = %+v (%v)", journey.Items, err)
	}
	if journey.Total != 1+len(rep.Lineage[0].Hops)+len(rep.Lineage[0].Deliveries)+len(rep.Lineage[0].Stops) {
		t.Fatalf("journey total = %d", journey.Total)
	}

	all := query(Query{Family: FamilyEvents})
	window := query(Query{Family: FamilyEvents, FromSeq: 2, ToSeq: 4})
	batch := query(Query{Family: FamilyEvents, Component: "batch"})
	if all.Total != len(rep.Result.Events) || window.Total != 3 || window.Items[0].Ref != "event:2" || batch.Total == 0 {
		t.Fatalf("events: all %d, window %d, batch %d", all.Total, window.Total, batch.Total)
	}
	var e instrument.Event
	if err := json.Unmarshal(batch.Items[0].Data, &e); err != nil || e.Component != "batch" {
		t.Fatalf("batch event = %+v (%v)", e, err)
	}
}

func TestRecommendationsFilterByPipelineAndComponent(t *testing.T) {
	rep := recordedStage("a")
	rep.Recommendations = []recommend.Recommendation{
		{Rule: "config.unused_component", Components: []string{"processor/attributes"}},
		{Rule: "filter.after_batch", Pipeline: "logs", Components: []string{"processor/batch", "processor/filter/health"}},
		{Rule: "sampling.late_spans", Pipeline: "traces", Components: []string{"processor/tail_sampling"}},
	}
	run := Run{ID: "run", End: EndDone, Report: rep}
	for _, c := range []struct {
		q    Query
		refs []string
	}{
		{Query{Family: FamilyRecommendations}, []string{"recommendation:0", "recommendation:1", "recommendation:2"}},
		{Query{Family: FamilyRecommendations, Pipeline: "logs"}, []string{"recommendation:0", "recommendation:1"}},
		{Query{Family: FamilyRecommendations, Component: "filter/health"}, []string{"recommendation:1"}},
		{Query{Family: FamilyRecommendations, Component: "filter"}, nil},
	} {
		refs, _ := pageAll(t, run, c.q, 10, 1<<20)
		if !slices.Equal(refs, c.refs) {
			t.Errorf("%+v: refs = %v, want %v", c.q, refs, c.refs)
		}
	}
	if _, err := QueryRun(run, Query{Family: FamilyRecommendations, Side: SideBefore}, Page{Items: 10, Bytes: 1 << 20}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("side on recommendations: %v", err)
	}
}
