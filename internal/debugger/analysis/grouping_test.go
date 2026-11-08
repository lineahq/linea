package analysis

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// TestGroupMarksOnlyAttributesARecordGaveUp: a record value that replaces
// the resource's own value of the same key is moved; an attribute the input
// block already held with the same value is not. An output record without
// lineage is counted, not guessed, and an input without an emission keeps
// its blocks and no output.
func TestGroupMarksOnlyAttributesARecordGaveUp(t *testing.T) {
	const in = `{"resourceLogs":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"api"}},{"key":"host.name","value":{"stringValue":"a"}}]},` +
		`"scopeLogs":[{"logRecords":[{"body":{"stringValue":"x"}},{"body":{"stringValue":"y"}}]}]}]}`
	const out = `{"resourceLogs":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"api"}},{"key":"host.name","value":{"stringValue":"b"}}]},` +
		`"scopeLogs":[{"logRecords":[{"body":{"stringValue":"x"}},{"body":{"stringValue":"y"}}]}]}]}`
	events := []instrument.Event{
		{Seq: 2, Kind: instrument.KindIngress, Snapshot: "in"},
		{Seq: 3, Kind: instrument.KindEgress, Snapshot: "out"},
		{Seq: 4, Kind: instrument.KindIngress, Snapshot: "in"},
	}
	snaps := instrument.Snapshots{"in": json.RawMessage(in), "out": json.RawMessage(out)}
	lineage := []RecordLineage{{ID: "1:x", Name: "x", Hops: []Hop{
		{Parent: -1, Kind: instrument.KindInput},
		{Parent: 0, Seq: 2, Kind: instrument.KindIngress, Ref: "resource[0].scope[0].log_record[0]"},
		{Parent: 1, Seq: 3, Kind: instrument.KindEgress, Ref: "resource[0].scope[0].log_record[0]"},
	}}}
	stage := Stage{Component: "groupbyattrs", Pipeline: "logs", Kind: KindProcessor, Type: "groupbyattrs", Signal: "logs",
		Inputs: []StageInput{{Seq: 2, Outputs: []int{3}}, {Seq: 4}}}
	g, err := Group(GroupInput{Stage: stage, Keys: []string{"host.name"}, Events: events, Snapshots: snaps, Lineage: lineage})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(g.Requests))
	}
	r := g.Requests[0]
	if want := []GroupAttribute{{Key: "service.name", Value: "api"}, {Key: "host.name", Value: "b", Moved: true}}; !reflect.DeepEqual(r.After[0].Attributes, want) {
		t.Fatalf("attributes = %+v", r.After[0].Attributes)
	}
	if want := []GroupRecord{{Lineage: "1:x", Name: "x", From: 0, Moved: []string{"host.name"}}}; !reflect.DeepEqual(r.After[0].Records, want) {
		t.Fatalf("records = %+v", r.After[0].Records)
	}
	if r.Grouped != 1 || r.Kept != 0 || r.Unfollowed != 1 || r.Before[0].Records != 2 {
		t.Fatalf("request = %+v", r)
	}
	if e := g.Requests[1]; e.Output != nil || len(e.Before) != 1 || len(e.After) != 0 {
		t.Fatalf("request without emission = %+v", e)
	}
}
