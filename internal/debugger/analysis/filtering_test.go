package analysis

import (
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// TestNameDropsFindsTheRecordsUnderEachDrop: a record drop names its record,
// a resource drop every record under it in input order, and records seen at
// another seq or not at the processor's input are not named.
func TestNameDropsFindsTheRecordsUnderEachDrop(t *testing.T) {
	hop := func(seq int, kind instrument.Kind, ref string) Hop {
		return Hop{Seq: seq, Kind: kind, Ref: ref, Parent: -1}
	}
	lineage := []RecordLineage{
		{ID: "1:a", Name: "a", Hops: []Hop{hop(1, instrument.KindInput, "x"), hop(5, instrument.KindIngress, "resource[0].scope[0].log_record[2]")}},
		{ID: "1:b", Name: "b", Hops: []Hop{hop(5, instrument.KindIngress, "resource[1].scope[0].log_record[10]")}},
		{ID: "1:c", Name: "c", Hops: []Hop{hop(5, instrument.KindIngress, "resource[1].scope[0].log_record[2]")}},
		{ID: "1:d", Name: "d", Hops: []Hop{hop(5, instrument.KindIngress, "resource[10].scope[0].log_record[0]")}},
		{ID: "1:e", Name: "e", Hops: []Hop{hop(6, instrument.KindIngress, "resource[0].scope[0].log_record[2]")}},
	}
	f := &Filtering{Input: 5, Drops: []FilterDrop{
		{Ref: "resource[0].scope[0].log_record[2]", Records: 1},
		{Ref: "resource[1]", Records: 2},
	}}
	NameDrops(f, lineage)
	got := [][]string{}
	for _, d := range f.Drops {
		var names []string
		for _, r := range d.Removed {
			names = append(names, r.Name)
		}
		got = append(got, names)
	}
	if want := [][]string{{"a"}, {"c", "b"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}
}
