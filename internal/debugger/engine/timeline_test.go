package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/assertion"
)

var (
	tailSamplingConfig   = filepath.Join("..", "..", "..", "testdata", "components", "tail_sampling.yaml")
	tailSamplingScenario = filepath.Join("..", "..", "..", "testdata", "components", "tail_sampling_late_span.yaml")
)

// TestTailSamplingFollowsTheScenarioTimeline: with
// drop_pending_traces_on_shutdown a tail_sampling processor emits only what
// it decided on its ticker. The run waits until decision_wait has passed
// (settle), injects the late span after its declared wait, and matches an
// uninstrumented reference Collector that follows the same timeline.
func TestTailSamplingFollowsTheScenarioTimeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: tailSamplingConfig, Scenario: tailSamplingScenario, Compare: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Comparison.Equal {
		t.Fatalf("comparison: %v", rep.Comparison.Differences)
	}
	if rep.Assertions.Outcome != assertion.Pass {
		t.Fatalf("assertions: %+v", rep.Assertions)
	}
	if s := rep.Result.Settle; s == nil || s.Duration != "2.5s" || !strings.Contains(s.Reason, "drop_pending_traces_on_shutdown") {
		t.Fatalf("settle = %+v, want the derived 2.5s", s)
	}
	if s := rep.Reference.Settle; s == nil || s.Duration != "2.5s" {
		t.Fatalf("reference settle = %+v", s)
	}
	if got := rep.Result.Inputs[1].After; got != "3s" {
		t.Fatalf("second input after = %q", got)
	}
	// The root was sampled before the late child arrived: two deliveries.
	if out := rep.Result.Outputs[0]; out.Batches != 2 || out.Counts.Items != 2 {
		t.Fatalf("output: %d batches, %d spans; want 2 and 2", out.Batches, out.Counts.Items)
	}
	for _, l := range rep.Lineage {
		for _, s := range l.Stops {
			if s.End == analysis.EndUnresolved {
				t.Errorf("%s ends unresolved at %s", l.Name, s.Component)
			}
		}
	}
	for _, n := range rep.Topology.Components {
		if n.ID == "tail_sampling" && !strings.Contains(n.Note, "decision_wait (1s)") {
			t.Errorf("topology note = %q", n.Note)
		}
	}
}

// TestDeclaredSettleReplacesTheDerivedWait: a scenario's execution.settle
// is used as declared, even when it is shorter than what the processor
// needs. With the child sent right away, the whole trace is still pending
// at shutdown and dropped, which is what a Collector stopped that early
// does.
func TestDeclaredSettleReplacesTheDerivedWait(t *testing.T) {
	raw, err := os.ReadFile(tailSamplingScenario)
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(raw), "- after: 3s\n    data:", "- data:", 1)
	yaml = strings.Replace(yaml, "assertions:\n", "execution:\n  settle: 100ms\nassertions:\n", 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: tailSamplingConfig, ScenarioYAML: yaml})
	if err != nil {
		t.Fatal(err)
	}
	if s := rep.Result.Settle; s == nil || s.Duration != "100ms" || s.Reason != "execution.settle" {
		t.Fatalf("settle = %+v", s)
	}
	for _, out := range rep.Result.Outputs {
		if out.Counts.Items != 0 {
			t.Fatalf("%s received %d spans, want none", out.Exporter, out.Counts.Items)
		}
	}
	if rep.Assertions.Outcome != assertion.Fail {
		t.Fatalf("assertions = %s, want fail", rep.Assertions.Outcome)
	}
}

