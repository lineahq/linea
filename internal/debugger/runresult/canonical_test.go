package runresult

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

func TestCanonicalizeOrdersShutdownByPipeline(t *testing.T) {
	type ev struct {
		kind              instrument.Kind
		component, signal string
		pipeline          string
		inputSeq          int
		objects           []int
	}
	build := func(evs []ev) *Result {
		r := &Result{}
		for i, e := range evs {
			r.Events = append(r.Events, instrument.Event{
				ID: fmt.Sprintf("r:%d", i+1), RunID: "r", Seq: i + 1, Kind: e.kind,
				Component: e.component, Signal: e.signal, Pipeline: e.pipeline, InputSeq: e.inputSeq,
				Objects: e.objects,
			})
		}
		return r
	}
	start := []ev{
		{instrument.KindRunStarted, "", "", "", 0, nil},
		{instrument.KindIngress, "batch", "traces", "traces", 0, nil},
		{instrument.KindIngress, "batch", "logs", "logs", 0, nil},
	}
	// Each pipeline stops resource, then batch flushes from Shutdown into the
	// exporter and stops. The flushed records are numbered when observed.
	stop := func(signal string, ingress int, objects []int) []ev {
		return []ev{
			{instrument.KindComponentStopped, "resource", signal, signal, 0, nil},
			{instrument.KindEgress, "batch", signal, signal, ingress, objects},
			{instrument.KindExport, "otlp", signal, "", 0, objects},
			{instrument.KindComponentStopped, "batch", signal, signal, 0, nil},
		}
	}
	end := ev{instrument.KindRunCompleted, "", "", "", 0, nil}
	// Two observed shutdown orders of the same run.
	tracesFirst := slices.Concat(start, stop("traces", 2, []int{1, 2}), stop("logs", 3, []int{3}), []ev{end})
	logsFirst := slices.Concat(start, stop("logs", 3, []int{1}), stop("traces", 2, []int{2, 3}), []ev{end})

	a, b := build(tracesFirst), build(logsFirst)
	a.Canonicalize()
	b.Canonicalize()
	if !reflect.DeepEqual(a.Events, b.Events) {
		t.Fatalf("canonical orders differ:\n%v\n%v", a.Events, b.Events)
	}
	if got := a.Events[4].Objects; !slices.Equal(got, []int{1}) {
		t.Errorf("first flushed objects = %v, want [1]", got)
	}
	var got []string
	for _, e := range a.Events {
		got = append(got, fmt.Sprintf("%d %s %s %d", e.Seq, e.Kind, e.Signal, e.InputSeq))
	}
	want := []string{
		"1 run.started  0",
		"2 stage.input traces 0",
		"3 stage.input logs 0",
		"4 component.stopped logs 0",
		"5 stage.output logs 3",
		"6 exporter.captured logs 0",
		"7 component.stopped logs 0",
		"8 component.stopped traces 0",
		"9 stage.output traces 2",
		"10 exporter.captured traces 0",
		"11 component.stopped traces 0",
		"12 run.completed  0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", got, want)
	}
	if a.Events[8].ID != "r:9" {
		t.Errorf("ID = %s, want r:9", a.Events[8].ID)
	}
}

func TestCanonicalizeSortsOutputResources(t *testing.T) {
	a := &Result{Outputs: []Output{logsOutput(t, "b", "a")}}
	b := &Result{Outputs: []Output{logsOutput(t, "a", "b")}}
	a.Canonicalize()
	b.Canonicalize()
	if string(a.Outputs[0].Data) != string(b.Outputs[0].Data) {
		t.Fatalf("outputs differ after canonicalization:\n%s\n%s", a.Outputs[0].Data, b.Outputs[0].Data)
	}
}

