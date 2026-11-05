package instrument

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func transformConfig(t *testing.T, f processor.Factory) component.Config {
	t.Helper()
	cfg := f.CreateDefaultConfig()
	conf := confmap.NewFromStringMap(map[string]any{
		"trace_statements": []any{`set(span.attributes["stage"], "after")`},
	})
	if err := conf.Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newTraces() ptrace.Traces {
	td := ptrace.NewTraces()
	span := td.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	span.SetName("op")
	span.Attributes().PutStr("stage", "before")
	return td
}

// run drives one batch through a processor created from f and returns what
// reached the next consumer, plus the processor's reported capabilities.
func run(t *testing.T, f processor.Factory) (ptrace.Traces, bool) {
	t.Helper()
	ctx := context.Background()
	sink := new(consumertest.TracesSink)
	p, err := f.CreateTraces(ctx, processortest.NewNopSettings(f.Type()), transformConfig(t, f), sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	if err := p.ConsumeTraces(ctx, newTraces()); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.AllTraces()) != 1 {
		t.Fatalf("got %d batches, want 1", len(sink.AllTraces()))
	}
	return sink.AllTraces()[0], p.Capabilities().MutatesData
}

func TestProcessorPreservesTransformSemantics(t *testing.T) {
	real := transformprocessor.NewFactory()
	rec := NewRecorder()
	decorated := Processor(real, rec)

	if decorated.Type() != real.Type() || decorated.TracesStability() != real.TracesStability() ||
		decorated.MetricsStability() != real.MetricsStability() || decorated.LogsStability() != real.LogsStability() {
		t.Fatal("decorated factory metadata differs from the real factory")
	}

	want, wantMutates := run(t, real)
	got, gotMutates := run(t, decorated)

	if gotMutates != wantMutates {
		t.Fatalf("MutatesData = %v, real processor reports %v", gotMutates, wantMutates)
	}
	if mustJSON(t, telemetry.FromTraces(got)) != mustJSON(t, telemetry.FromTraces(want)) {
		t.Fatal("decorated output differs from the real processor output")
	}

	events := rec.Events()
	kinds := make([]Kind, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	if !slices.Equal(kinds, []Kind{KindComponentStarted, KindIngress, KindEgress, KindComponentStopped}) {
		t.Fatalf("events = %+v", events)
	}
	snaps := rec.Snapshots()
	for i, wantStage := range []string{"before", "after"} {
		e := events[i+1]
		if e.Component != "transform" || e.Mode != ModeReal || e.RunID != rec.RunID() {
			t.Errorf("event %+v", e)
		}
		if !strings.Contains(string(snaps[e.Snapshot]), `"stringValue":"`+wantStage+`"`) {
			t.Errorf("event %d snapshot lacks stage %q: %s", e.Seq, wantStage, snaps[e.Snapshot])
		}
	}
	if events[2].InputSeq != events[1].Seq {
		t.Errorf("egress input_seq = %d, want %d", events[2].InputSeq, events[1].Seq)
	}
	if err := rec.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderSnapshotsAreImmutable(t *testing.T) {
	rec := NewRecorder()
	td := newTraces()
	rec.RecordTelemetry(Event{Kind: KindInput, Component: "fixture"}, telemetry.FromTraces(td))
	td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).SetName("mutated")

	if strings.Contains(string(rec.Snapshots()[rec.Events()[0].Snapshot]), "mutated") {
		t.Fatal("snapshot reflects mutation made after recording")
	}
}

func TestRecorderStoresIdenticalSnapshotsOnce(t *testing.T) {
	rec := NewRecorder()
	rec.RecordTelemetry(Event{Kind: KindEgress, Component: "a"}, telemetry.FromTraces(newTraces()))
	rec.RecordTelemetry(Event{Kind: KindIngress, Component: "b"}, telemetry.FromTraces(newTraces()))
	events := rec.Events()
	if events[0].Snapshot != events[1].Snapshot || len(rec.Snapshots()) != 1 {
		t.Fatalf("events %+v, %d snapshots", events, len(rec.Snapshots()))
	}
}

// TestProcessorRecordsOwnErrorsOnly: an OTTL error with error_mode
// propagate is the stage's error; an error returned by the next consumer is
// not, although the processor passes it up.
func TestProcessorRecordsOwnErrorsOnly(t *testing.T) {
	downstream := errors.New("exporter unavailable")
	tests := []struct {
		name      string
		statement string
		next      consumer.Traces
		wantError bool
	}{
		{"own error", `merge_maps(span.attributes, span.attributes["stage"], "insert")`, consumertest.NewNop(), true},
		{"downstream error", `set(span.attributes["x"], "y")`, consumertest.NewErr(downstream), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := transformprocessor.NewFactory()
			cfg := f.CreateDefaultConfig()
			conf := confmap.NewFromStringMap(map[string]any{
				"error_mode":       "propagate",
				"trace_statements": []any{tt.statement},
			})
			if err := conf.Unmarshal(cfg); err != nil {
				t.Fatal(err)
			}
			rec := NewRecorder()
			p, err := Processor(f, rec).CreateTraces(context.Background(), processortest.NewNopSettings(f.Type()), cfg, tt.next)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ConsumeTraces(context.Background(), newTraces()); err == nil {
				t.Fatal("ConsumeTraces returned no error")
			}
			var stageErrors []Event
			for _, e := range rec.Events() {
				if e.Kind == KindStageError {
					stageErrors = append(stageErrors, e)
				}
			}
			if got := len(stageErrors) == 1; got != tt.wantError {
				t.Fatalf("stage errors = %+v, want error recorded: %v", stageErrors, tt.wantError)
			}
			if tt.wantError && stageErrors[0].InputSeq != 1 {
				t.Fatalf("stage error not linked to its input: %+v", stageErrors[0])
			}
		})
	}
}

func TestPipelineOfReadsComponentLoggerAttributes(t *testing.T) {
	root := zap.NewNop().WithOptions(zap.WrapCore(WrapCore))
	tests := []struct {
		name   string
		logger *zap.Logger
		want   string
	}{
		{"plain field", root.With(zap.String("otelcol.pipeline.id", "logs/a")), "logs/a"},
		{"inline object", root.With(zap.Inline(attrs{"otelcol.component.id": "batch", "otelcol.pipeline.id": "traces"})), "traces"},
		{"later fields keep it", root.With(zap.String("otelcol.pipeline.id", "metrics")).With(zap.String("k", "v")), "metrics"},
		{"no attribute", root, ""},
		{"logger without probe core", zap.NewNop().With(zap.String("otelcol.pipeline.id", "traces")), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pipelineOf(tt.logger); got != tt.want {
				t.Fatalf("pipelineOf = %q, want %q", got, tt.want)
			}
		})
	}
}

// attrs mimics the inline field the Collector's componentattribute logger adds.
type attrs map[string]string

func (a attrs) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	for k, v := range a {
		enc.AddString(k, v)
	}
	return nil
}

func mustJSON(t *testing.T, p telemetry.Payload) string {
	t.Helper()
	b, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
