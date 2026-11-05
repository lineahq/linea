package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/engine"
)

func TestCompareRunsReadsTwoTerminalRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := New(Options{})
	t.Cleanup(s.Close)
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "samples", "logs-cleanup", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	config, fixture := read("config.yaml"), read("input.json")
	quick := func(fixture string) string {
		run, err := s.SubmitRun(ctx, testCaller, RunRequest{Mode: ModeQuick, Config: Source{Text: config}, Quick: &QuickInput{Fixture: Source{Text: fixture}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetRunReport(ctx, testCaller, run.ID, true); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	a, b := quick(fixture), quick(fixture)
	c, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: a, After: b})
	if err != nil {
		t.Fatal(err)
	}
	if c.Outcome != ComparisonSame || !c.InputsIdentical || c.Before.Mode != "quick" || c.Before.Build == "" || c.Before.Build != c.After.Build ||
		c.Before.InputsDigest == "" || c.Records == nil {
		t.Fatalf("identical runs = %+v", c)
	}

	// Other input bytes carrying the same telemetry: the inputs are not proven
	// identical, so the runs cannot be compared, however equal their outputs.
	other := quick(fixture[:len(fixture)-1] + " ")
	c, err = s.CompareRuns(ctx, testCaller, CompareRequest{Before: a, After: other})
	if err != nil || c.InputsIdentical || c.Records != nil || c.Outcome != ComparisonInconclusive || len(c.Reasons) != 1 ||
		c.Reasons[0].Code != ComparisonInputsDiffer || len(c.Exporters) == 0 || !c.Exporters[0].Equal {
		t.Fatalf("different input bytes, same telemetry = %+v, %v", c, err)
	}

	if _, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: a, After: b, Ignore: []string{"body"}}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid rule: %v", err)
	}
	if _, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: a, After: "missing"}); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown run: %v", err)
	}
}

func TestCompareRunsRefusesUnfinishedRuns(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	s := New(Options{Prepare: unprepared, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-release
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	first := submit(t, s, request("first"))
	receive(t, started)
	second := submit(t, s, request("second"))
	for _, pair := range [][2]string{{first.ID(), second.ID()}, {second.ID(), first.ID()}} {
		if _, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: pair[0], After: pair[1]}); ErrorCode(err) != CodeConflict {
			t.Fatalf("compare running and queued runs: %v", err)
		}
	}
	close(release)
	waitRun(t, first)
	waitRun(t, second)
	// Terminal runs without captured bundles compare with unknown inputs.
	c, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: first.ID(), After: second.ID()})
	if err != nil || c.InputsIdentical || c.Before.InputsDigest != "" || c.Outcome != ComparisonInconclusive {
		t.Fatalf("comparison = %+v, %v", c, err)
	}
}

