package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// bulkyExecutor completes runs whose fixture is "big" with 2 MiB of random hex
// evidence, so their archive stays large compressed.
func bulkyExecutor(_ context.Context, req engine.Request) (*engine.Report, error) {
	rep := suiteReport(req, assertion.Pass)
	if req.FixtureJSON == "big" {
		random := make([]byte, 1<<20)
		if _, err := rand.Read(random); err != nil {
			return nil, err
		}
		rep.Stages = []analysis.Stage{{Component: hex.EncodeToString(random)}}
	}
	return rep, nil
}

// savedSources is the saved sources and their revision chains, encoded.
func savedSources(t *testing.T, s *Service) string {
	t.Helper()
	page, err := s.ListArtifacts(context.Background(), testCaller, ListArtifactsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func wantDeleted(t *testing.T, err error, message string) {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != CodeNotFound || pe.Message != message {
		t.Fatalf("error = %v, want not_found %q", err, message)
	}
}

// TestDeletingARunFreesStorageForRefusedWork checks that a workspace
// limit that refuses a new run admits it once a finished run is deleted.
func TestDeletingARunFreesStorageForRefusedWork(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	limits := artifact.Limits{RunBytes: 4 << 20, CatalogBytes: 1 << 20}
	opts := Options{Prepare: unprepared, Execute: bulkyExecutor}
	s := openPlatformLimits(t, data, limits, opts)
	kept := waitRun(t, submit(t, s, request("small")))
	big := waitRun(t, submit(t, s, request("big")))
	usage, err := s.GetStorageUsage(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Half a MiB short of one admission's reserve; the big run holds more.
	limits.StorageBytes = usage.Used + limits.RunBytes + 2*limits.CatalogBytes - 512<<10
	s = openPlatformLimits(t, data, limits, opts)
	if _, err := s.SubmitRun(ctx, testCaller, quickRun("refused")); ErrorCode(err) != CodeResourceExhausted {
		t.Fatalf("run before deletion = %v, want resource_exhausted", err)
	}
	if err := s.DeleteRun(ctx, testCaller, big.ID); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetStorageUsage(ctx, testCaller)
	if err != nil || after.Used >= usage.Used-(1<<20) {
		t.Fatalf("usage after deletion = %+v, %v; before %d", after, err, usage.Used)
	}
	if h, err := s.Get(kept.ID); err != nil || h.View(false).State != StateDone {
		t.Fatalf("kept run = %v", err)
	}
	admitted, err := s.SubmitRun(ctx, testCaller, quickRun("admitted"))
	if err != nil {
		t.Fatalf("run after deletion = %v", err)
	}
	if _, err := s.GetRunReport(ctx, testCaller, admitted.ID, true); err != nil {
		t.Fatal(err)
	}
}

// TestDeletedRunReadsAreNotFoundAndOthersStay: every read naming a deleted
// run says it was deleted, also after reopening, while other runs, their
// evaluations, suites and saved sources are unchanged.
func TestDeletedRunReadsAreNotFoundAndOthersStay(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	opts := evaluationOptions()
	s := openPlatform(t, data, opts)
	info, err := s.GetWorkspaceInfo(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Setup, Content: []byte("setup")}); err != nil {
		t.Fatal(err)
	}
	kept := evaluationRun(t, s, ModeQuick)
	keptEval := recordEvaluation(t, s, kept)
	gone := evaluationRun(t, s, ModeScenario)
	goneEval := recordEvaluation(t, s, gone)
	suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("member"))
	if err != nil {
		t.Fatal(err)
	}
	suiteBefore := waitSuite(t, s, suite)
	keptBefore, _ := s.GetRun(ctx, testCaller, kept)
	sources := savedSources(t, s)

	if err := s.DeleteRun(ctx, testCaller, gone); err != nil {
		t.Fatal(err)
	}
	for reopened := range 2 {
		if reopened == 1 {
			s.Close()
			s = openPlatform(t, data, opts)
		}
		_, err := s.GetRun(ctx, testCaller, gone)
		wantDeleted(t, err, "run was deleted")
		_, err = s.GetRunReport(ctx, testCaller, gone, false)
		wantDeleted(t, err, "run was deleted")
		_, err = s.GetRunInputs(ctx, testCaller, gone)
		wantDeleted(t, err, "run was deleted")
		_, err = s.ListEvaluations(ctx, testCaller, gone)
		wantDeleted(t, err, "run was deleted")
		_, err = s.EvaluateAssertions(ctx, testCaller, gone, AssertionsRequest{ScenarioYAML: evaluationYAML})
		wantDeleted(t, err, "run was deleted")
		wantDeleted(t, s.DeleteRun(ctx, testCaller, gone), "run was deleted")
		if _, err := s.GetEvaluation(ctx, testCaller, goneEval.ID); ErrorCode(err) != CodeNotFound {
			t.Fatalf("deleted run's evaluation = %v, want not_found", err)
		}
		if _, err := s.GetRun(ctx, testCaller, "run-0000000000000000"); ErrorCode(err) != CodeNotFound || strings.Contains(err.Error(), "deleted") {
			t.Fatalf("unknown run = %v", err)
		}
		runs, _ := allRuns(ctx, s, testCaller, RunFilter{})
		for _, r := range runs {
			if r.ID == gone {
				t.Fatal("deleted run is listed")
			}
		}
		if got, _ := s.GetRun(ctx, testCaller, kept); !reflect.DeepEqual(evaluationJSON(t, got), evaluationJSON(t, keptBefore)) {
			t.Fatal("deletion changed another run")
		}
		if got, err := s.GetEvaluation(ctx, testCaller, keptEval.ID); err != nil || got.EvaluationInfo != keptEval.EvaluationInfo {
			t.Fatalf("other evaluation = %v", err)
		}
		if got, err := s.GetSuite(ctx, testCaller, suite.ID); err != nil || !reflect.DeepEqual(evaluationJSON(t, got), evaluationJSON(t, suiteBefore)) {
			t.Fatalf("suite changed: %v", err)
		}
		if savedSources(t, s) != sources {
			t.Fatal("deletion changed saved sources or revision origins")
		}
	}
}

// TestSuiteMembersAreDeletedWithTheirSuite: a member is refused alone and
// removed with its suite, whose reads then say it was deleted.
func TestSuiteMembersAreDeletedWithTheirSuite(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "workspace"}[durable], func(t *testing.T) {
			ctx := context.Background()
			opts := Options{Prepare: unprepared, Execute: bulkyExecutor}
			var s *Service
			data := memory.NewData()
			if durable {
				s = openPlatform(t, data, opts)
			} else {
				s = New(opts)
				t.Cleanup(s.Close)
			}
			other := waitRun(t, submit(t, s, request("other")))
			suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("one", "two"))
			if err != nil {
				t.Fatal(err)
			}
			waitSuite(t, s, suite)
			member := suite.Members[0].RunID
			err = s.DeleteRun(ctx, testCaller, member)
			if ErrorCode(err) != CodeConflict || !strings.Contains(err.Error(), suite.ID) {
				t.Fatalf("member deletion = %v, want conflict naming %s", err, suite.ID)
			}
			if err := s.DeleteSuite(ctx, testCaller, suite.ID); err != nil {
				t.Fatal(err)
			}
			if durable {
				s.Close()
				s = openPlatform(t, data, opts)
			}
			_, err = s.GetSuite(ctx, testCaller, suite.ID)
			wantDeleted(t, err, "suite was deleted")
			wantDeleted(t, s.DeleteSuite(ctx, testCaller, suite.ID), "suite was deleted")
			if _, err := s.GetSuite(ctx, testCaller, "suite-0000000000000000"); ErrorCode(err) != CodeNotFound || strings.Contains(err.Error(), "deleted") {
				t.Fatalf("unknown suite = %v", err)
			}
			for _, m := range suite.Members {
				_, err := s.GetRun(ctx, testCaller, m.RunID)
				wantDeleted(t, err, "run was deleted")
			}
			if list, _ := s.ListSuites(ctx, testCaller); len(list) != 0 {
				t.Fatalf("suites = %+v", list)
			}
			if runs, _ := allRuns(ctx, s, testCaller, RunFilter{}); len(runs) != 1 || runs[0].ID != other.ID {
				t.Fatalf("runs = %+v", runs)
			}
		})
	}
}

