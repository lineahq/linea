package instrument

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/batchprocessor"
	"go.opentelemetry.io/collector/processor/processortest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/attributesprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/filterprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/groupbyattrsprocessor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor"

	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// objectLogs returns three logs sharing one timestamp and body, the second
// marked to be dropped.
func objectLogs(first int) plog.Logs {
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	lrs := rl.ScopeLogs().AppendEmpty().LogRecords()
	for i := range 3 {
		lr := lrs.AppendEmpty()
		lr.SetTimestamp(1760000000000000000)
		lr.Body().SetStr("request")
		lr.Attributes().PutInt("enduser.id", int64(first+i))
		lr.Attributes().PutStr("host", []string{"a", "b"}[i%2])
		if i == 1 {
			lr.Attributes().PutBool("drop", true)
		}
	}
	return ld
}

// stageObjects drives two inputs of objectLogs through a decorated processor
// and returns the object numbers of its inputs and of its outputs.
func stageObjects(t *testing.T, f processor.Factory, conf map[string]any) (in, out []int) {
	t.Helper()
	ctx := context.Background()
	cfg := f.CreateDefaultConfig()
	if err := confmap.NewFromStringMap(conf).Unmarshal(cfg); err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder()
	p, err := Processor(f, rec).CreateLogs(ctx, processortest.NewNopSettings(f.Type()), cfg, new(consumertest.LogsSink))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(ctx, componenttest.NewNopHost()); err != nil {
		t.Fatal(err)
	}
	for _, first := range []int{0, 3} {
		if err := p.ConsumeLogs(ctx, objectLogs(first)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	for _, e := range rec.Events() {
		if e.Snapshot != "" && len(e.Objects) != e.Counts.Items {
			t.Fatalf("event %d: %d objects for %d records", e.Seq, len(e.Objects), e.Counts.Items)
		}
		switch e.Kind {
		case KindIngress:
			in = append(in, e.Objects...)
		case KindEgress:
			out = append(out, e.Objects...)
		}
	}
	if err := rec.Err(); err != nil {
		t.Fatal(err)
	}
	return in, out
}

func TestStagesInPlaceKeepRecordObjects(t *testing.T) {
	all := []int{1, 2, 3, 4, 5, 6}
	tests := []struct {
		name    string
		factory processor.Factory
		conf    map[string]any
		want    []int
	}{
		{"attributes", attributesprocessor.NewFactory(), map[string]any{
			"actions": []any{map[string]any{"key": "enduser.id", "action": "hash"}},
		}, all},
		{"transform", transformprocessor.NewFactory(), map[string]any{
			"log_statements": []any{`set(log.body, "handled")`},
		}, all},
		{"filter", filterprocessor.NewFactory(), map[string]any{
			"logs": map[string]any{"log_record": []any{`attributes["drop"] == true`}},
		}, []int{1, 3, 4, 6}},
		// batch merges both inputs into one output at shutdown.
		{"batch", batchprocessor.NewFactory(), map[string]any{
			"timeout": "1h", "send_batch_size": 100,
		}, all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, out := stageObjects(t, tt.factory, tt.conf)
			if !slices.Equal(in, all) {
				t.Fatalf("input objects = %v, want %v", in, all)
			}
			if !slices.Equal(out, tt.want) {
				t.Fatalf("output objects = %v, want %v", out, tt.want)
			}
		})
	}
}

func TestCopyingStageGivesNewObjects(t *testing.T) {
	in, out := stageObjects(t, groupbyattrsprocessor.NewFactory(), map[string]any{"keys": []any{"host"}})
	if len(out) != 6 {
		t.Fatalf("output objects = %v", out)
	}
	for _, n := range out {
		if slices.Contains(in, n) {
			t.Fatalf("groupbyattrs copies records, but output object %d is an input's (in %v, out %v)", n, in, out)
		}
	}
}

func TestRecorderNumbersOnlyKeptEvents(t *testing.T) {
	rec := NewRecorder()
	rec.SetBudget(NewBudget(1, 0))
	if seq := rec.RecordTelemetry(Event{Kind: KindIngress}, telemetry.FromLogs(objectLogs(0))); seq != 0 {
		t.Fatalf("event kept past the budget: %d", seq)
	}
	rec.SetBudget(nil)
	ld := objectLogs(0)
	rec.RecordTelemetry(Event{Kind: KindIngress}, telemetry.FromLogs(ld))
	rec.RecordTelemetry(Event{Kind: KindEgress}, telemetry.FromLogs(ld))
	events := rec.Events()
	if len(events) != 2 || !slices.Equal(events[0].Objects, []int{1, 2, 3}) || !slices.Equal(events[1].Objects, []int{1, 2, 3}) {
		t.Fatalf("events = %+v", events)
	}

	rec.ReleaseObjects()
	rec.RecordTelemetry(Event{Kind: KindEgress}, telemetry.FromLogs(ld))
	if last := rec.Events()[2]; last.Objects != nil {
		t.Fatalf("objects after release = %v", last.Objects)
	}
}

func TestEventsWithoutObjectsDecode(t *testing.T) {
	var e Event
	old := `{"id":"run-1:2","run_id":"run-1","seq":2,"time":"2026-10-01T00:00:00Z","kind":"stage.input",` +
		`"component":"transform","signal":"logs","source":"processor_wrapper","counts":{"resources":1,"scopes":1,"items":1},"snapshot":"sha256:00"}`
	if err := json.Unmarshal([]byte(old), &e); err != nil {
		t.Fatal(err)
	}
	if e.Objects != nil || e.Seq != 2 || e.Snapshot != "sha256:00" {
		t.Fatalf("decoded %+v", e)
	}
	data, err := json.Marshal(Event{Kind: KindRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["objects"]; ok {
		t.Fatalf("event without records encodes objects: %s", data)
	}
}
