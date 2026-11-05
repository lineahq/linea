package platform

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestSummaryAndQueriesWaitForATerminalRun(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-release
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	r := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	receive(t, started)
	if _, err := s.GetRunSummary(ctx, testCaller, r.ID()); ErrorCode(err) != CodeConflict {
		t.Fatalf("summary of a running run: %v", err)
	}
	if _, err := s.QueryEvidence(ctx, testCaller, r.ID(), EvidenceQuery{Family: EvidenceEvents}, EvidencePage{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("query of a running run: %v", err)
	}
	close(release)
	waitRun(t, r)
	summary, err := s.GetRunSummary(ctx, testCaller, r.ID())
	if err != nil || !summary.Complete || summary.ExecutionStatus != runresult.StatusCompleted {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	if !reflect.DeepEqual(summary, r.View(false).Summary()) {
		t.Fatal("GetRunSummary differs from the run's summary")
	}
	if _, err := s.GetRunSummary(ctx, testCaller, "missing"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown run: %v", err)
	}
}

func TestQueriesWithoutAReportAreConflicts(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		return nil, os.ErrNotExist
	}})
	t.Cleanup(s.Close)
	r := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	waitRun(t, r)
	if _, err := s.QueryEvidence(context.Background(), testCaller, r.ID(), EvidenceQuery{Family: EvidenceEvents}, EvidencePage{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("query of a failed run: %v", err)
	}
	summary, err := s.GetRunSummary(context.Background(), testCaller, r.ID())
	if err != nil || summary.Complete || summary.Incomplete != "error" {
		t.Fatalf("summary of a failed run = %+v, %v", summary, err)
	}
}

func TestEvidencePagesFollowTheBudgets(t *testing.T) {
	s := New(Options{Prepare: unprepared, Budgets: Budgets{PageItems: 3, PageBytes: 4096}, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Result.Events = make([]instrument.Event, 7)
		for i := range rep.Result.Events {
			rep.Result.Events[i] = instrument.Event{Seq: i + 1, Kind: instrument.KindIngress, Component: "batch"}
		}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	r := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	waitRun(t, r)
	q := EvidenceQuery{Family: EvidenceEvents}

	def, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{})
	if err != nil || len(def.Items) != 3 || def.Page.Items != 3 || def.Page.Bytes != 4096 || def.Page.Clamped != nil || def.NextCursor == "" {
		t.Fatalf("default page = %+v, %v", def, err)
	}
	big, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{Items: 50, Bytes: 1 << 20})
	want := []engine.Clamp{{Limit: "page_items", Requested: "50", Effective: "3"}, {Limit: "page_bytes", Requested: "1048576", Effective: "4096"}}
	if err != nil || len(big.Items) != 3 || !reflect.DeepEqual(big.Page.Clamped, want) {
		t.Fatalf("clamped page = %+v, %v", big, err)
	}
	small, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{Items: 2})
	if err != nil || len(small.Items) != 2 || small.Page.Clamped != nil {
		t.Fatalf("smaller page = %+v, %v", small, err)
	}
	next, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{Cursor: def.NextCursor})
	if err != nil || next.Items[0].Ref != "event:4" {
		t.Fatalf("next page = %+v, %v", next, err)
	}

	for name, call := range map[string]func() error{
		"negative page": func() error {
			_, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{Items: -1})
			return err
		},
		"bad cursor": func() error {
			_, err := s.QueryEvidence(ctx, testCaller, r.ID(), EvidenceQuery{Family: EvidenceEvents, Component: "batch"}, EvidencePage{Cursor: def.NextCursor})
			return err
		},
		"bad family": func() error {
			_, err := s.QueryEvidence(ctx, testCaller, r.ID(), EvidenceQuery{Family: "nope"}, EvidencePage{})
			return err
		},
	} {
		if code := ErrorCode(call()); code != CodeInvalidRequest {
			t.Errorf("%s: code %s, want invalid_request", name, code)
		}
	}
	if _, err := s.QueryEvidence(ctx, testCaller, r.ID(), EvidenceQuery{Family: EvidenceJourney, Entity: "1:x"}, EvidencePage{}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown entity: %v", err)
	}
}

// TestWorkspaceEvidenceQueriesSurviveRestartInBothModes: queries read the
// recorded snapshots, the same before and after a restart, for quick and
// scenario runs.
func TestWorkspaceEvidenceQueriesSurviveRestartInBothModes(t *testing.T) {
	sample := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario := "version: 1\ninputs:\n  - data: '" + strings.ReplaceAll(strings.TrimSpace(string(input)), "\n", " ") + "'\n"
	data := memory.NewData()
	s := openPlatform(t, data, Options{})
	ctx := context.Background()
	q := EvidenceQuery{Family: EvidenceRecords, Pipeline: "logs", Component: "filter/health", Side: "before"}
	results := map[string]*EvidenceResult{}
	for _, req := range []engine.Request{
		{ConfigYAML: string(config), FixtureJSON: string(input)},
		{ConfigYAML: string(config), ScenarioYAML: scenario},
	} {
		r := submit(t, s, req)
		if got := waitRun(t, r); got.State != StateDone {
			t.Fatalf("run = %+v", got)
		}
		res, err := s.QueryEvidence(ctx, testCaller, r.ID(), q, EvidencePage{})
		if err != nil || res.Total != 4 || !res.Complete {
			t.Fatalf("records = %+v, %v", res, err)
		}
		results[r.ID()] = res
	}
	s.Close()
	s = openPlatform(t, data, Options{})
	for id, want := range results {
		got, err := s.QueryEvidence(ctx, testCaller, id, q, EvidencePage{})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("after restart: %+v, %v", got, err)
		}
	}
}
