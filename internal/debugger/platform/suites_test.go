package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func suiteRequest(labels ...string) SubmitSuiteRequest {
	req := SubmitSuiteRequest{Config: Source{Path: "config"}}
	for _, label := range labels {
		req.Members = append(req.Members, ScenarioInput{Scenario: Source{Path: label}})
	}
	return req
}

func suiteReport(req engine.Request, outcome assertion.Outcome) *engine.Report {
	rep := report(req, runresult.StatusCompleted)
	rep.Scenario = req.Scenario
	rep.Assertions = &assertion.Report{Outcome: outcome}
	return rep
}

func waitSuite(t *testing.T, s *Service, suite *Suite) *SuiteStatus {
	t.Helper()
	for _, m := range suite.Members {
		if m.RunID != "" {
			r, err := s.Get(m.RunID)
			if err != nil {
				t.Fatal(err)
			}
			waitRun(t, r)
		}
	}
	status, err := s.GetSuite(context.Background(), testCaller, suite.ID)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestSuiteCapacityRejectsBeforePreparation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	s := New(Options{QueueCapacity: 2, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) { calls.Add(1); return req, nil }, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	t.Cleanup(s.Close)
	submit(t, s, request("active"))
	receive(t, started)
	submit(t, s, request("queued"))
	before := calls.Load()
	_, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("one", "two"))
	wantCode(t, err, CodeResourceExhausted)
	var pe *Error
	if !errors.As(err, &pe) || !pe.Retryable || calls.Load() != before || len(s.List()) != 2 {
		t.Fatalf("capacity rejection: %v, calls %d", err, calls.Load())
	}
	list, _ := s.ListSuites(context.Background(), testCaller)
	if len(list) != 0 {
		t.Fatal("rejected suite was recorded")
	}
}

func TestSuiteValidationPrecedesReservation(t *testing.T) {
	var calls atomic.Int32
	s := New(Options{QueueCapacity: 1, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) { calls.Add(1); return req, nil }})
	t.Cleanup(s.Close)
	for _, req := range []SubmitSuiteRequest{{Config: Source{Text: "config"}}, {Config: Source{Text: "config"}, Members: make([]ScenarioInput, MaxSuiteMembers+1)}, {Members: suiteRequest("one").Members}, {Config: Source{Path: "config", Text: "config"}, Members: suiteRequest("one").Members}} {
		_, err := s.SubmitSuite(context.Background(), testCaller, req)
		wantCode(t, err, CodeInvalidRequest)
	}
	if calls.Load() != 0 || s.reserved != 0 || len(s.List()) != 0 {
		t.Fatal("invalid suite did work")
	}
}

func TestSuiteReservationsBlockOrdinarySubmissionAndReleaseOnFailure(t *testing.T) {
	preparing, release := make(chan struct{}), make(chan struct{})
	s := New(Options{QueueCapacity: 2, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		if req.Scenario == "one" {
			close(preparing)
			select {
			case <-release:
			case <-ctx.Done():
				return req, ctx.Err()
			}
		}
		if req.Scenario != "" {
			return req, errors.New("setup failed")
		}
		return req, nil
	}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	finished := make(chan error, 1)
	go func() {
		_, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("one", "two"))
		finished <- err
	}()
	receive(t, preparing)
	_, err := s.SubmitRun(context.Background(), testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: "config"}, Quick: &QuickInput{Fixture: Source{Text: "{}"}}})
	wantCode(t, err, CodeResourceExhausted)
	close(release)
	if err := receive(t, finished); err != nil {
		t.Fatal(err)
	}
	if s.reserved != 0 || s.preparing != 0 {
		t.Fatal("reservations leaked")
	}
	waitRun(t, submit(t, s, request("now fits")))
}