func TestSubmitComparisonRunsBothConfigurationsOnOneCapturedInput(t *testing.T) {
	samples := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	config, err := os.ReadFile(filepath.Join(samples, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(config), `"upper"`, `"lower"`, 1)
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dir := t.TempDir()
			for _, name := range []string{"input.json", "scenario.yaml"} {
				raw, err := os.ReadFile(filepath.Join(samples, name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			policy := engine.Policy{Paths: PathPolicy{Unrestricted: true}}
			var prepared atomic.Int32
			s := New(Options{Paths: PathPolicy{Unrestricted: true}, Prepare: func(ctx context.Context, req engine.Request) (engine.Request, error) {
				out, err := engine.PrepareWithPolicy(ctx, req, policy)
				if prepared.Add(1) == 1 {
					// The after run must not read the input again.
					if err := os.WriteFile(filepath.Join(dir, "input.json"), []byte(`{"resourceLogs":[]}`), 0o600); err != nil {
						t.Error(err)
					}
				}
				return out, err
			}})
			t.Cleanup(s.Close)
			req := ComparisonRequest{Before: Source{Text: string(config)}, After: Source{Text: edited}, Mode: mode}
			if mode == ModeQuick {
				req.Quick = &QuickInput{Fixture: Source{Path: filepath.Join(dir, "input.json")}}
			} else {
				req.Scenario = &ScenarioInput{Scenario: Source{Path: filepath.Join(dir, "scenario.yaml")}}
			}
			runs, err := s.SubmitComparison(ctx, testCaller, req)
			if err != nil {
				t.Fatal(err)
			}
			if runs.Before.Mode != mode || runs.After.Mode != mode || runs.Before.ID == runs.After.ID {
				t.Fatalf("runs = %+v", runs)
			}
			for _, id := range []string{runs.Before.ID, runs.After.ID} {
				if _, err := s.GetRunReport(ctx, testCaller, id, true); err != nil {
					t.Fatal(err)
				}
			}
			c, err := s.CompareRuns(ctx, testCaller, CompareRequest{Before: runs.Before.ID, After: runs.After.ID})
			if err != nil {
				t.Fatal(err)
			}
			if !c.InputsIdentical || c.Outcome != ComparisonDifferent || c.Records == nil || c.Records.Aligned != 4 {
				t.Fatalf("comparison = %+v", c)
			}
			// Exactly the edited field differs.
			for _, e := range c.Exporters {
				if len(e.Changes) != 1 || e.Changes[0].Path != "log.severity_text" || e.Before != 3 || e.After != 3 {
					t.Fatalf("exporter %+v", e)
				}
			}
			inputs, err := s.GetRunInputs(ctx, testCaller, runs.After.ID)
			if err != nil || inputs.Config.Text != edited {
				t.Fatalf("after inputs = %+v, %v", inputs, err)
			}
		})
	}
}

func TestSubmitComparisonAdmitsBothOrNeither(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var prepares atomic.Int32
	s := New(Options{QueueCapacity: 2, Prepare: func(_ context.Context, req engine.Request) (engine.Request, error) {
		prepares.Add(1)
		if req.ConfigYAML == "invalid" {
			return req, errors.New("invalid configuration")
		}
		return req, nil
	}, Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		started <- struct{}{}
		<-release
		return report(req, runresult.StatusCompleted), nil
	}})
	t.Cleanup(s.Close)
	ctx := context.Background()
	quick := &QuickInput{Fixture: Source{Text: `{"resourceSpans":[]}`}}
	count := func() int {
		runs, err := allRuns(ctx, s, testCaller, RunFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}

	_, err := s.SubmitComparison(ctx, testCaller, ComparisonRequest{Before: Source{Text: "config"}, After: Source{Text: "invalid"}, Mode: ModeQuick, Quick: quick})
	if ErrorCode(err) != CodeValidationFailed || count() != 0 || prepares.Load() != 2 {
		t.Fatalf("invalid after configuration: %v, %d runs, %d preparations", err, count(), prepares.Load())
	}

	first := submit(t, s, request("first"))
	receive(t, started)
	queued := submit(t, s, request("queued"))
	_, err = s.SubmitComparison(ctx, testCaller, ComparisonRequest{Before: Source{Text: "config"}, After: Source{Text: "other"}, Mode: ModeQuick, Quick: quick})
	var e *Error
	if ErrorCode(err) != CodeResourceExhausted || !errors.As(err, &e) || !e.Retryable || count() != 2 {
		t.Fatalf("one free slot: %v, %d runs", err, count())
	}
	close(release)
	waitRun(t, first)
	waitRun(t, queued)

	runs, err := s.SubmitComparison(ctx, testCaller, ComparisonRequest{Before: Source{Text: "config"}, After: Source{Text: "other"}, Mode: ModeQuick, Quick: quick})
	if err != nil || count() != 4 {
		t.Fatalf("free queue: %+v, %v", runs, err)
	}
	if _, err := s.SubmitComparison(ctx, testCaller, ComparisonRequest{Before: Source{Text: "config"}, After: Source{Text: "other"}, Mode: ModeScenario, Quick: quick}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("mode mismatch: %v", err)
	}
}
