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
