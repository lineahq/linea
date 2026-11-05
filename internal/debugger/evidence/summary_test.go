package evidence

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

func TestRunSummaryAppliesCaps(t *testing.T) {
	long := strings.Repeat("a", 255) + "é" + "tail" // é straddles byte 256
	large := map[string]any{"k": strings.Repeat("v", 300)}
	var changes []analysis.Change
	for i := range 25 {
		changes = append(changes, analysis.Change{Entity: fmt.Sprintf("resource[0].scope[0].log[%d]", i),
			Path: fmt.Sprintf(`attributes["k%d"]`, i), Op: analysis.OpModified, Before: "x", After: "y", BeforeType: "Str", AfterType: "Str",
			Confidence: analysis.ConfidenceExact})
	}
	changes[1] = analysis.Change{Path: changes[0].Path, Op: analysis.OpModified, Before: long, After: large, BeforeType: "Str", AfterType: "Map",
		Confidence: analysis.ConfidenceMedium}
	inputs := []analysis.StageInput{{Outcome: analysis.OutcomeEmitted, Diff: &analysis.Diff{Changes: changes,
		Removed: []analysis.RecordRef{{Ref: "0"}}, Unresolved: []string{"resources merged"}}}}
	for i := range 7 {
		inputs = append(inputs, analysis.StageInput{Outcome: analysis.OutcomeNoOutput, Error: fmt.Sprintf("error %d", i)})
	}
	run := Run{End: EndDone, Report: &engine.Report{
		Result:     &runresult.Result{Status: runresult.StatusCompleted},
		Stages:     []analysis.Stage{{Component: "transform", Pipeline: "logs", Inputs: inputs}},
		Assertions: &assertion.Report{Outcome: assertion.Fail, Results: []assertion.Result{{Outcome: assertion.Pass}, {Outcome: assertion.Fail}}},
	}}
	s := Summarize(run)
	stage := s.Stages[0]
	if len(stage.Changes) != maxChangeGroups || !stage.ChangesTruncated || stage.Removed != 1 || stage.Unresolved != 1 ||
		!reflect.DeepEqual(stage.Outcomes, map[analysis.Outcome]int{analysis.OutcomeEmitted: 1, analysis.OutcomeNoOutput: 7}) {
		t.Fatalf("stage = %+v", stage)
	}
	first := stage.Changes[0]
	if first.Count != 2 || first.Confidence != analysis.ConfidenceMedium || first.Before.Value != "x" || first.Before.Truncated {
		t.Fatalf("first group = %+v (the first change's values, the lowest confidence)", first)
	}
	if s.Assertions == nil || *s.Assertions != (AssertionSummary{Outcome: assertion.Fail, Pass: 1, Fail: 1}) {
		t.Fatalf("assertions = %+v", s.Assertions)
	}
	if len(s.Errors) != maxErrors || !s.ErrorsTruncated || s.Errors[0] != (SummaryError{Source: "stage", Component: "transform", Pipeline: "logs", Message: "error 0"}) {
		t.Fatalf("errors = %+v", s.Errors)
	}

	if v := summarize(long, "Str"); v.Value != strings.Repeat("a", 255) || !v.Truncated {
		t.Fatalf("long string = %+v", v)
	}
	v := summarize(large, "Map")
	if text, ok := v.Value.(string); !ok || len(text) != maxSummaryBytes || !strings.HasPrefix(text, `{"k":"vvv`) || !v.Truncated || v.Type != "Map" {
		t.Fatalf("large map = %+v", v)
	}
	if v := summarize(map[string]any{"k": 1.0}, "Map"); !reflect.DeepEqual(v.Value, map[string]any{"k": 1.0}) || v.Truncated {
		t.Fatalf("small map = %+v", v)
	}
	if summarize(nil, "") != nil {
		t.Fatal("an absent value must stay absent")
	}
	message := strings.Repeat("m", 300)
	var e Summary
	e.addError(SummaryError{Source: "run", Message: message})
	if len(e.Errors[0].Message) != maxSummaryBytes || !e.Errors[0].Truncated {
		t.Fatalf("error message = %+v", e.Errors[0])
	}
}

