package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"github.com/lineahq/linea/internal/debugger/topology"
)

const evaluationYAML = "version: 1\ninputs: [{file: never-read.json}]\nassertions: [{target: {exporter: otlp, signal: logs}, count: {equals: 0}}]\n# private-assertion-source\r\n"

func evaluationOptions() Options {
	return Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		rep := report(req, runresult.StatusCompleted)
		rep.Topology = &topology.Graph{
			Pipelines:  []topology.Pipeline{{ID: "logs", Signal: "logs", Exporters: []string{"exporter/otlp"}}},
			Components: []topology.Component{{Ref: "exporter/otlp", ID: "otlp", Kind: topology.KindExporter, Mode: topology.ModeCaptured}},
		}
		return rep, nil
	}}
}

func evaluationRun(t *testing.T, s *Service, mode InputMode) string {
	t.Helper()
	req := quickRun("input")
	if mode == ModeScenario {
		req = RunRequest{Mode: ModeScenario, Config: Source{Text: "config"}, Scenario: &ScenarioInput{Scenario: Source{Text: "scenario"}}}
	}
	run, err := s.SubmitRun(context.Background(), testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRunReport(context.Background(), testCaller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func evaluationJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// repositorySnapshot is everything the service's repository stores: its usage
// and every run, evidence archive, evaluation and result. A service without a
// repository has nothing stored.
func repositorySnapshot(t *testing.T, s *Service) map[string]string {
	t.Helper()
	out := map[string]string{}
	if s.repository == nil {
		return out
	}
	ctx := context.Background()
	usage, err := s.repository.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out["usage"] = fmt.Sprint(usage)
	runs, err := s.repository.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		out["run/"+r.ID] = string(evaluationJSON(t, r))
		evidence, _ := s.repository.RunEvidence(ctx, r.ID)
		out["evidence/"+r.ID] = string(evidence)
		evaluations, err := s.repository.Evaluations(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evaluations {
			_, result, err := s.repository.EvaluationResult(ctx, e.ID)
			if err != nil {
				t.Fatal(err)
			}
			out["evaluation/"+e.ID] = string(evaluationJSON(t, e))
			out["result/"+e.ID] = string(result)
		}
	}
	return out
}

func recordEvaluation(t *testing.T, s *Service, id string) *Evaluation {
	t.Helper()
	e, err := s.EvaluateAssertions(context.Background(), Local("mcp"), id, AssertionsRequest{ScenarioYAML: evaluationYAML, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Recorded || !regexp.MustCompile(`^eval-[0-9a-f]{16}$`).MatchString(e.ID) || e.RunID != id || e.Caller != Local("mcp").Actor() || e.Created.IsZero() || e.Outcome != assertion.Pass || e.Result.Outcome != e.Outcome {
		t.Fatalf("evaluation = %+v", e)
	}
	return e
}

func TestRecordedEvaluationsPreserveRunsAndRestoreResultsInBothModes(t *testing.T) {
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			data := memory.NewData()
			opts := evaluationOptions()
			s := openPlatform(t, data, opts)
			ctx := context.Background()
			id := evaluationRun(t, s, mode)
			before, _ := s.GetRun(ctx, testCaller, id)
			beforeView := evaluationJSON(t, before)
			beforeReport, _ := s.GetRunReport(ctx, testCaller, id, false)
			beforeReportBytes := encodedReport(t, beforeReport)
			beforeRecords, _ := s.repository.Runs(ctx)
			beforeRecordsBytes := evaluationJSON(t, beforeRecords)
			beforeEvidence, _ := s.repository.RunEvidence(ctx, id)
			e := recordEvaluation(t, s, id)
			second := recordEvaluation(t, s, id)
			wantResult := evaluationJSON(t, e.Result)
			after, _ := s.GetRun(ctx, testCaller, id)
			afterReport, _ := s.GetRunReport(ctx, testCaller, id, false)
			afterRecords, _ := s.repository.Runs(ctx)
			afterEvidence, _ := s.repository.RunEvidence(ctx, id)
			if !bytes.Equal(beforeView, evaluationJSON(t, after)) || !bytes.Equal(beforeReportBytes, encodedReport(t, afterReport)) || !bytes.Equal(beforeRecordsBytes, evaluationJSON(t, afterRecords)) || !bytes.Equal(beforeEvidence, afterEvidence) {
				t.Fatal("recording changed the run view, report, record or evidence")
			}
			got, err := s.GetEvaluation(ctx, testCaller, e.ID)
			if err != nil || !bytes.Equal(wantResult, evaluationJSON(t, got.Result)) || got.EvaluationInfo != e.EvaluationInfo {
				t.Fatal("readback changed result", err)
			}
			s.Close()
			s = openPlatform(t, data, opts)
			got, err = s.GetEvaluation(ctx, testCaller, e.ID)
			if err != nil || !got.Recorded || got.EvaluationInfo != e.EvaluationInfo || !bytes.Equal(wantResult, evaluationJSON(t, got.Result)) {
				t.Fatal("restart changed evaluation", err)
			}
			list, err := s.ListEvaluations(ctx, testCaller, id)
			if err != nil || !reflect.DeepEqual(list, []EvaluationInfo{e.EvaluationInfo, second.EvaluationInfo}) {
				t.Fatalf("list = %+v, %v", list, err)
			}
			if strings.Contains(string(evaluationJSON(t, list)), "private-assertion-source") || strings.Contains(string(evaluationJSON(t, got)), "private-assertion-source") {
				t.Fatal("source exposed in public evaluation")
			}
		})
	}
}

func TestUnrecordedEvaluationsWriteNothingInEitherComposition(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		for _, mode := range []InputMode{ModeQuick, ModeScenario} {
			t.Run(fmt.Sprintf("workspace=%t/%s", workspace, mode), func(t *testing.T) {
				data := memory.NewData()
				s := New(evaluationOptions())
				if workspace {
					s.Close()
					s = openPlatform(t, data, evaluationOptions())
				}
				t.Cleanup(s.Close)
				id := evaluationRun(t, s, mode)
				before := repositorySnapshot(t, s)
				e, err := s.EvaluateAssertions(context.Background(), testCaller, id, AssertionsRequest{ScenarioYAML: evaluationYAML})
				if err != nil || e.ID != "" || e.Recorded || e.Created.IsZero() || e.RunID != id || e.Caller != testCaller.Actor() || e.Result.Outcome != assertion.Pass {
					t.Fatalf("evaluation = %+v, %v", e, err)
				}
				after := repositorySnapshot(t, s)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("unrecorded evaluation wrote files")
				}
				list, err := s.ListEvaluations(context.Background(), testCaller, id)
				if err != nil || len(list) != 0 {
					t.Fatal("unrecorded evaluation retained", err)
				}
				r, _ := s.handle(context.Background(), id)
				if len(r.evaluations) != 0 {
					t.Fatal("unrecorded source retained on handle")
				}
			})
		}
	}
}

