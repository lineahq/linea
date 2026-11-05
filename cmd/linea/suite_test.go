package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

func TestSuiteQueuedCancellationRendersTextAndJUnit(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var service *platform.Service
	service = platform.New(platform.Options{
		Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
			if req.Scenario == "last" {
				<-started // First is running; the second member was admitted already.
				page, err := service.ListRuns(ctx, cliCaller, platform.RunQuery{})
				if err != nil {
					return req, err
				}
				for _, run := range page.Runs {
					if run.State == platform.StateQueued {
						if _, err := service.CancelRun(ctx, cliCaller, run.ID); err != nil {
							return req, err
						}
					}
				}
				close(release)
			}
			return req, nil
		},
		Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
			if req.Scenario == "queued" {
				t.Error("cancelled queued member executed")
			}
			if req.Scenario == "first" {
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			return &engine.Report{Schema: engine.ReportSchemaVersion, Scenario: req.Scenario, Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}, Assertions: &assertion.Report{Outcome: assertion.Pass, RunStatus: runresult.StatusCompleted}}, nil
		},
	})
	t.Cleanup(service.Close)
	rep, err := service.RunSuite(context.Background(), cliCaller, platform.SuiteRequest{Config: "config", Scenarios: []string{"first", "queued", "last"}})
	if err != nil {
		t.Fatal(err)
	}
	if outcomeCode(rep.Outcome) != 1 || rep.Scenarios[1].Error != "run cancelled" || rep.Scenarios[1].Assertions != nil {
		t.Fatalf("cancelled member = %+v", rep)
	}
	var text, junit bytes.Buffer
	if err := writeTestText(&text, rep); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJUnit(&junit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "run cancelled") || !strings.Contains(junit.String(), `message="run cancelled"`) || !strings.Contains(junit.String(), `type="error"`) {
		t.Fatalf("text:\n%s\nJUnit:\n%s", &text, &junit)
	}
}

func TestSuiteAssertionErrorRetainsTextAndJUnitCases(t *testing.T) {
	path := testScenario(t, "{target: {exporter: kafka, signal: logs}, count: {min: 1}}")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"test", "--config", multisignalDir + "/config.yaml", "--scenario", path, "--format", "json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d: %s", code, &stderr)
	}
	var rep platform.SuiteReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	member := rep.Scenarios[0]
	if member.Outcome != assertion.Error || member.Error != "" || member.Assertions == nil || len(member.Assertions.Results) == 0 {
		t.Fatal("assertion error became a setup failure", member)
	}
	var text, junit bytes.Buffer
	if err := writeTestText(&text, &rep); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteJUnit(&junit); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "error target: no logs pipeline exports to kafka") {
		t.Fatal("assertion details lost", &text)
	}
	var suites engine.JUnitSuites
	if err := xml.Unmarshal(junit.Bytes(), &suites); err != nil {
		t.Fatal(err)
	}
	if len(suites.Suites) != 1 || len(suites.Suites[0].Cases) != len(member.Assertions.Results) || suites.Suites[0].Cases[0].Name == "setup" || suites.Suites[0].Cases[0].Error == nil || !strings.Contains(suites.Suites[0].Cases[0].Error.Text, "no logs pipeline exports to kafka") {
		t.Fatal("JUnit assertion cases lost", &junit)
	}
}
