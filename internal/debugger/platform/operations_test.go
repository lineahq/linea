package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func quickRun(label string) RunRequest {
	return RunRequest{Mode: ModeQuick, Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: label}}}
}

func wantCode(t *testing.T, err error, code Code) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v (%s), want %s", err, ErrorCode(err), code)
	}
}

func TestOperationsRequireACallerFromTheAdapter(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	for _, caller := range []Caller{
		{}, {Principal: LocalPrincipal()}, {Principal: LocalPrincipal(), Client: "bad\nclient"},
		{Principal: Principal{ID: LocalOwner}, Client: "http"},
		{Principal: Principal{ID: "intruder", Kind: PrincipalLocal}, Client: "http"},
		{Principal: Principal{ID: "user:a", Kind: PrincipalUser, Groups: []string{""}}, Client: "http"},
	} {
		_, err := s.SubmitRun(ctx, caller, quickRun("x"))
		wantCode(t, err, CodeInvalidRequest)
		_, err = s.InspectConfiguration(ctx, caller, InspectRequest{Config: Source{Text: "receivers: {}"}})
		wantCode(t, err, CodeInvalidRequest)
		_, err = allRuns(ctx, s, caller, RunFilter{})
		wantCode(t, err, CodeInvalidRequest)
	}
	if runs, _ := allRuns(ctx, s, testCaller, RunFilter{}); len(runs) != 0 {
		t.Fatal("a run was admitted without a caller")
	}
}

