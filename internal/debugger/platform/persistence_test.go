package platform

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// openPlatform starts a service on in-memory workspace data. Opening the same
// data again stands in for restarting on the same stored state.
func openPlatform(t *testing.T, data *memory.Data, opts Options) *Service {
	t.Helper()
	return openPlatformLimits(t, data, artifact.Limits{}, opts)
}

// openMemory starts a service on in-memory workspace data with default limits.
func openMemory(data *memory.Data, opts Options) (*Service, error) {
	return Open(memory.Open(data, artifact.Limits{}), opts)
}

// openKillable starts a service on data and returns a kill that ends its store
// handle without closing the service, as a killed process would: the service
// can write nothing more, and a service opened on the same data recovers what
// it left unfinished.
func openKillable(t *testing.T, data *memory.Data, opts Options) (*Service, func()) {
	t.Helper()
	repo := memory.Open(data, artifact.Limits{})
	s, err := Open(repo, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, repo.Close
}

func openPlatformLimits(t *testing.T, data *memory.Data, limits artifact.Limits, opts Options) *Service {
	t.Helper()
	s, err := Open(memory.Open(data, limits), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func encodedReport(t *testing.T, rep *engine.Report) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := engine.Encode(&b, rep); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestWorkspaceRetainsReportsModesOperationalEventsAndHistory(t *testing.T) {
	data := memory.NewData()
	s := openPlatform(t, data, Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		e := instrument.Event{ID: req.RunID + ":1", RunID: req.RunID, Seq: 1, Kind: instrument.KindRunStarted, Time: time.Now().UTC()}
		req.Observer(e)
		rep := report(req, runresult.StatusCompleted)
		// Normalized report IDs differ from live SSE IDs; preserve both.
		rep.Result.RunID = "normalized"
		rep.Result.Snapshots = instrument.Snapshots{"snapshot": []byte("{ \n \"resourceLogs\" : [] }")}
		rep.Stages = []analysis.Stage{{}}
		return rep, nil
	}})
	first := submit(t, s, request("quick"))
	original := encodedReport(t, waitRun(t, first).Report)
	second := submit(t, s, engine.Request{ConfigYAML: "config", ScenarioYAML: "scenario"})
	waitRun(t, second)
	if len(s.List()) != 2 {
		t.Fatal("ephemeral history limit evicted durable runs")
	}
	s.Close()
	s = openPlatform(t, data, Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		t.Error("recovery replayed terminal work")
		return nil, errors.New("unexpected replay")
	}})
	if len(s.List()) != 2 || s.List()[0].ID != second.ID() {
		t.Fatal("restart changed retained admission order")
	}
	recovered, err := s.Get(first.ID())
	if err != nil {
		t.Fatal(err)
	}
	v := waitRun(t, recovered)
	if v.Mode != ModeQuick || !bytes.Equal(encodedReport(t, v.Report), original) {
		t.Fatal("report changed on reopen")
	}
	if string(v.Report.Result.Snapshots["snapshot"]) != "{ \n \"resourceLogs\" : [] }" {
		t.Fatal("snapshot whitespace changed")
	}
	w := recovered.Since(EventCursor{})
	events := w.Events
	if !w.Terminal || len(events) != 1 || events[0].RunID != first.ID() || v.Report.Result.RunID != "normalized" {
		t.Fatal("operational event identity lost")
	}
	if w := recovered.Since(EventCursor{Attempt: w.Attempt, Seq: 1}); w.Restarted || len(w.Events) != 0 {
		t.Fatal("resume duplicated events")
	}
	if err := recovered.Cancel(); err != nil {
		t.Fatal(err)
	}
	if recovered.View(true).State != StateDone {
		t.Fatal("cancel changed recovered terminal state")
	}
	r, _ := s.Get(second.ID())
	if r.View(true).Mode != ModeScenario {
		t.Fatal("scenario mode lost")
	}
}

