package analysis

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// arrivalLineage is the lineage of one record injected by input event
// root that reaches the batch processor at arrival, optionally through an
// earlier batch processor.
func arrivalLineage(root, arrival int, via string) RecordLineage {
	hops := []Hop{{Parent: -1, Seq: root, Kind: instrument.KindInput, Component: "otlp"}}
	if via != "" {
		hops = append(hops,
			Hop{Parent: 0, Seq: root + 100, Kind: instrument.KindIngress, Component: via, Pipeline: "logs"},
			Hop{Parent: 1, Seq: root + 200, Kind: instrument.KindEgress, Component: via, Pipeline: "logs"})
	}
	hops = append(hops, Hop{Parent: len(hops) - 1, Seq: arrival, Kind: instrument.KindIngress, Component: "batch", Pipeline: "logs"})
	return RecordLineage{Signal: "logs", Hops: hops}
}

// TestBatchKeepsOneBatcherPerMetadataValues: arrivals are grouped by the
// values of the listed keys in their inputs' headers (matched in lower
// case, a repeated header being one value list), a missing key has its own
// batcher, unlisted headers never appear, arrivals that came through
// another batch processor are unknown, and combinations beyond
// metadata_cardinality_limit are refused.
func TestBatchKeepsOneBatcherPerMetadataValues(t *testing.T) {
	// Arrival seqs 2..6 come from input seqs 10..14.
	events := batchEvents([]batchStep{{"in", 0, 1}, {"in", 0, 1}, {"in", 0, 1}, {"in", 0, 1}, {"in", 0, 1}})
	headers := map[int]map[string][]string{
		10: {"tenant": {"acme"}, "authorization": {"secret"}},
		11: {"tenant": {"acme", "globex"}},
		12: {"other": {"x"}},
		14: {"tenant": {"acme"}},
	}
	lineage := []RecordLineage{arrivalLineage(10, 2, ""), arrivalLineage(11, 3, ""), arrivalLineage(12, 4, ""), arrivalLineage(13, 5, "batch/first"), arrivalLineage(14, 6, "")}
	in := BatchInput{
		Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: BatchSettings{SendBatchSize: 8, Timeout: "200ms", MetadataKeys: []string{"Tenant"}, MetadataCardinalityLimit: 2},
		Timeout:  200 * time.Millisecond, Events: events, Lineage: lineage,
		InputHeaders: headers, BatchProcessors: map[string]bool{"batch": true, "batch/first": true},
	}
	b, err := Batch(in)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, a := range b.Arrivals {
		got = append(got, a.Batcher)
	}
	if want := []int{0, 1, 2, 3, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("arrival batchers = %v, want %v", got, want)
	}
	want := []BatchBatcher{
		{Metadata: map[string][]string{"tenant": {"acme"}}, Arrivals: 2, Items: 2},
		{Metadata: map[string][]string{"tenant": {"acme", "globex"}}, Arrivals: 1, Items: 1},
		{Metadata: map[string][]string{"tenant": {}}, Refused: true, Arrivals: 1},
		{Metadata: map[string][]string{}, Unknown: true, Arrivals: 1, Items: 1},
	}
	if !reflect.DeepEqual(b.Batchers, want) {
		t.Fatalf("batchers = %+v, want %+v", b.Batchers, want)
	}
	notes := strings.Join(b.Notes, "\n")
	for _, w := range []string{"1 requests could not be traced", "metadata_cardinality_limit 2"} {
		if !strings.Contains(notes, w) {
			t.Fatalf("notes = %v, want %q", b.Notes, w)
		}
	}
	if strings.Contains(notes, "secret") {
		t.Fatalf("notes leak a header value: %v", b.Notes)
	}
}

// TestBatchTimesEachBatcherFromItsFirstRequest: with metadata_keys a
// batcher's timer starts when its first request arrives, so two batchers'
// timeout sends follow their own first arrivals.
func TestBatchTimesEachBatcherFromItsFirstRequest(t *testing.T) {
	events := batchEvents([]batchStep{{"in", 0, 1}, {"in", 150, 1}, {"out", 200, 1}, {"out", 350, 1}})
	lineage := []RecordLineage{arrivalLineage(10, 2, ""), arrivalLineage(11, 3, "")}
	// Flushes come from the arrivals' records.
	lineage[0].Hops = append(lineage[0].Hops, Hop{Parent: 1, Seq: 4, Kind: instrument.KindEgress, Component: "batch", Pipeline: "logs"})
	lineage[1].Hops = append(lineage[1].Hops, Hop{Parent: 1, Seq: 5, Kind: instrument.KindEgress, Component: "batch", Pipeline: "logs"})
	b, err := Batch(BatchInput{
		Processor: "batch", Pipeline: "logs", Signal: "logs",
		Settings: BatchSettings{SendBatchSize: 8, Timeout: "200ms", MetadataKeys: []string{"tenant"}},
		Timeout:  200 * time.Millisecond, Events: events, Lineage: lineage,
		InputHeaders: map[int]map[string][]string{10: {"tenant": {"a"}}, 11: {"tenant": {"b"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range b.Flushes {
		if f.Trigger != TriggerTimeout || f.Batcher != i {
			t.Fatalf("flush %d = %+v, want a timeout of batcher %d (notes %v)", i, f, i, b.Notes)
		}
	}
	if len(b.Notes) != 0 {
		t.Fatalf("notes = %v", b.Notes)
	}
}