// TestRunSummaryCountsStandIns: stand-in components and references are
// counted from the report's topology and references.
func TestRunSummaryCountsStandIns(t *testing.T) {
	run := Run{End: EndDone, Report: &engine.Report{
		Result: &runresult.Result{Status: runresult.StatusCompleted},
		Topology: &topology.Graph{Components: []topology.Component{
			{Ref: "processor/geoip", Mode: topology.ModeStandIn},
			{Ref: "exporter/datadog", Mode: topology.ModeStandIn},
			{Ref: "receiver/otlp", Mode: topology.ModeSimulated},
		}},
		References: []config.Reference{{Scheme: "env", Target: "A", Source: config.SourceStandIn}, {Scheme: "env", Target: "B", Source: config.SourceScenario}},
	}}
	if s := Summarize(run); s.StandIns != 2 || s.StandInReferences != 1 {
		t.Fatalf("summary counts %d stand-ins and %d stand-in references", s.StandIns, s.StandInReferences)
	}
}

// A buffering stage reports its reconciliation alone, so records are not
// counted once per input and again in the reconciliation.
func TestRunSummaryPrefersTheReconciliation(t *testing.T) {
	change := analysis.Change{Path: "body", Op: analysis.OpModified, Before: "a", After: "b"}
	stage := summarizeStage(analysis.Stage{
		Inputs:         []analysis.StageInput{{Outcome: analysis.OutcomeDeferred, Diff: &analysis.Diff{Changes: []analysis.Change{change}}}},
		Reconciliation: &analysis.Diff{Changes: []analysis.Change{change, change}},
	})
	if !stage.Reconciled || len(stage.Changes) != 1 || stage.Changes[0].Count != 2 {
		t.Fatalf("stage = %+v", stage)
	}
}

// What custom components sent to simulated endpoints and logged is
// counted; an entry at error level is a run error after stage errors.
func TestRunSummaryCountsEndpointOutputsAndComponentLogs(t *testing.T) {
	logged := func(level, msg string) instrument.Event {
		return instrument.Event{Kind: instrument.KindComponentLog, Component: "livecapture", ComponentKind: "extension",
			Log: &instrument.LogEntry{Level: level, Message: msg}}
	}
	run := Run{End: EndDone, Report: &engine.Report{Result: &runresult.Result{
		Status: runresult.StatusCompleted,
		EndpointOutputs: []runresult.EndpointOutput{{Component: "extension/livecapture", Endpoint: "delivery", Signal: "logs",
			Batches: 2, Counts: telemetry.Counts{Items: 5}}},
		Events: []instrument.Event{logged("info", "started"), logged("warn", "slow"), logged("error", "delivery failed")},
	}}}
	s := Summarize(run)
	if want := []EndpointOutputSummary{{Component: "extension/livecapture", Endpoint: "delivery", Signal: "logs", Batches: 2, Records: 5}}; !reflect.DeepEqual(s.EndpointOutputs, want) {
		t.Errorf("endpoint outputs %+v", s.EndpointOutputs)
	}
	if want := (LogCounts{Total: 3, Warnings: 1, Errors: 1}); s.ComponentLogs == nil || *s.ComponentLogs != want {
		t.Errorf("component logs %+v", s.ComponentLogs)
	}
	if len(s.Errors) != 1 || s.Errors[0] != (SummaryError{Source: "component", Component: "livecapture", Message: "delivery failed"}) {
		t.Errorf("errors %+v", s.Errors)
	}
	if s := Summarize(Run{End: EndDone, Report: &engine.Report{Result: &runresult.Result{Status: runresult.StatusCompleted}}}); s.ComponentLogs != nil || s.EndpointOutputs != nil {
		t.Errorf("a run without either summarizes %+v, %+v", s.ComponentLogs, s.EndpointOutputs)
	}
}