func TestSuitePreparationFailureAndOutcomeRules(t *testing.T) {
	for _, tt := range []struct {
		name                string
		other               assertion.Outcome
		setup, noAssertions bool
		want                assertion.Outcome
	}{
		{"pass", assertion.Pass, false, false, assertion.Pass}, {"fail", assertion.Fail, false, false, assertion.Fail}, {"inconclusive", assertion.Inconclusive, false, false, assertion.Inconclusive}, {"assertion error", assertion.Error, false, false, assertion.Error}, {"setup error", assertion.Pass, true, false, assertion.Error}, {"no assertions", assertion.Pass, false, true, assertion.Error},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
				if tt.setup && req.Scenario == "other" {
					return req, errors.New("bad member setup")
				}
				return req, nil
			}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				if req.Scenario == "pass" {
					return suiteReport(req, assertion.Pass), nil
				}
				if tt.noAssertions {
					return report(req, runresult.StatusCompleted), nil
				}
				return suiteReport(req, tt.other), nil
			}})
			t.Cleanup(s.Close)
			suite, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("pass", "other"))
			if err != nil {
				t.Fatal(err)
			}
			status := waitSuite(t, s, suite)
			if !status.Done || status.Outcome != tt.want {
				t.Fatalf("status = %+v", status)
			}
			m := status.Members[1]
			if tt.setup && (m.RunID != "" || m.Error != "bad member setup" || m.Message != m.Error) {
				t.Fatalf("setup member = %+v", m)
			}
			if tt.noAssertions && m.Message != "scenario declares no assertions" {
				t.Fatal(m)
			}
			if !tt.setup && !tt.noAssertions && (m.Message != "" || m.Name != "other") {
				t.Fatalf("assertion report hidden: %+v", m)
			}
		})
	}
}

func TestSuiteMemberMessages(t *testing.T) {
	for _, tt := range []struct {
		name   string
		member SuiteMember
		run    *Run
		want   string
	}{
		{"preparation", SuiteMember{Error: "prepare failed"}, nil, "prepare failed"},
		{"own error", SuiteMember{}, &Run{State: StateError, Error: "execution failed"}, "execution failed"},
		{"cancelled", SuiteMember{}, &Run{State: StateCancelled}, "run cancelled"},
		{"interrupted", SuiteMember{}, &Run{State: StateError, Interrupted: true}, "run interrupted"},
		{"failed", SuiteMember{}, &Run{State: StateError}, "run failed"},
		{"no assertions", SuiteMember{}, &Run{State: StateDone, Report: &engine.Report{}}, "scenario declares no assertions"},
		{"own cancelled error", SuiteMember{}, &Run{State: StateCancelled, Error: "deadline"}, "deadline"},
		{"own interrupted error", SuiteMember{}, &Run{State: StateError, Interrupted: true, Error: "owner stopped"}, "owner stopped"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := memberStatus(tt.member, tt.run)
			if got.Outcome != assertion.Error || got.Message != tt.want {
				t.Fatalf("member = %+v", got)
			}
		})
	}
}

func TestSuiteCancellationDuringPreparationWritesNoSuite(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		t.Run(fmt.Sprint(workspace), func(t *testing.T) {
			preparing, started := make(chan struct{}), make(chan string, 2)
			opts := Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
				if req.Scenario == "second" {
					close(preparing)
					<-ctx.Done()
					return req, ctx.Err()
				}
				return req, nil
			}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
				started <- req.RunID
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			var s *Service
			if workspace {
				s = openPlatform(t, memory.NewData(), opts)
			} else {
				s = New(opts)
				t.Cleanup(s.Close)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := s.SubmitSuite(ctx, testCaller, suiteRequest("first", "second", "third"))
				finished <- err
			}()
			receive(t, preparing)
			firstID := receive(t, started)
			cancel()
			err := receive(t, finished)
			wantCode(t, err, CodeCancelled)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			r, err := s.Get(firstID)
			if err != nil {
				t.Fatal(err)
			}
			waitRun(t, r)
			list, _ := s.ListSuites(context.Background(), testCaller)
			if len(list) != 0 || s.reserved != 0 || s.preparing != 0 || s.suiteSubmitting != 0 {
				t.Fatal("cancelled submission retained a suite or reservation")
			}
			if workspace {
				records, err := s.repository.Suites(context.Background())
				if err != nil || len(records) != 0 {
					t.Fatal("suite written", err)
				}
			}
		})
	}
}