// TestUnfinishedRunsAreNotDeleted: a queued or running run, or a suite with
// one, is a conflict and nothing changes.
func TestUnfinishedRunsAreNotDeleted(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "ephemeral", true: "workspace"}[durable], func(t *testing.T) {
			ctx := context.Background()
			started, release := make(chan struct{}, 4), make(chan struct{})
			opts := Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				started <- struct{}{}
				<-release
				return suiteReport(req, assertion.Pass), nil
			}}
			data := memory.NewData()
			var s *Service
			if durable {
				s = openPlatform(t, data, opts)
			} else {
				s = New(opts)
				t.Cleanup(s.Close)
			}
			running := submit(t, s, request("running"))
			receive(t, started)
			queued := submit(t, s, request("queued"))
			suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("member"))
			if err != nil {
				t.Fatal(err)
			}
			var before *StorageUsage
			if durable {
				before, _ = s.GetStorageUsage(ctx, testCaller)
			}
			for _, id := range []string{running.ID(), queued.ID()} {
				if err := s.DeleteRun(ctx, testCaller, id); ErrorCode(err) != CodeConflict {
					t.Fatalf("deleting unfinished %s = %v, want conflict", id, err)
				}
			}
			if err := s.DeleteSuite(ctx, testCaller, suite.ID); ErrorCode(err) != CodeConflict {
				t.Fatalf("deleting a suite with an unfinished member = %v, want conflict", err)
			}
			if after, _ := s.GetStorageUsage(ctx, testCaller); durable && !reflect.DeepEqual(before, after) {
				t.Fatal("refused deletion changed the workspace")
			}
			if _, err := s.GetSuite(ctx, testCaller, suite.ID); err != nil {
				t.Fatal(err)
			}
			close(release)
			waitRun(t, running)
			waitRun(t, queued)
			waitSuite(t, s, suite)
			if err := s.DeleteRun(ctx, testCaller, queued.ID()); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteSuite(ctx, testCaller, suite.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestDeletionWorksOnAWorkspaceOverItsLimit: a workspace reopened below what
// it stores, and a run limit below its stored run, refuses new work but
// still reads and deletes.
func TestDeletionWorksOnAWorkspaceOverItsLimit(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	opts := Options{Prepare: unprepared, Execute: bulkyExecutor}
	s := openPlatformLimits(t, data, artifact.Limits{RunBytes: 4 << 20}, opts)
	big := waitRun(t, submit(t, s, request("big")))
	s.Close()
	s = openPlatformLimits(t, data, artifact.Limits{StorageBytes: 1 << 20, RunBytes: 1 << 20, CatalogBytes: 256 << 10}, opts)
	if u, err := s.GetStorageUsage(ctx, testCaller); err != nil || u.Used <= u.Limit {
		t.Fatalf("usage = %+v, %v; want over the limit", u, err)
	}
	if rep, err := s.GetRunReport(ctx, testCaller, big.ID, false); err != nil || len(rep.Stages) != 1 {
		t.Fatalf("stored run under a lower run limit = %v", err)
	}
	if err := s.DeleteRun(ctx, testCaller, big.ID); err != nil {
		t.Fatal(err)
	}
	if u, err := s.GetStorageUsage(ctx, testCaller); err != nil || u.Used > u.Limit {
		t.Fatalf("usage after deletion = %+v, %v", u, err)
	}
}

func TestDeletionRequiresAnOpenService(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: completedExecutor})
	done := waitRun(t, submit(t, s, request("done")))
	s.Close()
	if err := s.DeleteRun(context.Background(), testCaller, done.ID); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("delete after close = %v, want unavailable", err)
	}
	if err := s.DeleteSuite(context.Background(), testCaller, "suite-0000000000000000"); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("suite delete after close = %v, want unavailable", err)
	}
}
