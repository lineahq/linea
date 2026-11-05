package analysis

import (
	"maps"
	"slices"
	"testing"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// ends summarizes a record lineage as its delivered exporters and its
// stops, by record name.
func ends(lineage []RecordLineage) map[string][]string {
	out := map[string][]string{}
	for _, r := range lineage {
		var got []string
		for _, d := range r.Deliveries {
			s := "delivered " + d.Exporter + " via " + d.Pipeline
			if d.Changed {
				s += " changed"
			}
			got = append(got, s)
		}
		for _, s := range r.Stops {
			got = append(got, string(s.End)+" at "+s.Component)
		}
		slices.Sort(got)
		out[r.Name] = got
	}
	return out
}

// TestLineageFollowsFanOutDropsAndDeferredStages covers a receiver feeding
// two pipelines: one filters a span and changes another, the other buffers
// everything and flushes later (deferred, reconciled).
func TestLineageFollowsFanOutDropsAndDeferredStages(t *testing.T) {
	in := newTraces([]byte{1, 2, 3})
	filtered := newTraces([]byte{1, 3})
	firstSpan(filtered).Attributes().PutStr("redacted", "yes")

	rec := instrument.NewRecorder()
	put := func(e instrument.Event, td ptrace.Traces) int {
		return rec.RecordTelemetry(e, telemetry.FromTraces(td))
	}
	inj := put(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, in)
	fIn := put(instrument.Event{Kind: instrument.KindIngress, Component: "filter", Pipeline: "traces/a", FromSeq: inj}, in)
	fOut := put(instrument.Event{Kind: instrument.KindEgress, Component: "filter", Pipeline: "traces/a", InputSeq: fIn}, filtered)
	put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/a", FromSeq: fOut}, filtered)
	put(instrument.Event{Kind: instrument.KindIngress, Component: "batch", Pipeline: "traces/b", FromSeq: inj}, in)
	bOut := put(instrument.Event{Kind: instrument.KindEgress, Component: "batch", Pipeline: "traces/b"}, in)
	put(instrument.Event{Kind: instrument.KindExport, Component: "otlp/b", FromSeq: bOut}, in)
	events, snaps := rec.Events(), rec.Snapshots()

	tests := []struct {
		name      string
		completed bool
		want      map[string][]string
	}{
		{"completed", true, map[string][]string{
			"span1": {"delivered otlp/a via traces/a changed", "delivered otlp/b via traces/b"},
			"span2": {"delivered otlp/b via traces/b", "dropped at filter"},
			"span3": {"delivered otlp/a via traces/a", "delivered otlp/b via traces/b"},
		}},
		// Without a clean shutdown the buffered output is not final, so
		// nothing is followed past the batch processor.
		{"incomplete", false, map[string][]string{
			"span1": {"delivered otlp/a via traces/a changed", "unresolved at batch"},
			"span2": {"dropped at filter", "unresolved at batch"},
			"span3": {"delivered otlp/a via traces/a", "unresolved at batch"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stages, err := Analyze(events, snaps, tt.completed)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Lineage(events, snaps, stages, nil, tt.completed)
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range tt.want {
				if !slices.Equal(ends(got)[name], want) {
					t.Errorf("%s ends = %v, want %v", name, ends(got)[name], want)
				}
			}
			span1 := got[0]
			if span1.Span != "span:"+testTrace.String()+":0000000000000001" || span1.ID != "1:resource[0].scope[0].span[0]" {
				t.Errorf("span1 identity = %q, %q", span1.ID, span1.Span)
			}
			var changes []string
			for _, h := range span1.Hops {
				if h.Kind == instrument.KindEgress && h.Component == "filter" {
					if h.Confidence != ConfidenceID {
						t.Errorf("filter output confidence = %q", h.Confidence)
					}
					for _, c := range h.Changes {
						changes = append(changes, c.Path+" "+string(c.Op))
					}
				}
			}
			if !slices.Equal(changes, []string{`attributes["redacted"] added`}) {
				t.Errorf("filter changes on span1 = %v", changes)
			}
		})
	}
}

// TestLineageLeavesUnmatchedRecordsUnresolved: a log record that no output
// record matches is dropped only when no output record is left unmatched
// either; otherwise it may have been rewritten, so nothing is claimed.
func TestLineageLeavesUnmatchedRecordsUnresolved(t *testing.T) {
	logs := func(bodies ...string) plog.Logs {
		ld := plog.NewLogs()
		sl := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
		for _, b := range bodies {
			sl.LogRecords().AppendEmpty().Body().SetStr(b)
		}
		return ld
	}
	run := func(out plog.Logs) map[string][]string {
		rec := instrument.NewRecorder()
		in := logs("keep", "drop me")
		inj := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, telemetry.FromLogs(in))
		sIn := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindIngress, Component: "transform", Pipeline: "logs", FromSeq: inj}, telemetry.FromLogs(in))
		sOut := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindEgress, Component: "transform", Pipeline: "logs", InputSeq: sIn}, telemetry.FromLogs(out))
		rec.RecordTelemetry(instrument.Event{Kind: instrument.KindExport, Component: "debug", FromSeq: sOut}, telemetry.FromLogs(out))
		stages, err := Analyze(rec.Events(), rec.Snapshots(), true)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Lineage(rec.Events(), rec.Snapshots(), stages, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		return ends(got)
	}
	if got := run(logs("keep")); !slices.Equal(got["drop me"], []string{"dropped at transform"}) {
		t.Errorf("removed record ends = %v", got["drop me"])
	}
	if got := run(logs("keep", "rewritten")); !slices.Equal(got["drop me"], []string{"unresolved at transform"}) {
		t.Errorf("rewritten record ends = %v", got["drop me"])
	}
}

