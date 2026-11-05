package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

func TestSubmitQuickRunRecordsAssertionsAndSupportsEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := New(Options{})
	t.Cleanup(s.Close)
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	const config = "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	const text = "assertions: [{target: {exporter: otlp, signal: traces}, count: {equals: 3}}]\n"
	req := RunRequest{Mode: ModeQuick, Config: Source{Text: config}, Quick: &QuickInput{Fixture: Source{Text: string(fixture)}, Assertions: text}}
	run, err := s.SubmitRun(ctx, testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Assertions == nil || rep.Assertions.Outcome != assertion.Pass || rep.Provenance.Assertions == nil {
		t.Fatalf("quick assertion report = %+v", rep)
	}
	var before bytes.Buffer
	if err := Encode(&before, rep); err != nil {
		t.Fatal(err)
	}
	evaluated, err := s.EvaluateAssertions(ctx, testCaller, run.ID, AssertionsRequest{ScenarioYAML: "version: 1\ninputs: [{file: never-read.json}]\n" + text, Record: true})
	if err != nil || !reflect.DeepEqual(evaluated.Result, rep.Assertions) {
		t.Fatalf("evaluation = %+v, %v; recorded %+v", evaluated, err, rep.Assertions)
	}
	if !evaluated.Recorded || evaluated.ID == "" {
		t.Fatal("real-engine evaluation was not recorded")
	}
	restored, err := s.GetEvaluation(ctx, testCaller, evaluated.ID)
	if err != nil || !reflect.DeepEqual(restored.Result, rep.Assertions) {
		t.Fatal("real-engine evaluation changed on readback", err)
	}
	recorded, err := s.GetRunReport(ctx, testCaller, run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var after bytes.Buffer
	if err := Encode(&after, recorded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("re-evaluation changed the recorded run")
	}
	// Setup remains a separate source with the existing invalid_request code.
	req.Quick.Setup = text
	_, err = s.SubmitRun(ctx, testCaller, req)
	wantCode(t, err, CodeInvalidRequest)
	if !strings.Contains(err.Error(), "setup_yaml: assertions: a quick run has none; use a scenario to assert") {
		t.Fatalf("setup assertion refusal changed: %v", err)
	}
	runs, err := allRuns(ctx, s, testCaller, RunFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("invalid setup was admitted: %+v, %v", runs, err)
	}
}
