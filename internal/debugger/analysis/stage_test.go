package analysis

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// record appends a telemetry event of pipeline "traces" and checks its seq.
func record(t *testing.T, rec *instrument.Recorder, seq int, kind instrument.Kind, component string, inputSeq int, td ptrace.Traces) {
	t.Helper()
	e := instrument.Event{Kind: kind, Component: component, InputSeq: inputSeq}
	if kind != instrument.KindInput {
		e.Pipeline = "traces"
	}
	if got := rec.RecordTelemetry(e, telemetry.FromTraces(td)); got != seq {
		t.Fatalf("seq = %d, want %d", got, seq)
	}
}

func TestAnalyzeClassifiesStageOutcomes(t *testing.T) {
	in := newTraces([]byte{1, 2})
	kept := newTraces([]byte{1})
	rec := instrument.NewRecorder()
	record(t, rec, 1, instrument.KindInput, "otlp", 0, in)
	record(t, rec, 2, instrument.KindIngress, "filter", 0, in)
	record(t, rec, 3, instrument.KindEgress, "filter", 2, kept)
	record(t, rec, 4, instrument.KindIngress, "batch", 0, kept)
	record(t, rec, 5, instrument.KindIngress, "filter/all", 0, in)
	record(t, rec, 6, instrument.KindEgress, "batch", 0, kept)
	events, snaps := rec.Events(), rec.Snapshots()

	tests := []struct {
		name      string
		completed bool
		want      map[string]Outcome
	}{
		{
			name:      "completed run",
			completed: true,
			want:      map[string]Outcome{"filter": OutcomeEmitted, "batch": OutcomeDeferred, "filter/all": OutcomeNoOutput},
		},
		{
			name:      "incomplete run",
			completed: false,
			want:      map[string]Outcome{"filter": OutcomeEmitted, "batch": OutcomeUnresolved, "filter/all": OutcomeUnresolved},
		},
	}
	emission := map[string]Emission{"filter": EmissionSynchronous, "batch": EmissionAsynchronous, "filter/all": EmissionNone}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stages, err := Analyze(events, snaps, tt.completed)
			if err != nil {
				t.Fatal(err)
			}
			if len(stages) != 3 {
				t.Fatalf("got %d stages", len(stages))
			}
			for _, s := range stages {
				if len(s.Inputs) != 1 || s.Inputs[0].Outcome != tt.want[s.Component] || s.Emission != emission[s.Component] {
					t.Errorf("%s: emission %s, inputs %+v", s.Component, s.Emission, s.Inputs)
				}
				switch s.Component {
				case "filter":
					if d := s.Inputs[0].Diff; d == nil || len(d.Removed) != 1 || d.Removed[0].Name != "span2" {
						t.Errorf("filter diff = %+v, want span2 removed", d)
					}
				case "batch":
					if r := s.Reconciliation; tt.completed && (r == nil || !r.Empty()) {
						t.Errorf("batch reconciliation = %+v, want empty diff", r)
					}
					if !tt.completed && s.Reconciliation != nil {
						t.Error("reconciliation claimed for incomplete run")
					}
				case "filter/all":
					if d := s.Inputs[0].Diff; tt.completed && (d == nil || len(d.Removed) != 2) {
						t.Errorf("no-output diff = %+v, want both spans removed", d)
					}
				}
			}
		})
	}
}

// TestAnalyzeSeparatesPipelinesAndAttachesErrors: one processor ID in two
// pipelines is two stages, and a stage.error is attached to its input.
func TestAnalyzeSeparatesPipelinesAndAttachesErrors(t *testing.T) {
	td := newTraces([]byte{1})
	rec := instrument.NewRecorder()
	for _, pipeline := range []string{"traces/a", "traces/b"} {
		seq := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindIngress, Component: "transform", Pipeline: pipeline}, telemetry.FromTraces(td))
		if pipeline == "traces/b" {
			rec.Record(instrument.Event{Kind: instrument.KindStageError, Component: "transform", Pipeline: pipeline, Signal: "traces", InputSeq: seq, Error: "boom"})
			continue
		}
		rec.RecordTelemetry(instrument.Event{Kind: instrument.KindEgress, Component: "transform", Pipeline: pipeline, InputSeq: seq}, telemetry.FromTraces(td))
	}
	stages, err := Analyze(rec.Events(), rec.Snapshots(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 {
		t.Fatalf("stages = %+v", stages)
	}
	a, b := stages[0], stages[1]
	if a.Pipeline != "traces/a" || a.Inputs[0].Outcome != OutcomeEmitted || a.Inputs[0].Error != "" {
		t.Errorf("traces/a stage = %+v", a)
	}
	if b.Pipeline != "traces/b" || b.Inputs[0].Outcome != OutcomeNoOutput || b.Inputs[0].Error != "boom" {
		t.Errorf("traces/b stage = %+v", b)
	}
}
