package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestGroupingFollowsEveryRecordIntoItsResource runs samples/resource-grouping
// on the real groupbyattrs processor: records with host.name move under a
// resource that carries it, and the two checkout blocks' records without it
// merge into one block, since their attributes end up equal.
func TestGroupingFollowsEveryRecordIntoItsResource(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "resource-grouping")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Grouping) != 1 {
		t.Fatalf("grouping = %d entries, want 1", len(rep.Grouping))
	}
	g := rep.Grouping[0]
	if !reflect.DeepEqual(g.Keys, []string{"host.name"}) || len(g.Requests) != 1 {
		t.Fatalf("grouping = %+v", g)
	}
	r := g.Requests[0]
	if len(r.Before) != 3 || len(r.After) != 4 || r.Output == nil {
		t.Fatalf("request = %d blocks in, %d out, output %v", len(r.Before), len(r.After), r.Output)
	}
	if r.Grouped != 4 || r.Kept != 2 || r.Unfollowed != 0 || !reflect.DeepEqual(r.Moved, []string{"host.name"}) {
		t.Fatalf("request = grouped %d, kept %d, unfollowed %d, moved %v", r.Grouped, r.Kept, r.Unfollowed, r.Moved)
	}
	type block struct {
		attrs   map[string]string
		moved   []string
		records []string
		from    []int
	}
	var got []block
	for _, b := range r.After {
		x := block{attrs: map[string]string{}}
		for _, a := range b.Attributes {
			x.attrs[a.Key] = a.Value
			if a.Moved {
				x.moved = append(x.moved, a.Key)
			}
		}
		for _, rec := range b.Records {
			x.records = append(x.records, rec.Name)
		}
		for _, f := range b.From {
			x.from = append(x.from, f.Block)
		}
		got = append(got, x)
	}
	want := []block{
		{map[string]string{"service.name": "checkout", "host.name": "node-1"}, []string{"host.name"}, []string{"order 1 placed", "order 3 placed"}, []int{0}},
		{map[string]string{"service.name": "checkout", "host.name": "node-2"}, []string{"host.name"}, []string{"order 2 placed"}, []int{0}},
		{map[string]string{"service.name": "checkout"}, nil, []string{"cache warmed", "checkout started"}, []int{0, 2}},
		{map[string]string{"service.name": "cart", "host.name": "node-1"}, []string{"host.name"}, []string{"cart 7 updated"}, []int{1}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output blocks =\n%+v\nwant\n%+v", got, want)
	}
	if rec := r.After[0].Records[0]; rec.From != 0 || !reflect.DeepEqual(rec.Moved, []string{"host.name"}) || rec.Lineage == "" {
		t.Fatalf("first record = %+v", rec)
	}
}