func TestSuiteQueuedCancellationAndCallerDetachment(t *testing.T) {
	started := make(chan struct{}, 1)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	t.Cleanup(s.Close)
	submit(t, s, request("blocker"))
	receive(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("queued"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	status, _ := s.GetSuite(context.Background(), testCaller, suite.ID)
	if status.Done || status.Outcome != "" || status.Members[0].Outcome != "" || status.Members[0].State != StateQueued {
		t.Fatal("caller cancelled admitted suite", status)
	}
	if _, err := s.CancelRun(context.Background(), testCaller, suite.Members[0].RunID); err != nil {
		t.Fatal(err)
	}
	status = waitSuite(t, s, suite)
	if status.Outcome != assertion.Error || status.Members[0].Message != "run cancelled" {
		t.Fatal(status)
	}
}

func TestSuiteEphemeralRetentionPinsMemberRuns(t *testing.T) {
	s := New(Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return suiteReport(req, assertion.Pass), nil
	}})
	t.Cleanup(s.Close)
	var oldest *Suite
	for i := 0; i < 21; i++ {
		suite, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		waitSuite(t, s, suite)
		if i == 0 {
			oldest = suite
		}
		waitRun(t, submit(t, s, request("ordinary")))
	}
	list, err := s.ListSuites(context.Background(), testCaller)
	if err != nil || len(list) != 20 || list[0].Members[0].Label != "20" || list[19].Members[0].Label != "1" {
		t.Fatal(list, err)
	}
	_, err = s.GetSuite(context.Background(), testCaller, oldest.ID)
	wantCode(t, err, CodeNotFound)
	if _, err := s.Get(oldest.Members[0].RunID); !errors.Is(err, ErrUnknownRun) {
		t.Fatal("evicted suite kept its run")
	}
	for _, suite := range list {
		if waitSuite(t, s, &suite).Outcome != assertion.Pass {
			t.Fatal("retained suite lost evidence")
		}
	}
	if len(s.List()) != 21 {
		t.Fatalf("retained runs = %d", len(s.List()))
	}
	list[0].Members[0].Label = "mutated"
	fresh, _ := s.ListSuites(context.Background(), testCaller)
	if fresh[0].Members[0].Label == "mutated" {
		t.Fatal("suite metadata aliased")
	}
}

func TestRunSuiteChunksPreserveOrderAndAssertionErrors(t *testing.T) {
	s := New(Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		outcome := assertion.Pass
		if req.Scenario == "17" {
			outcome = assertion.Error
		}
		return suiteReport(req, outcome), nil
	}})
	t.Cleanup(s.Close)
	paths := make([]string, 65)
	for i := range paths {
		paths[i] = fmt.Sprint(i)
	}
	rep, err := s.RunSuite(context.Background(), testCaller, SuiteRequest{Config: "config", Scenarios: paths})
	if err != nil || rep.Outcome != assertion.Error || len(rep.Scenarios) != len(paths) {
		t.Fatal(rep, err)
	}
	for i, result := range rep.Scenarios {
		if result.Path != paths[i] || result.Name != paths[i] || result.RunID == "" || result.Assertions == nil || result.Error != "" || result.Duration < 0 {
			t.Fatalf("member %d: %+v", i, result)
		}
	}
	list, _ := s.ListSuites(context.Background(), testCaller)
	if len(list) != 3 || len(list[0].Members) != 1 || len(list[1].Members) != 32 || len(list[2].Members) != 32 {
		t.Fatal("wrong chunk sizes", list)
	}
}

func TestSuiteCapturesMemberFileBeforeItRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	configPath, scenarioPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	config := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.json"), fixture, 0600); err != nil {
		t.Fatal(err)
	}
	text := "version: 1\nname: captured\ninputs: [{file: input.json}]\nassertions: [{target: {exporter: otlp, signal: traces}, count: {equals: 3}}]\n"
	if err := os.WriteFile(scenarioPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	calls := 0
	s := New(Options{Paths: PathPolicy{Unrestricted: true}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		calls++
		if calls == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return report(req, runresult.StatusCompleted), nil
		}
		return engine.Execute(ctx, req)
	}})
	t.Cleanup(s.Close)
	submit(t, s, engine.Request{ConfigYAML: config, FixtureJSON: string(fixture)})
	receive(t, started)
	suite, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Path: configPath}, Members: []ScenarioInput{{Scenario: Source{Path: scenarioPath}}}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := s.Get(suite.Members[0].RunID)
	before := append([]byte(nil), r.bundle...)
	if err := os.WriteFile(scenarioPath, []byte(strings.ReplaceAll(text, "equals: 3", "equals: 99")), 0600); err != nil {
		t.Fatal(err)
	}
	close(release)
	status := waitSuite(t, s, suite)
	if status.Outcome != assertion.Pass || status.Members[0].Name != "captured" || !bytes.Equal(before, r.bundle) {
		t.Fatal("member changed after submission", status)
	}
	decoded, err := engine.DecodeBundle(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Prepared() {
		t.Fatal("member did not retain its capture")
	}
}

func TestSuiteSurvivesCrashWithInterruptedMembers(t *testing.T) {
	ctx := context.Background()
	data := memory.NewData()
	var started sync.Once
	startedCh := make(chan struct{})
	crashed, kill := openKillable(t, data, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started.Do(func() { close(startedCh) })
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	before, err := crashed.SubmitSuite(ctx, testCaller, suiteRequest("active", "queued"))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, startedCh)
	kill()
	store := memory.Open(data, artifact.Limits{})
	t.Cleanup(store.Close)
	beforeRecords, err := store.Suites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s := openPlatform(t, data, Options{Execute: func(context.Context, engine.Request) (*engine.Report, error) {
		t.Error("replayed suite")
		return nil, nil
	}})
	status, err := s.GetSuite(context.Background(), testCaller, before.ID)
	if err != nil || !status.Done || status.Outcome != assertion.Error || !reflect.DeepEqual(status.Suite, *before) {
		t.Fatal(status, err)
	}
	for _, member := range status.Members {
		if member.State != StateError || member.Outcome != assertion.Error || !strings.Contains(member.Message, "interrupted") {
			t.Fatal(member)
		}
	}
	afterRecords, err := store.Suites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRecords, afterRecords) {
		t.Fatal("recovery rewrote suite records")
	}
}

func TestSuiteWorkspaceCapReservesBeforeMembers(t *testing.T) {
	preparing, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s := openPlatformLimits(t, memory.NewData(), artifact.Limits{Suites: 1}, Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		calls.Add(1)
		close(preparing)
		select {
		case <-release:
		case <-ctx.Done():
			return req, ctx.Err()
		}
		return req, nil
	}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return suiteReport(req, assertion.Pass), nil
	}})
	finished := make(chan *Suite, 1)
	errCh := make(chan error, 1)
	go func() {
		suite, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("first"))
		finished <- suite
		errCh <- err
	}()
	receive(t, preparing)
	_, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("second"))
	wantCode(t, err, CodeResourceExhausted)
	if calls.Load() != 1 {
		t.Fatal("suite-count cap prepared a member")
	}
	close(release)
	suite := receive(t, finished)
	if err := receive(t, errCh); err != nil {
		t.Fatal(err)
	}
	waitSuite(t, s, suite)
	before, err := s.repository.Suites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SubmitSuite(context.Background(), testCaller, suiteRequest("third"))
	wantCode(t, err, CodeResourceExhausted)
	after, err := s.repository.Suites(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !reflect.DeepEqual(before, after) || len(s.List()) != 1 {
		t.Fatal("cap wrote or admitted work")
	}
}

func TestRunSuiteWaitCancellationCleansUpAllMembers(t *testing.T) {
	started, stopped, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		<-cleanup
		return nil, errors.New("run cancelled")
	}})
	t.Cleanup(s.Close)
	suite, handles, err := s.submitSuite(context.Background(), testCaller, authz.RunSubmit, suiteRequest("running", "queued"))
	if err != nil {
		t.Fatal(err)
	}
	receive(t, started)
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan []engine.ScenarioResult, 1)
	go func() { results <- suiteResults(ctx, suite, handles) }()
	cancel()
	receive(t, stopped)
	select {
	case <-results:
		t.Fatal("returned before executor cleanup")
	default:
	}
	close(cleanup)
	got := receive(t, results)
	for i, result := range got {
		if result.Outcome != assertion.Error || result.Error != "run cancelled" || result.RunID != handles[i].ID() {
			t.Fatal(result)
		}
	}
	if got[1].Duration != 0 {
		t.Fatal("queued cancellation acquired an execution duration")
	}
}