func TestSubmitRunNeedsAnExplicitModeMatchingItsInput(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	scenario := &ScenarioInput{Scenario: Source{Text: "scenario"}}
	for name, req := range map[string]RunRequest{
		"no mode":           {Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "x"}}},
		"quick as scenario": {Mode: ModeScenario, Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "x"}}},
		"scenario as quick": {Mode: ModeQuick, Config: Source{Text: "config"}, Scenario: scenario},
		"both inputs":       {Mode: ModeScenario, Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "x"}}, Scenario: scenario},
		"quick field":       {Mode: ModeScenario, Config: Source{Text: "config"}, Quick: &QuickInput{Receiver: "otlp"}, Scenario: scenario},
		"path and text":     {Mode: ModeQuick, Config: Source{Path: "c.yaml", Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "x"}}},
	} {
		if _, err := s.SubmitRun(ctx, testCaller, req); ErrorCode(err) != CodeInvalidRequest || !errors.Is(err, engine.ErrInvalidRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	quick, err := s.SubmitRun(ctx, testCaller, quickRun("x"))
	if err != nil || quick.Mode != ModeQuick || quick.Caller != testCaller.Actor() {
		t.Fatalf("quick = %+v, %v", quick, err)
	}
	run, err := s.SubmitRun(ctx, Local("other"), RunRequest{Mode: ModeScenario, Config: Source{Text: "config"}, Scenario: scenario})
	if err != nil || run.Mode != ModeScenario || run.Caller.Client != "other" {
		t.Fatalf("scenario = %+v, %v", run, err)
	}
}

func TestOperationErrorsAreTyped(t *testing.T) {
	started, release := make(chan string, 2), make(chan struct{})
	s := New(Options{QueueCapacity: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.RunID
		<-release
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	_, err := s.GetRun(ctx, testCaller, "missing")
	wantCode(t, err, CodeNotFound)
	if !errors.Is(err, ErrUnknownRun) {
		t.Fatal("typed error lost its cause")
	}
	running, err := s.SubmitRun(ctx, testCaller, quickRun("running"))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	queued, err := s.SubmitRun(ctx, testCaller, quickRun("queued"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitRun(ctx, testCaller, quickRun("full"))
	wantCode(t, err, CodeResourceExhausted)
	if !err.(*Error).Retryable {
		t.Fatal("a full queue is retryable")
	}
	_, err = s.GetRunReport(ctx, testCaller, running.ID, false)
	wantCode(t, err, CodeConflict)
	_, err = s.GetStageChanges(ctx, testCaller, queued.ID)
	wantCode(t, err, CodeConflict)

	cancelled, err := s.CancelRun(ctx, testCaller, queued.ID)
	if err != nil || !cancelled.Effective || cancelled.Run.State != StateCancelled {
		t.Fatalf("cancel = %+v, %v", cancelled, err)
	}
	again, err := s.CancelRun(ctx, testCaller, queued.ID)
	if err != nil || again.Effective {
		t.Fatalf("repeated cancel = %+v, %v", again, err)
	}
	_, err = s.GetRunReport(ctx, testCaller, queued.ID, false)
	if wantCode(t, err, CodeConflict); err.Error() != "run was cancelled before it started" {
		t.Fatal(err)
	}
	close(release)
	if _, err := s.GetRunReport(ctx, testCaller, running.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetSnapshot(ctx, testCaller, running.ID, "sha256:missing")
	wantCode(t, err, CodeNotFound)
	s.Close()
	_, err = s.SubmitRun(ctx, testCaller, quickRun("closed"))
	wantCode(t, err, CodeUnavailable)
}

func TestRunReportOfAFailedRunNamesTheFailure(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return nil, errors.New("collector failed to start")
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	run, err := s.SubmitRun(ctx, testCaller, quickRun("x"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.GetRunReport(ctx, testCaller, run.ID, true)
	if wantCode(t, err, CodeRunFailed); err.Error() != "collector failed to start" {
		t.Fatal(err)
	}
}

func TestWatchRunResumesAfterASequenceAndEndsWithTheOutcome(t *testing.T) {
	release := make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		for seq := 1; seq <= 2; seq++ {
			req.Observer(instrument.Event{RunID: req.RunID, Seq: seq})
		}
		<-release
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	run, err := s.SubmitRun(ctx, testCaller, quickRun("x"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []int
	for after := (EventCursor{}); ; {
		w, err := s.WatchRun(ctx, testCaller, run.ID, after)
		if err != nil {
			t.Fatal(err)
		}
		if w.Restarted {
			t.Fatal("a first attempt restarted the watch")
		}
		for _, e := range w.Events {
			seen, after = append(seen, e.Seq), EventCursor{Attempt: w.Attempt, Seq: e.Seq}
		}
		if w.Terminal {
			if w.End.State != StateDone || w.End.Status != runresult.StatusCompleted {
				t.Fatalf("end = %+v", w.End)
			}
			break
		}
		if after.Seq == 2 {
			close(release)
		}
		select {
		case <-w.Changed:
		case <-time.After(5 * time.Second):
			t.Fatal("watch missed a change")
		}
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("events = %v", seen)
	}
}

func TestRunCallerSurvivesWorkspaceRestart(t *testing.T) {
	data := memory.NewData()
	opts := Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}}
	s := openPlatform(t, data, opts)
	ctx := context.Background()
	run, err := s.SubmitRun(ctx, Local("cli"), quickRun("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openPlatform(t, data, opts)
	got, err := s.GetRun(ctx, testCaller, run.ID)
	if err != nil || got.Caller != Local("cli").Actor() {
		t.Fatalf("recovered caller = %+v, %v", got.Caller, err)
	}
}

func TestInspectionAndPreviewRunOutsideTheExecutionQueue(t *testing.T) {
	blocked := make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		<-blocked
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(func() { close(blocked); s.Close() })
	ctx := context.Background()
	if _, err := s.SubmitRun(ctx, testCaller, quickRun("busy")); err != nil {
		t.Fatal(err)
	}
	config := "receivers: {otlp: {protocols: {http: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['set(attributes[\"seen\"], true)']}]}}\nexporters: {debug: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [debug]}}}\n"
	ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Text: config}})
	if err != nil || !ins.Valid {
		t.Fatalf("inspection = %+v, %v", ins, err)
	}
	invalid, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Text: "receivers: {nope: {}}"}})
	if err != nil || invalid.Valid {
		t.Fatalf("an invalid configuration is a result: %+v, %v", invalid, err)
	}
	input := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"x"}}]}]}]}`
	preview, err := s.PreviewTransformation(ctx, testCaller, PreviewRequest{Config: Source{Text: config}, Processor: "transform", Input: []byte(input)})
	if err != nil || !preview.Valid || !strings.Contains(string(preview.Output), "seen") {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	// Both name the runtime that answered: here, this process's.
	for what, rt := range map[string]*engine.ReportRuntime{"inspection": ins.Runtime, "invalid inspection": invalid.Runtime, "preview": preview.Runtime} {
		if rt == nil || rt.Kind != engine.RuntimeBuiltIn || rt.CollectorVersion == "" {
			t.Errorf("%s runtime = %+v", what, rt)
		}
	}
	_, err = s.PreviewTransformation(ctx, testCaller, PreviewRequest{Config: Source{Text: config}, Processor: "missing", Input: []byte(input)})
	wantCode(t, err, CodeInvalidRequest)
}

// TestInspectionAndPreviewFollowTheServicePolicy: a service composed with the
// zero (explicit) environment policy does not read the host for operations
// that start no Collector, exactly as for runs: the name stands in.
func TestInspectionAndPreviewFollowTheServicePolicy(t *testing.T) {
	const name = "OTEL_PLATFORM_INSPECT_BOUND"
	t.Setenv(name, "private-host-value")
	config := "receivers: {otlp: {protocols: {http: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['set(log.attributes[\"bound\"], \"${env:" + name + "}\")']}]}}\nexporters: {debug: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [debug]}}}\n"
	input := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"x"}}]}]}]}`
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		policy EnvironmentPolicy
		host   bool
	}{
		{"explicit", EnvironmentPolicy{}, false},
		{"imported", EnvironmentPolicy{Host: []string{name}}, true},
		{"legacy", EnvironmentPolicy{LegacyHost: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Options{Environment: tt.policy})
			t.Cleanup(s.Close)
			ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Text: config}})
			if err != nil {
				t.Fatal(err)
			}
			effective := fmt.Sprint(ins.Effective)
			standIn := slices.ContainsFunc(ins.Diagnostics, func(d Diagnostic) bool { return d.Code == "reference_stand_in" })
			if !ins.Valid || strings.Contains(effective, "private-host-value") != tt.host || standIn == tt.host {
				t.Fatalf("inspection valid = %v, diagnostics = %+v, effective = %s", ins.Valid, ins.Diagnostics, effective)
			}
			preview, err := s.PreviewTransformation(ctx, testCaller, PreviewRequest{Config: Source{Text: config}, Processor: "transform", Input: []byte(input)})
			if err != nil {
				t.Fatal(err)
			}
			if !preview.Valid || strings.Contains(string(preview.Output), "private-host-value") != tt.host {
				t.Fatalf("preview valid = %v, diagnostics = %+v, output = %s", preview.Valid, preview.Diagnostics, preview.Output)
			}
		})
	}
}