func TestEvaluationCapRetainsTheFirstHundredInEitherComposition(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(fmt.Sprint(workspace), func(t *testing.T) {
			data := memory.NewData()
			s := New(evaluationOptions())
			if workspace {
				s.Close()
				s = openPlatform(t, data, evaluationOptions())
			}
			t.Cleanup(s.Close)
			id := evaluationRun(t, s, ModeQuick)
			ctx := context.Background()
			want := []EvaluationInfo{}
			for i := 0; i < 100; i++ {
				want = append(want, recordEvaluation(t, s, id).EvaluationInfo)
			}
			before := repositorySnapshot(t, s)
			_, err := s.EvaluateAssertions(ctx, testCaller, id, AssertionsRequest{ScenarioYAML: evaluationYAML, Record: true})
			wantCode(t, err, CodeResourceExhausted)
			after := repositorySnapshot(t, s)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("101st evaluation wrote files")
			}
			got, err := s.ListEvaluations(ctx, testCaller, id)
			if err != nil || !reflect.DeepEqual(want, got) {
				t.Fatal("cap evicted evaluations", err)
			}
			for _, info := range want {
				if _, err := s.GetEvaluation(ctx, testCaller, info.ID); err != nil {
					t.Fatal("retained evaluation missing", err)
				}
			}
		})
	}
}