func TestReportRecoveryPreservesLargeAnalysisIntegers(t *testing.T) {
	data := memory.NewData()
	s := openPlatform(t, data, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Stages = []analysis.Stage{{Reconciliation: &analysis.Diff{Changes: []analysis.Change{{Before: int64(9223372036854775807), After: int64(-9223372036854775807)}}}}}
		return rep, nil
	}})
	r := submit(t, s, request("quick"))
	original := encodedReport(t, waitRun(t, r).Report)
	s.Close()
	s = openPlatform(t, data, Options{})
	r, _ = s.Get(r.ID())
	if !bytes.Equal(encodedReport(t, r.View(true).Report), original) {
		t.Fatal("analysis integers rounded on recovery")
	}
}

// failingAdmission is a repository that refuses to record runs.
type failingAdmission struct{ artifact.Repository }

func (failingAdmission) AdmitRun(context.Context, artifact.Admission) (*artifact.RunRecord, error) {
	return nil, artifact.ErrStorage
}

func (failingAdmission) SaveRun(context.Context, string, string, uint64, bool, artifact.RunIndex, []byte, []byte) (*artifact.RunRecord, error) {
	return nil, artifact.ErrStorage
}

func TestWorkspaceRejectsUnpersistedAdmissionAndOversizedEvidence(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		s, err := Open(failingAdmission{memory.New(artifact.Limits{})}, Options{Prepare: unprepared, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
			t.Error("unpersisted run executed")
			return nil, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if r, err := s.Submit(request("quick")); r != nil || !errors.Is(err, ErrPersistence) {
			t.Fatalf("admission = %v, %v", r, err)
		}
		if len(s.List()) != 0 {
			t.Fatal("failed admission retained")
		}
	})
	t.Run("evidence", func(t *testing.T) {
		data := memory.NewData()
		s := openPlatformLimits(t, data, artifact.Limits{RunBytes: 1}, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
			return report(req, runresult.StatusCompleted), nil
		}})
		r := submit(t, s, request("quick"))
		v := waitRun(t, r)
		if v.State != StateError || v.Report != nil || !errors.Is(r.Err(), ErrPersistence) {
			t.Fatalf("unpublished report claimed done: %+v", v)
		}
		s.Close()
		s = openPlatform(t, data, Options{})
		r, _ = s.Get(r.ID())
		if r.View(true).State != StateError || r.View(true).Interrupted || !errors.Is(r.Err(), ErrPersistence) {
			t.Fatal("durable publication failure lost")
		}
	})
}

func TestPersistentQueuedCancellationAndCooperativeShutdown(t *testing.T) {
	data := memory.NewData()
	started := make(chan struct{})
	s := openPlatform(t, data, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		close(started)
		<-ctx.Done()
		return report(req, runresult.StatusCancelled), nil
	}})
	active := submit(t, s, request("active"))
	receive(t, started)
	queued := submit(t, s, request("queued"))
	if err := queued.Cancel(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openPlatform(t, data, Options{})
	r, _ := s.Get(queued.ID())
	if v := waitRun(t, r); v.State != StateCancelled || v.Started != nil || v.Report != nil {
		t.Fatalf("queued = %+v", v)
	}
	r, _ = s.Get(active.ID())
	if v := waitRun(t, r); v.State != StateDone || v.Report.Result.Status != runresult.StatusCancelled {
		t.Fatalf("active = %+v", v)
	}
}

func TestOwnerCrashMarksQueuedAndRunningInterruptedWithoutReplay(t *testing.T) {
	data := memory.NewData()
	var started sync.Once
	startedCh := make(chan struct{})
	crashed, kill := openKillable(t, data, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started.Do(func() { close(startedCh) })
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	active, err := crashed.Submit(request("active"))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, startedCh)
	queued, err := crashed.Submit(engine.Request{ConfigYAML: "config", ScenarioYAML: "scenario"})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{active.ID(), queued.ID()}
	kill()
	s := openPlatform(t, data, Options{Prepare: unprepared, Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		t.Error("replayed unfinished work")
		return nil, nil
	}})
	for i, id := range ids {
		r, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		v := waitRun(t, r)
		if v.State != StateError || !v.Interrupted || v.Report != nil || !errors.Is(r.Err(), ErrInterrupted) || (v.Started != nil) != (i == 0) {
			t.Fatalf("recovered = %+v", v)
		}
	}
	s.Close()
	s = openPlatform(t, data, Options{})
	if len(s.List()) != 2 || !s.List()[0].Interrupted {
		t.Fatal("interruption was not durable")
	}
}