// TestPathRefusalsAreForbiddenForEveryCaller: under roots, a run, an
// inspection and a preview naming a file outside them fail with forbidden for
// every client, the same file inside a root is read, and an unrestricted
// service reads both.
func TestPathRefusalsAreForbiddenForEveryCaller(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	input := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"x"}}]}]}]}`
	config := "receivers: {otlp: {protocols: {http: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['set(log.attributes[\"a\"], \"b\")']}]}}\nexporters: {debug: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [debug]}}}\n"
	for _, dir := range []string{root, outside} {
		for name, text := range map[string]string{"config.yaml": config, "input.json": input} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	ops := map[string]func(*Service, Caller, string) error{
		"run": func(s *Service, c Caller, dir string) error {
			_, err := s.SubmitRun(context.Background(), c, RunRequest{Mode: ModeQuick, Config: Source{Path: filepath.Join(dir, "config.yaml")}, Quick: &QuickInput{Fixture: Source{Path: filepath.Join(dir, "input.json")}}})
			return err
		},
		"inspect": func(s *Service, c Caller, dir string) error {
			_, err := s.InspectConfiguration(context.Background(), c, InspectRequest{Config: Source{Path: filepath.Join(dir, "config.yaml")}})
			return err
		},
		"preview": func(s *Service, c Caller, dir string) error {
			_, err := s.PreviewTransformation(context.Background(), c, PreviewRequest{Config: Source{Path: filepath.Join(dir, "config.yaml")}, Processor: "transform", Input: []byte(input)})
			return err
		},
	}
	noop := func(context.Context, engine.Request) (*engine.Report, error) { return &engine.Report{}, nil }
	rooted := New(Options{Paths: PathPolicy{Roots: []string{root}}, Execute: noop})
	t.Cleanup(rooted.Close)
	open := New(Options{Paths: PathPolicy{Unrestricted: true}, Execute: noop})
	t.Cleanup(open.Close)
	for name, op := range ops {
		for _, client := range []string{"cli", "http", "mcp"} {
			t.Run(name+"/"+client, func(t *testing.T) {
				caller := Local(client)
				err := op(rooted, caller, outside)
				wantCode(t, err, CodeForbidden)
				if !strings.Contains(err.Error(), outside) || strings.Contains(err.Error(), "resourceLogs") {
					t.Fatalf("refusal message %q: want the path and no contents", err)
				}
				if err := op(rooted, caller, root); err != nil {
					t.Fatalf("inside the root: %v", err)
				}
				if err := op(open, caller, outside); err != nil {
					t.Fatalf("unrestricted: %v", err)
				}
			})
		}
	}
	// Inline content needs no root.
	inline := New(Options{Execute: noop})
	t.Cleanup(inline.Close)
	if _, err := inline.InspectConfiguration(context.Background(), testCaller, InspectRequest{Config: Source{Text: config}}); err != nil {
		t.Fatalf("inline content without roots: %v", err)
	}
	_, err := inline.InspectConfiguration(context.Background(), testCaller, InspectRequest{Config: Source{Path: filepath.Join(root, "config.yaml")}})
	wantCode(t, err, CodeForbidden)
}

// TestUnusablePathPoliciesAreInternalErrors: the policy is trusted
// composition configuration, so a root that disappears is not the request's
// fault.
func TestUnusablePathPoliciesAreInternalErrors(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	s := New(Options{Paths: PathPolicy{Roots: []string{gone}}})
	t.Cleanup(s.Close)
	_, err := s.InspectConfiguration(context.Background(), testCaller, InspectRequest{Config: Source{Text: "receivers: {}"}})
	wantCode(t, err, CodeInternal)

}