func TestEvaluationStorageQuotaIsResourceExhaustedAndWritesNothing(t *testing.T) {
	data := memory.NewData()
	opts := evaluationOptions()
	s := openPlatformLimits(t, data, artifact.Limits{StorageBytes: 128 << 10, CatalogBytes: 16 << 10, RunBytes: 10 << 10, BlobBytes: 256 << 10}, opts)
	id := evaluationRun(t, s, ModeQuick)
	first := recordEvaluation(t, s, id)
	before := repositorySnapshot(t, s)
	_, err := s.EvaluateAssertions(context.Background(), testCaller, id, AssertionsRequest{ScenarioYAML: evaluationYAML + "# " + strings.Repeat("private", 30000), Record: true})
	wantCode(t, err, CodeResourceExhausted)
	after := repositorySnapshot(t, s)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("quota failure wrote journal or blobs")
	}
	list, err := s.ListEvaluations(context.Background(), testCaller, id)
	if err != nil || !reflect.DeepEqual(list, []EvaluationInfo{first.EvaluationInfo}) {
		t.Fatal("quota failure changed listing", err)
	}
}

func TestEphemeralEvaluationsDisappearWithTheirEvictedRun(t *testing.T) {
	opts := evaluationOptions()
	opts.HistoryLimit = 1
	s := New(opts)
	t.Cleanup(s.Close)
	id := evaluationRun(t, s, ModeQuick)
	e := recordEvaluation(t, s, id)
	r, _ := s.handle(context.Background(), id)
	if len(r.evaluations) != 1 || string(r.evaluations[0].source) != evaluationYAML {
		t.Fatal("private source not retained")
	}
	got, err := s.GetEvaluation(context.Background(), testCaller, e.ID)
	if err != nil || !bytes.Equal(evaluationJSON(t, got.Result), evaluationJSON(t, e.Result)) {
		t.Fatal(err)
	}
	evaluationRun(t, s, ModeScenario)
	_, err = s.GetEvaluation(context.Background(), testCaller, e.ID)
	wantCode(t, err, CodeNotFound)
	_, err = s.ListEvaluations(context.Background(), testCaller, id)
	wantCode(t, err, CodeNotFound)
}

func TestEvaluationOperationsReturnTypedLookupAndLifecycleErrors(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(fmt.Sprint(workspace), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			opts := evaluationOptions()
			execute := opts.Execute
			opts.Execute = func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				close(started)
				select {
				case <-release:
					return execute(ctx, req)
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			s := New(opts)
			if workspace {
				s.Close()
				s = openPlatform(t, memory.NewData(), opts)
			}
			t.Cleanup(s.Close)
			ctx := context.Background()
			_, err := s.EvaluateAssertions(ctx, testCaller, "missing", AssertionsRequest{ScenarioYAML: evaluationYAML, Record: true})
			wantCode(t, err, CodeNotFound)
			_, err = s.ListEvaluations(ctx, testCaller, "missing")
			wantCode(t, err, CodeNotFound)
			_, err = s.GetEvaluation(ctx, testCaller, "missing")
			wantCode(t, err, CodeNotFound)
			run, err := s.SubmitRun(ctx, testCaller, quickRun("input"))
			if err != nil {
				t.Fatal(err)
			}
			receive(t, started)
			for _, record := range []bool{false, true} {
				_, err = s.EvaluateAssertions(ctx, testCaller, run.ID, AssertionsRequest{ScenarioYAML: evaluationYAML, Record: record})
				wantCode(t, err, CodeConflict)
			}
			for _, op := range []func(context.Context, Caller) error{
				func(ctx context.Context, caller Caller) error {
					_, err := s.GetEvaluation(ctx, caller, "missing")
					return err
				},
				func(ctx context.Context, caller Caller) error {
					_, err := s.ListEvaluations(ctx, caller, run.ID)
					return err
				},
				func(ctx context.Context, caller Caller) error {
					_, err := s.EvaluateAssertions(ctx, caller, run.ID, AssertionsRequest{ScenarioYAML: evaluationYAML})
					return err
				},
			} {
				wantCode(t, op(ctx, Caller{}), CodeInvalidRequest)
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				wantCode(t, op(cancelled, testCaller), CodeCancelled)
			}
			close(release)
			if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