// quickTailSamplingFixture is the scenario's first input: a failing root and
// a health check, both pending until decision_wait has passed.
const quickTailSamplingFixture = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[` +
	`{"traceId":"6c9f00a8a9149214e37ac744d2fd71d0","spanId":"fff2a08cd4d2c285","name":"POST /orders","kind":2,"startTimeUnixNano":"1791626401000000000","endTimeUnixNano":"1791626401400000000","status":{"code":2}},` +
	`{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"GET /health","kind":2,"startTimeUnixNano":"1791626400000000000","endTimeUnixNano":"1791626400002000000"}]}]}]}`

// TestQuickRunsFollowTheTimeline: a quick run gets the same settle as a
// scenario: derived for the timer-driven
// processor and matching the reference Collector, or as its setup declares.
func TestQuickRunsFollowTheTimeline(t *testing.T) {
	for _, tt := range []struct {
		name, setup, duration, reason string
		spans                         int
	}{
		{"derived", "", "2.5s", "drop_pending_traces_on_shutdown", 1},
		{"declared", "execution:\n  settle: 100ms\n", "100ms", "execution.settle", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{Config: tailSamplingConfig, FixtureJSON: quickTailSamplingFixture, SetupYAML: tt.setup, Compare: true})
			if err != nil {
				t.Fatal(err)
			}
			if s := rep.Result.Settle; s == nil || s.Duration != tt.duration || !strings.Contains(s.Reason, tt.reason) {
				t.Fatalf("settle = %+v, want %s (%s)", s, tt.duration, tt.reason)
			}
			if s := rep.Reference.Settle; s == nil || s.Duration != tt.duration {
				t.Fatalf("reference settle = %+v", s)
			}
			if !rep.Comparison.Equal {
				t.Fatalf("comparison: %v", rep.Comparison.Differences)
			}
			spans := 0
			for _, out := range rep.Result.Outputs {
				spans += out.Counts.Items
			}
			if spans != tt.spans {
				t.Fatalf("exporters received %d spans, want %d", spans, tt.spans)
			}
		})
	}
}

// TestWaitsLongerThanTheTimeoutAreRefused: a run that could not finish its
// declared waits before execution.timeout fails before it starts.
func TestWaitsLongerThanTheTimeoutAreRefused(t *testing.T) {
	raw, err := os.ReadFile(tailSamplingScenario)
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(raw), "assertions:\n", "execution:\n  timeout: 2s\nassertions:\n", 1)
	start := time.Now()
	_, err = Execute(context.Background(), Request{Config: tailSamplingConfig, ScenarioYAML: yaml})
	if err == nil || !strings.Contains(err.Error(), "raise execution.timeout") {
		t.Fatalf("err = %v, want a refusal naming execution.timeout", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the refusal took %v; it must not wait", d)
	}
}

// TestInputsAfterAWaitLeaveTheBatchSeparately: the batch processor sends
// what it holds when its timeout expires, so an input sent after a longer
// wait arrives in a batch of its own. Without the wait both inputs share
// one batch, flushed at shutdown.
func TestInputsAfterAWaitLeaveTheBatchSeparately(t *testing.T) {
	const config = `receivers:
  otlp:
    protocols: {grpc: {endpoint: 0.0.0.0:4317}}
processors:
  batch: {timeout: 200ms}
exporters:
  otlp/tempo: {endpoint: tempo:4317}
service:
  pipelines:
    traces: {receivers: [otlp], processors: [batch], exporters: [otlp/tempo]}