// TestLineageEndsRecordsAtAnAggregation: a connector turns three logs into
// one data point. A verified aggregation ends the records it counted
// "aggregated" into the point, which continues as a lineage of its own, and
// the rest "uncounted"; an unverified one attributes nothing, and an input
// the connector emitted nothing for ends dropped.
func TestLineageEndsRecordsAtAnAggregation(t *testing.T) {
	ld := plog.NewLogs()
	sl := ld.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty()
	for _, b := range []string{"a", "b", "c"} {
		sl.LogRecords().AppendEmpty().Body().SetStr(b)
	}
	md := pmetric.NewMetrics()
	m := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("log.count")
	m.SetEmptySum().DataPoints().AppendEmpty().SetIntValue(2)

	run := func(emitted bool, a Aggregation) []RecordLineage {
		rec := instrument.NewRecorder()
		inj := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindInput, Component: "otlp"}, telemetry.FromLogs(ld))
		cIn := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindIngress, Component: "count",
			Source: instrument.SourceConnectorWrapper, FromSeq: inj}, telemetry.FromLogs(ld))
		a.Input = cIn
		if emitted {
			cOut := rec.RecordTelemetry(instrument.Event{Kind: instrument.KindEgress, Component: "count", Pipeline: "metrics",
				Source: instrument.SourceConnectorWrapper, InputSeq: cIn}, telemetry.FromMetrics(md))
			rec.RecordTelemetry(instrument.Event{Kind: instrument.KindExport, Component: "otlp/metrics", FromSeq: cOut}, telemetry.FromMetrics(md))
			a.Output = cOut
		}
		stages, err := Analyze(rec.Events(), rec.Snapshots(), true)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Lineage(rec.Events(), rec.Snapshots(), stages, []*Aggregation{&a}, true)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	point := AggregatedPoint{Ref: "resource[0].scope[0].metric[0].data_point[0]", Metric: "log.count", Value: 2,
		Records: []string{"resource[0].scope[0].log_record[0]", "resource[0].scope[0].log_record[1]"}}

	verified := run(true, Aggregation{Connector: "count", Verification: AggregationVerified, Points: []AggregatedPoint{point}})
	want := map[string][]string{
		"a":         {"aggregated at count"},
		"b":         {"aggregated at count"},
		"c":         {"uncounted at count"},
		"log.count": {"delivered otlp/metrics via metrics"},
	}
	if got := ends(verified); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("verified ends = %v, want %v", got, want)
	}
	derived := verified[len(verified)-1]
	if derived.Origin != OriginAggregated || !slices.Equal(derived.Sources, []string{verified[0].ID, verified[1].ID}) {
		t.Errorf("derived lineage %s: origin %q, sources %v", derived.ID, derived.Origin, derived.Sources)
	}
	if into := verified[0].Hops[len(verified[0].Hops)-1].Into; !slices.Equal(into, []string{derived.ID}) {
		t.Errorf("record a went into %v, want %s", into, derived.ID)
	}

	point.Records = nil
	unverified := run(true, Aggregation{Connector: "count", Verification: AggregationDiffers, Points: []AggregatedPoint{point}})
	want = map[string][]string{"a": {"aggregated at count"}, "b": {"aggregated at count"}, "c": {"aggregated at count"}}
	if got := ends(unverified); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("unverified ends = %v, want %v", got, want)
	}

	failed := run(false, Aggregation{Connector: "count", Verification: AggregationVerified, Notes: []string{"condition failed"}})
	for _, r := range failed {
		h := r.Hops[len(r.Hops)-1]
		if h.End != EndDropped || h.Note != "condition failed" {
			t.Errorf("%s ends %s (%s), want dropped with the aggregation's note", r.Name, h.End, h.Note)
		}
	}
}