// TestCanonicalizeKeepsConnectorDeliveriesFirst: pipelines a connector feeds
// sort after the pipeline that feeds it, whatever their names, so every
// delivery still precedes what the receiving pipeline did with it.
func TestCanonicalizeKeepsConnectorDeliveriesFirst(t *testing.T) {
	type ev struct {
		kind                instrument.Kind
		component, pipeline string
		connector           bool
		inputSeq, fromSeq   int
	}
	build := func(evs []ev) *Result {
		r := &Result{}
		for i, e := range evs {
			src := instrument.SourceProcessorWrapper
			if e.connector {
				src = instrument.SourceConnectorWrapper
			}
			r.Events = append(r.Events, instrument.Event{
				ID: fmt.Sprintf("r:%d", i+1), RunID: "r", Seq: i + 1, Kind: e.kind, Source: src,
				Component: e.component, Signal: "logs", Pipeline: e.pipeline, InputSeq: e.inputSeq, FromSeq: e.fromSeq,
			})
		}
		return r
	}
	// logs/z feeds the routing connector, which delivers into logs/a and
	// logs/b; the observed delivery order varies. at is the delivery's seq.
	head := []ev{
		{instrument.KindInput, "otlp", "", false, 0, 0},
		{instrument.KindEgress, "resource", "logs/z", false, 0, 0},
		{instrument.KindIngress, "routing", "", true, 0, 2},
	}
	deliver := func(pipeline string, at int) []ev {
		return []ev{
			{instrument.KindEgress, "routing", pipeline, true, 3, 0},
			{instrument.KindIngress, "filter", pipeline, false, 0, at},
			{instrument.KindEgress, "filter", pipeline, false, at + 1, 0},
			{instrument.KindExport, "otlp/" + pipeline, "", false, 0, at + 2},
		}
	}
	aFirst := slices.Concat(head, deliver("logs/a", 4), deliver("logs/b", 8))
	bFirst := slices.Concat(head, deliver("logs/b", 4), deliver("logs/a", 8))

	a, b := build(aFirst), build(bFirst)
	a.Canonicalize()
	b.Canonicalize()
	var got []string
	for _, e := range a.Events {
		got = append(got, fmt.Sprintf("%d %s %s %s %d %d", e.Seq, e.Kind, e.Component, e.Pipeline, e.InputSeq, e.FromSeq))
	}
	want := []string{
		"1 input.injected otlp  0 0",
		"2 stage.output resource logs/z 0 0",
		"3 stage.input routing  0 2",
		"4 stage.output routing logs/a 3 0",
		"5 stage.input filter logs/a 0 4",
		"6 stage.output filter logs/a 5 0",
		"7 exporter.captured otlp/logs/a  0 6",
		"8 stage.output routing logs/b 3 0",
		"9 stage.input filter logs/b 0 8",
		"10 stage.output filter logs/b 9 0",
		"11 exporter.captured otlp/logs/b  0 10",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events =\n%v\nwant\n%v", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !reflect.DeepEqual(a.Events, b.Events) {
		t.Fatalf("canonical orders differ:\n%v\n%v", a.Events, b.Events)
	}
}

func TestCanonicalizeOrdersSamplingDecisionsByTrace(t *testing.T) {
	decide := func(trace string, released bool) []instrument.Event {
		evs := []instrument.Event{{Kind: instrument.KindDecision, Component: "tail_sampling", Signal: "traces", Pipeline: "traces",
			Decision: &instrument.Decision{TraceID: trace}}}
		if released {
			evs = append(evs, instrument.Event{Kind: instrument.KindEgress, Component: "tail_sampling", Signal: "traces", Pipeline: "traces"},
				instrument.Event{Kind: instrument.KindExport, Component: "otlp", Signal: "traces"})
		}
		return evs
	}
	build := func(groups ...[]instrument.Event) *Result {
		r := &Result{Events: []instrument.Event{{Kind: instrument.KindInput, Signal: "traces"}}}
		r.Events = append(r.Events, slices.Concat(groups...)...)
		for i := range r.Events {
			r.Events[i].Seq = i + 1
		}
		return r
	}
	a := build(decide("b", true), decide("a", false), decide("c", true))
	b := build(decide("c", true), decide("b", true), decide("a", false))
	a.Canonicalize()
	b.Canonicalize()
	order := func(r *Result) string {
		var s []string
		for _, e := range r.Events {
			if e.Decision != nil {
				s = append(s, e.Decision.TraceID)
			} else {
				s = append(s, string(e.Kind))
			}
		}
		return strings.Join(s, " ")
	}
	want := "input.injected a b stage.output exporter.captured c stage.output exporter.captured"
	if order(a) != want || order(b) != want {
		t.Fatalf("orders:\n%s\n%s\nwant %s", order(a), order(b), want)
	}
}