func TestRunSuiteHandlesSurviveSuiteEviction(t *testing.T) {
	s := New(Options{HistoryLimit: 1, Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return suiteReport(req, assertion.Pass), nil
	}})
	t.Cleanup(s.Close)
	suite, handles, err := s.submitSuite(context.Background(), testCaller, authz.RunSubmit, suiteRequest("old"))
	if err != nil {
		t.Fatal(err)
	}
	waitSuite(t, s, suite)
	for i := 0; i < 20; i++ {
		next, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest(fmt.Sprint(i)))
		if err != nil {
			t.Fatal(err)
		}
		waitSuite(t, s, next)
	}
	waitRun(t, submit(t, s, request("ordinary")))
	if _, err := s.Get(handles[0].ID()); !errors.Is(err, ErrUnknownRun) {
		t.Fatal("old run remained in history")
	}
	results := suiteResults(context.Background(), suite, handles)
	if len(results) != 1 || results[0].Outcome != assertion.Pass || results[0].RunID != handles[0].ID() || results[0].Assertions == nil {
		t.Fatal("held evidence lost", results)
	}
}

func TestRunSuiteContextCancellationReturnsReport(t *testing.T) {
	preparing, started := make(chan struct{}), make(chan string, 1)
	s := New(Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		if req.Scenario == "second" {
			close(preparing)
			<-ctx.Done()
			return req, ctx.Err()
		}
		return req, nil
	}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- req.RunID
		<-ctx.Done()
		return nil, errors.New("run cancelled")
	}})
	t.Cleanup(s.Close)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan *SuiteReport, 1)
	errCh := make(chan error, 1)
	go func() {
		rep, err := s.RunSuite(ctx, testCaller, SuiteRequest{Config: "config", Scenarios: []string{"first", "second", "third"}})
		finished <- rep
		errCh <- err
	}()
	receive(t, preparing)
	id := receive(t, started)
	cancel()
	rep := receive(t, finished)
	if err := receive(t, errCh); err != nil {
		t.Fatal("foreground returned context error", err)
	}
	if rep == nil || len(rep.Scenarios) != 3 || rep.Outcome != assertion.Error || rep.Scenarios[0].RunID != id {
		t.Fatal(rep)
	}
	for _, member := range rep.Scenarios {
		if member.Error != "run cancelled" || member.Outcome != assertion.Error {
			t.Fatal(member)
		}
	}
	list, _ := s.ListSuites(context.Background(), testCaller)
	if len(list) != 0 {
		t.Fatal("cancelled preparation wrote a suite")
	}
}

func TestSuitePublicationQuotaLeavesOrdinaryRunWithoutSuite(t *testing.T) {
	started, release := make(chan struct{}, 2), make(chan struct{})
	s := openPlatformLimits(t, memory.NewData(), artifact.Limits{CatalogBytes: 10 << 10, Suites: 1}, Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return suiteReport(req, assertion.Pass), nil
	}})
	_, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest(strings.Repeat("s", 12<<10)))
	wantCode(t, err, CodeResourceExhausted)
	receive(t, started)
	runs := s.List()
	if len(runs) != 1 {
		t.Fatal("publication failure lost ordinary run", runs)
	}
	list, err := s.ListSuites(context.Background(), testCaller)
	if err != nil || len(list) != 0 || s.suiteSubmitting != 0 || s.reserved != 0 {
		t.Fatal("failed suite retained metadata or reservations", err)
	}
	close(release)
	r, _ := s.Get(runs[0].ID)
	waitRun(t, r)
	suite, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("fits"))
	if err != nil {
		t.Fatal("suite-count reservation leaked", err)
	}
	waitSuite(t, s, suite)
}

