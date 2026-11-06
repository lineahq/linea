package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
)

// TestWriteGroupingMarksMovedAttributesAndMerges: the text names each output
// block's attributes with a * on those moved up from the records, the input
// blocks it came from and its records, and a request without an emission.
func TestWriteGroupingMarksMovedAttributesAndMerges(t *testing.T) {
	out := 5
	g := &analysis.Grouping{Processor: "groupbyattrs", Pipeline: "logs", Signal: "logs", Keys: []string{"host.name"}, Requests: []analysis.GroupRequest{
		{Input: 4, Output: &out, Before: make([]analysis.GroupBlockIn, 2), Grouped: 1, Kept: 2, After: []analysis.GroupBlockOut{
			{Attributes: []analysis.GroupAttribute{{Key: "service.name", Value: "api"}, {Key: "host.name", Value: "a", Moved: true}},
				Records: []analysis.GroupRecord{{Name: "x", Moved: []string{"host.name"}}}, From: []analysis.GroupSource{{Block: 0, Records: 1}}},
			{Attributes: []analysis.GroupAttribute{{Key: "service.name", Value: "api"}},
				Records: []analysis.GroupRecord{{Name: "y"}, {Name: "z", From: 1}}, From: []analysis.GroupSource{{Block: 0, Records: 1}, {Block: 1, Records: 1}}},
		}},
		{Input: 6, Before: make([]analysis.GroupBlockIn, 1)},
	}}
	var b strings.Builder
	writeGrouping(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }, g)
	want := `  groupbyattrs (pipeline logs, logs): keys host.name
    input seq 4 -> seq 5: 2 resource block(s) in, 2 out; 1 record(s) moved attributes up, 2 kept their resource
      out 1 {service.name=api, host.name=a*} from in 1: x
      out 2 {service.name=api} merged from in 1, in 2: y, z
    input seq 6: 1 resource block(s) in, nothing emitted
    * moved up from the records
`
	if b.String() != want {
		t.Fatalf("text =\n%s\nwant\n%s", b.String(), want)
	}
}

// TestWriteFilteringNamesConditionsAndDrops: the text lists each condition
// with its counts and each drop with the condition that made it, and only
// the configuration when the replay does not cover the settings.
func TestWriteFilteringNamesConditionsAndDrops(t *testing.T) {
	out := 4
	cases := []struct {
		name string
		f    *analysis.Filtering
		want string
	}{
		{"verified", &analysis.Filtering{Processor: "filter/health", Pipeline: "logs", Signal: "logs", Input: 3, Output: &out, Verification: analysis.FilterVerified, Records: 6, Dropped: 5,
			Conditions: []analysis.FilterCondition{
				{Index: 0, Source: "logs.resource[0]", Context: "resource", Condition: `attributes["env"] == "dev"`, ErrorMode: "ignore", Evaluated: 2, Matched: 1, Records: 2},
				{Index: 1, Source: "logs.log_record[0]", Context: "log", Condition: `IsMatch(body, "^health")`, ErrorMode: "ignore", Evaluated: 2, Matched: 1, Records: 1, Errors: 1, Error: "boom"},
			},
			Drops: []analysis.FilterDrop{
				{Ref: "resource[1]", Condition: 0, Records: 4, Removed: []analysis.FilterRecord{{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}}},
				{Ref: "resource[0].scope[0].log_record[1]", Condition: 1, Records: 1, Removed: []analysis.FilterRecord{{Name: "healthcheck ok"}}},
			},
		}, `  filter/health (pipeline logs, logs): input seq 3 -> seq 4: 6 record(s) in, 5 dropped, replay_verified
    [0] logs.resource[0] (resource, ignore): attributes["env"] == "dev"
        evaluated 2, matched 1 (2 record(s)), 0 error(s)
    [1] logs.log_record[0] (log, ignore): IsMatch(body, "^health")
        evaluated 2, matched 1 (1 record(s)), 1 error(s): boom
    resource[1] dropped by [0] with 4 record(s): "a", "b", "c", +1 more
    resource[0].scope[0].log_record[1] dropped by [1]: "healthcheck ok"
`},
		{"unsupported", &analysis.Filtering{Processor: "filter", Pipeline: "logs", Signal: "logs", Input: 3, Verification: analysis.FilterUnsupported, Records: 2, Dropped: 2,
			Notes: []string{"match properties"}}, `  filter (pipeline logs, logs): input seq 3, nothing emitted: 2 record(s) in, 2 dropped, unsupported
    note: match properties
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			writeFiltering(func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }, c.f)
			if b.String() != c.want {
				t.Fatalf("text =\n%s\nwant\n%s", b.String(), c.want)
			}
		})
	}
}