// TestCanonicalizeReordersObjectsWithSortedPoints: sorting a metrics
// snapshot's data points moves their object numbers with them.
func TestCanonicalizeReordersObjectsWithSortedPoints(t *testing.T) {
	snapshot := func(values ...string) string {
		md := pmetric.NewMetrics()
		dps := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints()
		for _, v := range values {
			dps.AppendEmpty().Attributes().PutStr("k", v)
		}
		data, err := telemetry.FromMetrics(md).JSON()
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	r := &Result{Snapshots: instrument.Snapshots{"one": []byte(snapshot("a")), "two": []byte(snapshot("b", "a"))}}
	r.Events = []instrument.Event{
		{Seq: 1, Kind: instrument.KindIngress, Signal: "metrics", Pipeline: "metrics", Snapshot: "one", Objects: []int{9}},
		{Seq: 2, Kind: instrument.KindEgress, Signal: "metrics", Pipeline: "metrics", Snapshot: "two", Objects: []int{7, 9}},
	}
	r.Canonicalize()
	// "a" (object 9, renumbered 1) now comes first.
	if got := r.Events[1].Objects; !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("objects = %v, want [1 2]", got)
	}
	if string(r.Snapshots[r.Events[1].Snapshot]) != snapshot("a", "b") {
		t.Fatalf("snapshot not sorted: %s", r.Snapshots[r.Events[1].Snapshot])
	}
}

// TestCanonicalizeGivesFanOutObjectsToOneBranch: the Collector passes an
// injection's record objects on to one of two mutating pipelines, varying
// between runs, and clones for the other; normalized runs agree.
func TestCanonicalizeGivesFanOutObjectsToOneBranch(t *testing.T) {
	build := func(a, b []int) *Result {
		evs := []instrument.Event{
			{Kind: instrument.KindRunStarted, Source: instrument.SourceOrchestrator},
			{Kind: instrument.KindInput, Signal: "logs", Objects: []int{1, 2}},
			{Kind: instrument.KindIngress, Component: "attributes", Signal: "logs", Pipeline: "logs/a", FromSeq: 2, Objects: a},
			{Kind: instrument.KindEgress, Component: "attributes", Signal: "logs", Pipeline: "logs/a", InputSeq: 3, Objects: a},
			{Kind: instrument.KindIngress, Component: "filter", Signal: "logs", Pipeline: "logs/b", FromSeq: 2, Objects: b},
			{Kind: instrument.KindEgress, Component: "filter", Signal: "logs", Pipeline: "logs/b", InputSeq: 5, Objects: b[:1]},
			{Kind: instrument.KindRunCompleted, Source: instrument.SourceOrchestrator},
		}
		r := &Result{}
		for i, e := range evs {
			e.Seq, e.RunID, e.ID = i+1, "r", fmt.Sprintf("r:%d", i+1)
			r.Events = append(r.Events, e)
		}
		return r
	}
	x, y := build([]int{1, 2}, []int{3, 4}), build([]int{3, 4}, []int{1, 2})
	x.Canonicalize()
	y.Canonicalize()
	if !reflect.DeepEqual(x.Events, y.Events) {
		t.Fatalf("canonical objects differ:\n%+v\n%+v", x.Events, y.Events)
	}
	// The canonically last delivery (logs/b) keeps the injection's objects.
	if got := x.Events[4].Objects; !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("logs/b objects = %v, want [1 2]", got)
	}
	if got := x.Events[2].Objects; !slices.Equal(got, []int{3, 4}) {
		t.Fatalf("logs/a objects = %v, want [3 4]", got)
	}
}
