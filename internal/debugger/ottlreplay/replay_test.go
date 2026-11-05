package ottlreplay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pipeline"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/registry"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

var tracesFixture = filepath.Join("..", "..", "..", "testdata", "traces", "traces.json")

func fixture(t *testing.T) telemetry.Payload {
	t.Helper()
	raw, err := os.ReadFile(tracesFixture)
	if err != nil {
		t.Fatal(err)
	}
	p, err := telemetry.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// transformConfig resolves a transform processor block through the real
// Collector config pipeline.
func transformConfig(t *testing.T, block string) component.Config {
	t.Helper()
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"processors:\n  transform:\n" + block +
		"exporters: {nop: {}}\n" +
		"service: {pipelines: {traces: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(context.Background(), path, factories, nil)
	if err != nil {
		t.Fatal(err)
	}
	return resolved.Config.Processors[component.MustNewID("transform")]
}

// runProcessor executes the real transform processor on in's signal and
// returns its output, or nil if it rejected the input.
func runProcessor(t *testing.T, cfg component.Config, in telemetry.Payload) *telemetry.Payload {
	t.Helper()
	ctx, f := context.Background(), transformprocessor.NewFactory()
	set := processortest.NewNopSettings(f.Type())
	var out telemetry.Payload
	switch in.Signal {
	case pipeline.SignalLogs:
		sink := &consumertest.LogsSink{}
		p, err := f.CreateLogs(ctx, set, cfg, sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ConsumeLogs(ctx, in.Clone().Logs); err != nil {
			return nil
		}
		out = telemetry.FromLogs(sink.AllLogs()[0])
	case pipeline.SignalMetrics:
		sink := &consumertest.MetricsSink{}
		p, err := f.CreateMetrics(ctx, set, cfg, sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ConsumeMetrics(ctx, in.Clone().Metrics); err != nil {
			return nil
		}
		out = telemetry.FromMetrics(sink.AllMetrics()[0])
	default:
		sink := &consumertest.TracesSink{}
		p, err := f.CreateTraces(ctx, set, cfg, sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.ConsumeTraces(ctx, in.Clone().Traces); err != nil {
			return nil
		}
		out = telemetry.FromTraces(sink.AllTraces()[0])
	}
	return &out
}

func TestReplayExplainsTraceStatements(t *testing.T) {
	cfg := transformConfig(t, `    error_mode: ignore
    trace_statements:
      - context: resource
        statements:
          - set(attributes["deployment.environment"], "staging")
      - context: span
        statements:
          - set(attributes["http.route"], "/checkout") where name == "POST /checkout"
          - delete_key(attributes, "user.email")
          - set(name, Concat(["failed", name], " ")) where attributes["http.status_code"] >= 500
`)
	in := fixture(t)
	out := runProcessor(t, cfg, in)
	rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationReplayVerified {
		t.Fatalf("verification = %s, mismatch %+v", rep.Verification, rep.Mismatch)
	}
	if len(rep.Groups) != 2 || rep.Groups[0].Status != StatusReplayed || rep.Groups[1].Status != StatusReplayed {
		t.Fatalf("groups = %+v", rep.Groups)
	}

	span := rep.Groups[1].Statements
	tests := []struct {
		name                 string
		got                  Statement
		matched, notMatched  int
		unchanged            int
		changePath, changeOp string
	}{
		{"resource set", rep.Groups[0].Statements[0], 2, 0, 0, `attributes["deployment.environment"]`, "added"},
		{"conditional set", span[0], 1, 2, 0, `attributes["http.route"]`, "added"},
		{"delete_key", span[1], 3, 0, 1, `attributes["user.email"]`, "removed"},
		{"rename on 5xx", span[2], 1, 2, 0, "name", "modified"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.got
			if s.Matched != tt.matched || s.NotMatched != tt.notMatched || s.Unchanged != tt.unchanged || len(s.Errors) != 0 {
				t.Fatalf("stats = %+v", s)
			}
			if len(s.Changes) == 0 || s.Changes[0].Path != tt.changePath || string(s.Changes[0].Op) != tt.changeOp {
				t.Fatalf("changes = %+v", s.Changes)
			}
		})
	}
	// The span-context statements were rewritten with path context names.
	if !strings.HasPrefix(span[1].Effective, "delete_key(span.attributes") {
		t.Errorf("effective statement = %q", span[1].Effective)
	}
}

func TestReplayRespectsErrorMode(t *testing.T) {
	// merge_maps fails on spans whose http.status_code is an int, not a map.
	block := func(mode string) string {
		return "    error_mode: " + mode + "\n" +
			"    trace_statements:\n" +
			"      - context: span\n" +
			"        statements:\n" +
			"          - merge_maps(attributes, attributes[\"http.status_code\"], \"insert\") where attributes[\"http.status_code\"] != nil\n" +
			"          - set(attributes[\"after\"], true)\n"
	}
	tests := []struct {
		mode             string
		wantErrors       int
		wantAfterMatched int
		wantRejected     bool
	}{
		{mode: "ignore", wantErrors: 2, wantAfterMatched: 3},
		{mode: "propagate", wantErrors: 1, wantAfterMatched: 0, wantRejected: true},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg := transformConfig(t, block(tt.mode))
			in := fixture(t)
			out := runProcessor(t, cfg, in)
			if (out == nil) != tt.wantRejected {
				t.Fatalf("processor rejected = %t", out == nil)
			}
			rep, err := Replay(context.Background(), "transform", cfg, 2, in, 0, out)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Verification != VerificationReplayVerified {
				t.Fatalf("verification = %s (%v)", rep.Verification, rep.Notes)
			}
			st := rep.Groups[0].Statements
			if len(st[0].Errors) != tt.wantErrors || st[0].Errors[0].Phase != "function" {
				t.Errorf("errors = %+v", st[0].Errors)
			}
			if st[1].Matched != tt.wantAfterMatched {
				t.Errorf("second statement matched %d, want %d", st[1].Matched, tt.wantAfterMatched)
			}
			if tt.wantRejected && (rep.Error == "" || st[1].NotEvaluated != 1) {
				t.Errorf("propagate: error %q, not evaluated %d", rep.Error, st[1].NotEvaluated)
			}
		})
	}
}

func TestReplayReportsDisagreement(t *testing.T) {
	cfg := transformConfig(t, "    trace_statements: ['set(span.attributes[\"x\"], 1)']\n")
	in := fixture(t)
	// Pretend the processor emitted its input unchanged.
	unchanged := in.Clone()
	rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, &unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationDisagrees || rep.Mismatch == nil || len(rep.Mismatch.Changes) != 3 {
		t.Fatalf("verification = %s, mismatch %+v", rep.Verification, rep.Mismatch)
	}
}

func TestReplayMarksUnsupportedGroups(t *testing.T) {
	cfg := transformConfig(t, `    trace_statements:
      - context: span
        conditions: ['name == "Charge"']
        statements: ['set(attributes["x"], 1)']
      - context: span
        statements: ['set(attributes["y"], 1)']
`)
	in := fixture(t)
	rep, err := Replay(context.Background(), "transform", cfg, 2, in, 3, runProcessor(t, cfg, in))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verification != VerificationNotVerifiable ||
		rep.Groups[0].Status != StatusUnsupported || rep.Groups[1].Status != StatusNotEvaluated {
		t.Fatalf("report = %+v", rep)
	}
}