func TestSuiteInlineLabelsAndOperationErrors(t *testing.T) {
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		return suiteReport(req, assertion.Pass), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	suite, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Text: "config"}, Members: []ScenarioInput{{Scenario: Source{Text: "first"}}, {Scenario: Source{Text: "second"}}}})
	if err != nil || suite.Config != "inline" || suite.Caller != testCaller.Actor() || suite.Members[0].Label != "inline scenario 1" || suite.Members[1].Label != "inline scenario 2" {
		t.Fatal(suite, err)
	}
	waitSuite(t, s, suite)
	_, err = s.GetSuite(ctx, testCaller, "missing")
	wantCode(t, err, CodeNotFound)
	_, err = s.ListSuites(ctx, Caller{})
	wantCode(t, err, CodeInvalidRequest)
	_, err = s.GetSuite(ctx, Caller{}, suite.ID)
	wantCode(t, err, CodeInvalidRequest)
	_, err = s.SubmitSuite(ctx, Caller{}, suiteRequest("member"))
	wantCode(t, err, CodeInvalidRequest)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.ListSuites(cancelled, testCaller)
	wantCode(t, err, CodeCancelled)
	_, err = s.GetSuite(cancelled, testCaller, suite.ID)
	wantCode(t, err, CodeCancelled)
	s.Close()
	_, err = s.GetSuite(ctx, testCaller, suite.ID)
	wantCode(t, err, CodeUnavailable)
	_, err = s.ListSuites(ctx, testCaller)
	wantCode(t, err, CodeUnavailable)
	_, err = s.SubmitSuite(ctx, testCaller, suiteRequest("member"))
	wantCode(t, err, CodeUnavailable)
}

func TestSuiteShutdownReleasesAllReservations(t *testing.T) {
	preparing := make(chan struct{})
	s := New(Options{Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
		close(preparing)
		<-ctx.Done()
		return req, ctx.Err()
	}})
	t.Cleanup(s.Close)
	finished := make(chan error, 1)
	go func() {
		_, err := s.SubmitSuite(context.Background(), testCaller, suiteRequest("one", "two"))
		finished <- err
	}()
	receive(t, preparing)
	s.Close()
	wantCode(t, receive(t, finished), CodeUnavailable)
	if s.reserved != 0 || s.preparing != 0 || s.suiteSubmitting != 0 {
		t.Fatal("shutdown leaked reservations")
	}
}

// GetSuiteReport derives RunSuite's report from a submitted suite; ending a
// wait leaves the members running.
func TestGetSuiteReportDerivesTheSuiteReport(t *testing.T) {
	release := make(chan struct{})
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		<-release
		outcome := assertion.Pass
		if req.Scenario == "b" {
			outcome = assertion.Fail
		}
		return suiteReport(req, outcome), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	suite, err := s.SubmitSuite(ctx, testCaller, suiteRequest("a", "b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSuiteReport(ctx, testCaller, suite.ID, false); ErrorCode(err) != CodeConflict {
		t.Fatalf("unfinished suite: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetSuiteReport(cancelled, testCaller, suite.ID, true); ErrorCode(err) != CodeCancelled {
		t.Fatalf("ended wait: %v", err)
	}
	if _, err := s.GetSuiteReport(ctx, testCaller, "suite-missing", true); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown suite: %v", err)
	}
	close(release)
	rep, err := s.GetSuiteReport(ctx, testCaller, suite.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != engine.TestSchemaVersion || rep.Config != "config" || rep.Outcome != assertion.Fail || len(rep.Scenarios) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	for i, want := range []assertion.Outcome{assertion.Pass, assertion.Fail} {
		got := rep.Scenarios[i]
		if got.Path != suite.Members[i].Label || got.RunID != suite.Members[i].RunID || got.Outcome != want || got.Assertions == nil || got.Error != "" {
			t.Fatalf("member %d: %+v", i, got)
		}
	}
	local, err := s.RunSuite(ctx, testCaller, SuiteRequest{Config: "config", Scenarios: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range local.Scenarios {
		local.Scenarios[i].RunID, local.Scenarios[i].Duration = rep.Scenarios[i].RunID, rep.Scenarios[i].Duration
	}
	if !reflect.DeepEqual(local, rep) {
		t.Fatalf("RunSuite = %+v, GetSuiteReport = %+v", local, rep)
	}
}