`
	const span = `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"GET /health"}]}]}]}`
	for _, tt := range []struct {
		after   string
		batches int
	}{{"", 1}, {"after: 1s\n    ", 2}} {
		scn := "version: 1\ninputs:\n  - data: '" + span + "'\n  - " + tt.after + "data: '" + span + "'\n"
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rep, err := Execute(ctx, Request{ConfigYAML: config, ScenarioYAML: scn})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if rep.Result.Settle != nil {
			t.Errorf("settle = %+v, want none for batch", rep.Result.Settle)
		}
		if out := rep.Result.Outputs[0]; out.Batches != tt.batches || out.Counts.Items != 2 {
			t.Errorf("%q: %d batches, %d spans; want %d and 2", tt.after, out.Batches, out.Counts.Items, tt.batches)
		}
	}
}

var (
	wallClockConfig   = filepath.Join("..", "..", "..", "testdata", "components", "wall_clock.yaml")
	wallClockScenario = filepath.Join("..", "..", "..", "testdata", "components", "wall_clock_rebase.yaml")
)

// TestRebasedTimestampsFollowTheRunsClock: a filter dropping logs older
// than an hour drops both 2024 logs as recorded. Rebased, the latest falls
// at the run's start and only the one two hours older is dropped; the
// reference run sees the same shifted inputs.
func TestRebasedTimestampsFollowTheRunsClock(t *testing.T) {
	raw, err := os.ReadFile(wallClockScenario)
	if err != nil {
		t.Fatal(err)
	}
	recorded := strings.Replace(string(raw), "timestamps: rebase", "timestamps: recorded", 1)
	for _, tt := range []struct {
		name, scenario, mode string
		kept                 int
	}{
		{"recorded", recorded, TimestampsRecorded, 0},
		{"rebased", string(raw), TimestampsRebased, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			rep, err := Execute(context.Background(), Request{Config: wallClockConfig, ScenarioYAML: tt.scenario, Compare: true})
			end := time.Now()
			if err != nil {
				t.Fatal(err)
			}
			if !rep.Comparison.Equal {
				t.Fatalf("comparison: %v", rep.Comparison.Differences)
			}
			kept := 0
			for _, o := range rep.Result.Outputs {
				kept += o.Counts.Items
			}
			if kept != tt.kept {
				t.Fatalf("%d logs kept, want %d", kept, tt.kept)
			}
			ts := rep.Timestamps
			if ts == nil || ts.Mode != tt.mode {
				t.Fatalf("timestamps = %+v", ts)
			}
			if want := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC); ts.LatestInput == nil || !ts.LatestInput.Equal(want) {
				t.Fatalf("latest input = %v, want %v", ts.LatestInput, want)
			}
			offset, err := time.ParseDuration(ts.Offset)
			// The engine reads its clock during the run and truncates the
			// offset to whole seconds.
			if err != nil || offset > end.Sub(*ts.LatestInput) || offset < start.Sub(*ts.LatestInput)-2*time.Second {
				t.Fatalf("offset = %q, want about %v", ts.Offset, start.Sub(*ts.LatestInput))
			}
			if len(ts.WallClock) != 1 || ts.WallClock[0].Component != "processor/filter/stale" || !strings.Contains(ts.WallClock[0].Expression, "Now()") {
				t.Fatalf("wall clock = %+v", ts.WallClock)
			}
		})
	}
}

// TestNormalizedRebasedReportsAreIdentical: normalization undoes the shift,
// so the outputs carry the recorded timestamps and two runs at different
// times give the same bytes.
func TestNormalizedRebasedReportsAreIdentical(t *testing.T) {
	var reports [2][]byte
	for i := range reports {
		rep, err := Execute(context.Background(), Request{Config: wallClockConfig, Scenario: wallClockScenario, Normalize: true})
		if err != nil {
			t.Fatal(err)
		}
		if rep.Timestamps.Offset != "" {
			t.Fatalf("normalized offset = %q", rep.Timestamps.Offset)
		}
		if out := string(rep.Result.Outputs[0].Data); !strings.Contains(out, `"timeUnixNano":"1717236000000000000"`) {
			t.Fatalf("output keeps the shifted time: %s", out)
		}
		if reports[i], err = json.Marshal(rep); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // a different whole-second offset
	}
	if !bytes.Equal(reports[0], reports[1]) {
		t.Fatal("normalized reports of a rebased scenario differ")
	}
}

// TestInspectWarnsAboutWallClockReads: Now() in a statement is reported
// unless the scenario rebases the input timestamps.
func TestInspectWarnsAboutWallClockReads(t *testing.T) {
	for _, tt := range []struct {
		name     string
		scenario string
		want     int
	}{
		{"no scenario", "", 1},
		{"rebased", wallClockScenario, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ins, err := Inspect(context.Background(), InspectRequest{Config: wallClockConfig, Scenario: tt.scenario})
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, d := range ins.Diagnostics {
				if d.Code == CodeWallClock && d.Component == "processor/filter/stale" {
					n++
				}
			}
			if n != tt.want {
				t.Fatalf("%d wall_clock diagnostics, want %d: %+v", n, tt.want, ins.Diagnostics)
			}
		})
	}
}
