package platform

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
)

func TestSuiteMembersAndIndividualRunsShareFIFOExecution(t *testing.T) {
	started, release := make(chan engine.Request, 4), make(chan struct{}, 4)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
		}
		rep := report(req, runresult.StatusCompleted)
		if req.Scenario != "" {
			rep.Scenario = req.Scenario
			rep.Assertions = &assertion.Report{Outcome: assertion.Pass}
		}
		return rep, nil
	}})
	t.Cleanup(s.Close)
	suite, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("first", "second"))
	if err != nil {
		t.Fatal(err)
	}
	first := receive(t, started)
	if first.Scenario != "first" {
		t.Fatalf("first suite member = %+v", first)
	}
	quick := submit(t, s, request("quick"))
	release <- struct{}{}
	second := receive(t, started)
	if second.Scenario != "second" {
		t.Fatalf("second suite member = %+v", second)
	}
	release <- struct{}{}
	if got := receive(t, started); got.RunID != quick.ID() {
		t.Fatalf("queue order changed: %+v", got)
	}
	release <- struct{}{}
	for i, id := range []string{first.RunID, second.RunID} {
		if suite.Members[i].RunID != id {
			t.Fatal("suite result lost its shared run identity")
		}
		r, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		waitRun(t, r)
		if err != nil || r.View(true).Report.Assertions == nil || r.View(false).Mode != ModeScenario {
			t.Fatalf("suite evidence unavailable: %v", err)
		}
	}
	waitRun(t, quick)
	got, err := s.GetSuite(context.Background(), testCaller, suite.ID)
	if err != nil || !got.Done || got.Outcome != assertion.Pass {
		t.Fatalf("suite = %+v, %v", got, err)
	}
}

func TestSuiteWithoutAssertionsDoesNotPass(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	rep, err := s.RunSuite(context.Background(), testCaller, SuiteRequest{Config: "config", Scenarios: []string{"no-assertions"}})
	if err != nil || rep.Outcome != assertion.Error || rep.Scenarios[0].Error == "" {
		t.Fatalf("scenario without assertions falsely passed: %+v, %v", rep, err)
	}
	if len(s.List()) != 1 {
		t.Fatal("scenario evidence was not retained")
	}
}
