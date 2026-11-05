package analysis

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// TestEndpointRecordsArePairedByContent: what a simulated endpoint
// received is paired with the lineage whose record it is, exactly when
// identical and by the stage diffs' content keys when the component
// changed it; a record no lineage has, or several have, is not paired.
func TestEndpointRecordsArePairedByContent(t *testing.T) {
	in := newLogs(logSpec{time: 1, body: "a"}, logSpec{time: 2, body: "b"}, logSpec{time: 3, body: "twin"}, logSpec{time: 3, body: "twin"})
	sent := newLogs(logSpec{time: 1, body: "a"}, logSpec{time: 2, body: "b", attrs: map[string]any{"capture.session": "s1"}},
		logSpec{time: 9, body: "unknown"}, logSpec{time: 3, body: "twin"})

	rec := instrument.NewRecorder()
	put := func(e instrument.Event, ld plog.Logs) int {
		return rec.RecordTelemetry(e, telemetry.FromLogs(ld))
	}
	inj := put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, in)
	tIn := put(instrument.Event{Kind: instrument.KindIngress, Component: "tap", Pipeline: "logs", FromSeq: inj}, in)
	tOut := put(instrument.Event{Kind: instrument.KindEgress, Component: "tap", Pipeline: "logs", InputSeq: tIn}, in)
	put(instrument.Event{Kind: instrument.KindExport, Component: "debug", FromSeq: tOut}, in)
	recv := put(instrument.Event{Kind: instrument.KindEndpointReceived, Component: "forwarder", ComponentKind: "extension",
		Endpoint: "delivery", Source: instrument.SourceEndpointSink}, sent)
	events, snaps := rec.Events(), rec.Snapshots()

	stages, err := Analyze(events, snaps, true)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := Lineage(events, snaps, stages, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]Sent{}
	for _, l := range lineage {
		got[l.ID] = append(got[l.ID], l.Sent...)
	}
	want := map[string][]Sent{
		"1:resource[0].scope[0].log_record[0]": {{Component: "forwarder", Endpoint: "delivery", Seq: recv,
			Ref: "resource[0].scope[0].log_record[0]", Confidence: ConfidenceExact}},
		"1:resource[0].scope[0].log_record[1]": {{Component: "forwarder", Endpoint: "delivery", Seq: recv,
			Ref: "resource[0].scope[0].log_record[1]", Confidence: ConfidenceHigh}},
		"1:resource[0].scope[0].log_record[2]": nil,
		"1:resource[0].scope[0].log_record[3]": nil,
	}
	if len(got) != len(want) {
		t.Fatalf("lineages %v", got)
	}
	for id, w := range want {
		g := got[id]
		if len(g) != len(w) || (len(w) == 1 && g[0] != w[0]) {
			t.Errorf("%s sent %+v, want %+v", id, g, w)
		}
	}
	// The endpoint event is not a stage: stages are unchanged by it.
	for _, s := range stages {
		if s.Component == "forwarder" {
			t.Errorf("endpoint event made a stage %+v", s)
		}
	}
}
