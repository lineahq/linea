package analysis

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// objectRun is a recorded run whose events carry chosen record object
// numbers.
type objectRun struct {
	t       *testing.T
	rec     *instrument.Recorder
	objects map[int][]int
}

func newObjectRun(t *testing.T) *objectRun {
	return &objectRun{t: t, rec: instrument.NewRecorder(), objects: map[int][]int{}}
}

// put records e with ld and gives its records the object numbers objects.
func (r *objectRun) put(e instrument.Event, ld plog.Logs, objects ...int) int {
	r.t.Helper()
	if e.Pipeline == "" && e.Kind != instrument.KindInput && e.Kind != instrument.KindExport {
		e.Pipeline = "logs"
	}
	seq := r.rec.RecordTelemetry(e, telemetry.FromLogs(ld))
	if seq == 0 {
		r.t.Fatal("event not recorded")
	}
	r.objects[seq] = objects
	return seq
}

// events returns the recorded events, with their chosen object numbers or,
// when strip is set, none (as recorded before event schema 0.5).
func (r *objectRun) events(strip bool) ([]instrument.Event, instrument.Snapshots) {
	events := r.rec.Events()
	for i := range events {
		events[i].Objects = nil
		if !strip {
			events[i].Objects = r.objects[events[i].Seq]
		}
	}
	return events, r.rec.Snapshots()
}

// sharedLogs returns n logs sharing their timestamp and body, told apart
// only by enduser.id, which hashed replaces.
func sharedLogs(n int, hashed bool, keep ...int) plog.Logs {
	var specs []logSpec
	for i := range n {
		if keep != nil && !containsInt(keep, i) {
			continue
		}
		id := any(int64(i))
		if hashed {
			id = "h" + string(rune('a'+i))
		}
		specs = append(specs, logSpec{time: 1760000000000000000, body: "request", attrs: map[string]any{"enduser.id": id}})
	}
	return newLogs(specs...)
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// objectFixtures are runs with chosen object numbers, most of whose records
// share their content:
//   - hash: a stage changes every record of four sharing their content;
//   - filter: a stage removes one of three and changes the others;
//   - batch: a stage buffers two inputs and emits them changed, later;
//   - pass: a stage passes shared records on unchanged;
//   - copy: a stage copies distinct records (new objects).
func objectFixtures(t *testing.T) map[string]*objectRun {
	out := map[string]*objectRun{}

	r := newObjectRun(t)
	inj := r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, sharedLogs(4, false), 1, 2, 3, 4)
	in := r.put(instrument.Event{Kind: instrument.KindIngress, Component: "attributes/hash", FromSeq: inj}, sharedLogs(4, false), 1, 2, 3, 4)
	o := r.put(instrument.Event{Kind: instrument.KindEgress, Component: "attributes/hash", InputSeq: in}, sharedLogs(4, true), 1, 2, 3, 4)
	r.put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/backend", FromSeq: o}, sharedLogs(4, true), 1, 2, 3, 4)
	out["hash"] = r

	r = newObjectRun(t)
	inj = r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, sharedLogs(3, false), 1, 2, 3)
	in = r.put(instrument.Event{Kind: instrument.KindIngress, Component: "transform/drop", FromSeq: inj}, sharedLogs(3, false), 1, 2, 3)
	o = r.put(instrument.Event{Kind: instrument.KindEgress, Component: "transform/drop", InputSeq: in}, sharedLogs(3, true, 0, 2), 1, 3)
	r.put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/backend", FromSeq: o}, sharedLogs(3, true, 0, 2), 1, 3)
	out["filter"] = r

	r = newObjectRun(t)
	inj = r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, sharedLogs(2, false), 1, 2)
	r.put(instrument.Event{Kind: instrument.KindIngress, Component: "batch", FromSeq: inj}, sharedLogs(2, false), 1, 2)
	inj = r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, sharedLogs(2, false), 3, 4)
	r.put(instrument.Event{Kind: instrument.KindIngress, Component: "batch", FromSeq: inj}, sharedLogs(2, false), 3, 4)
	merged := sharedLogs(2, true)
	sharedLogs(2, true).ResourceLogs().MoveAndAppendTo(merged.ResourceLogs())
	o = r.put(instrument.Event{Kind: instrument.KindEgress, Component: "batch"}, merged, 1, 2, 3, 4)
	r.put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/backend", FromSeq: o}, merged, 1, 2, 3, 4)
	out["batch"] = r

	r = newObjectRun(t)
	inj = r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, sharedLogs(2, false), 1, 2)
	in = r.put(instrument.Event{Kind: instrument.KindIngress, Component: "filter/none", FromSeq: inj}, sharedLogs(2, false), 1, 2)
	o = r.put(instrument.Event{Kind: instrument.KindEgress, Component: "filter/none", InputSeq: in}, sharedLogs(2, false), 1, 2)
	r.put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/backend", FromSeq: o}, sharedLogs(2, false), 1, 2)
	out["pass"] = r

	r = newObjectRun(t)
	distinct := newLogs(logSpec{time: 1, body: "a", attrs: map[string]any{"k": "v"}}, logSpec{time: 2, body: "b", attrs: map[string]any{"k": "v"}})
	copied := newLogs(logSpec{time: 1, body: "a", attrs: map[string]any{"k": "w"}}, logSpec{time: 2, body: "b", attrs: map[string]any{"k": "v"}})
	inj = r.put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, distinct, 1, 2)
	in = r.put(instrument.Event{Kind: instrument.KindIngress, Component: "groupbyattrs", FromSeq: inj}, distinct, 1, 2)
	o = r.put(instrument.Event{Kind: instrument.KindEgress, Component: "groupbyattrs", InputSeq: in}, copied, 3, 4)
	r.put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/backend", FromSeq: o}, copied, 3, 4)
	out["copy"] = r
	return out
}
