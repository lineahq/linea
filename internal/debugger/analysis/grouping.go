package analysis

import (
	"regexp"
	"slices"
	"strconv"

	"go.opentelemetry.io/collector/pdata/pcommon"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Grouping explains what one groupbyattrs processor instance (groupbyattrs
// in one pipeline) did to each request: the resource blocks it received,
// the blocks it emitted and where each record went. Records are tied to
// their input position by the record lineage, so nothing is inferred from
// the configuration (groupbyattrsprocessor v0.162.0, docs/internals/grouping.md).
type Grouping struct {
	Processor string `json:"processor"`
	Pipeline  string `json:"pipeline"`
	Signal    string `json:"signal"`
	// Keys are the configured keys.
	Keys     []string       `json:"keys"`
	Requests []GroupRequest `json:"requests"`
	Notes    []string       `json:"notes,omitempty"`
}

// GroupRequest is one request the processor received and the request it
// emitted for it.
type GroupRequest struct {
	// Input is the stage input's seq and Output the seq of its emission;
	// absent when the processor emitted nothing for it.
	Input  int  `json:"input"`
	Output *int `json:"output,omitempty"`
	// Before are the input's resource blocks and After the output's, in
	// payload order.
	Before []GroupBlockIn  `json:"before"`
	After  []GroupBlockOut `json:"after"`
	// Moved are the attribute keys records gave up to their resource, in
	// first-seen order.
	Moved []string `json:"moved"`
	// Grouped counts the followed records that gave up an attribute, Kept
	// those that kept their resource as it was (possibly merged with an
	// equal one), and Unfollowed the output records the lineage did not tie
	// to an input record.
	Grouped    int `json:"grouped"`
	Kept       int `json:"kept"`
	Unfollowed int `json:"unfollowed"`
}

// GroupAttribute is one resource attribute. Moved: it came from the
// records, because the input block of a record under it lacked the
// attribute or held another value.
type GroupAttribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Moved bool   `json:"moved,omitempty"`
}

// GroupBlockIn is one resource block of the input.
type GroupBlockIn struct {
	Attributes []GroupAttribute `json:"attributes"`
	// Records counts spans, log records or metric data points.
	Records int `json:"records"`
}

// GroupBlockOut is one resource block of the output.
type GroupBlockOut struct {
	Attributes []GroupAttribute `json:"attributes"`
	// Records are the followed records under the block, in lineage order.
	Records []GroupRecord `json:"records"`
	// From are the input blocks those records came from, in block order;
	// more than one means the processor merged blocks.
	From []GroupSource `json:"from"`
}

// GroupRecord is one record the lineage followed through the processor.
type GroupRecord struct {
	Lineage string `json:"lineage"`
	Name    string `json:"name"`
	// From indexes GroupRequest.Before.
	From int `json:"from"`
	// Moved are the keys of the block's attributes this record gave up.
	Moved []string `json:"moved,omitempty"`
}

// GroupSource is how many of an output block's records came from one input
// block.
type GroupSource struct {
	Block   int `json:"block"`
	Records int `json:"records"`
}

// GroupInput is what Group reads for one processor instance.
type GroupInput struct {
	Stage     Stage
	Keys      []string
	Events    []instrument.Event
	Snapshots instrument.Snapshots
	Lineage   []RecordLineage
}

// Group explains one groupbyattrs processor instance.
func Group(in GroupInput) (*Grouping, error) {
	s := in.Stage
	g := &Grouping{Processor: s.Component, Pipeline: s.Pipeline, Signal: s.Signal, Keys: in.Keys, Requests: []GroupRequest{}}
	if g.Keys == nil {
		g.Keys = []string{}
	}
	bySeq := map[int]instrument.Event{}
	for _, e := range in.Events {
		bySeq[e.Seq] = e
	}
	links := groupLinks(in.Lineage)
	for _, input := range s.Inputs {
		r := GroupRequest{Input: input.Seq, Before: []GroupBlockIn{}, After: []GroupBlockOut{}, Moved: []string{}}
		before, ok, err := payloadOf(bySeq[input.Seq], in.Snapshots)
		if err != nil {
			return nil, err
		}
		var beforeValues []pcommon.Map
		if ok {
			counts := blockRecords(before)
			for i := range before.Counts().Resources {
				res, _ := before.ResourceAt(i)
				beforeValues = append(beforeValues, res.Attributes())
				r.Before = append(r.Before, GroupBlockIn{Attributes: attributesOf(res.Attributes()), Records: counts[i]})
			}
		}
		if len(input.Outputs) > 1 {
			g.Notes = append(g.Notes, "input "+strconv.Itoa(input.Seq)+" has "+strconv.Itoa(len(input.Outputs))+" emissions; only the first is shown")
		}
		if len(input.Outputs) == 0 {
			g.Requests = append(g.Requests, r)
			continue
		}
		out := input.Outputs[0]
		r.Output = &out
		after, ok, err := payloadOf(bySeq[out], in.Snapshots)
		if err != nil {
			return nil, err
		}
		total := 0
		var afterValues []pcommon.Map
		if ok {
			total = after.Counts().Items
			for i := range after.Counts().Resources {
				res, _ := after.ResourceAt(i)
				afterValues = append(afterValues, res.Attributes())
				r.After = append(r.After, GroupBlockOut{Attributes: attributesOf(res.Attributes()), Records: []GroupRecord{}, From: []GroupSource{}})
			}
		}
		followed := 0
		for _, l := range links[[2]int{input.Seq, out}] {
			from, to := resourceIndex(l.from), resourceIndex(l.to)
			if from < 0 || to < 0 || from >= len(r.Before) || to >= len(r.After) {
				continue
			}
			followed++
			block := &r.After[to]
			rec := GroupRecord{Lineage: l.lineage, Name: l.name, From: from}
			for i := range block.Attributes {
				a := &block.Attributes[i]
				want, _ := afterValues[to].Get(a.Key)
				if have, found := beforeValues[from].Get(a.Key); found && have.Equal(want) {
					continue
				}
				a.Moved = true
				rec.Moved = append(rec.Moved, a.Key)
				if !slices.Contains(r.Moved, a.Key) {
					r.Moved = append(r.Moved, a.Key)
				}
			}
			if len(rec.Moved) > 0 {
				r.Grouped++
			}
			block.Records = append(block.Records, rec)
			addSource(block, from)
		}
		r.Kept = followed - r.Grouped
		r.Unfollowed = max(0, total-followed)
		g.Requests = append(g.Requests, r)
	}
	return g, nil
}

// groupLink is one record's position in a stage input and in its emission.
type groupLink struct {
	lineage, name, from, to string
}

// groupLinks indexes, by stage input and output seq, the records the
// lineage followed across a stage.
func groupLinks(lineage []RecordLineage) map[[2]int][]groupLink {
	out := map[[2]int][]groupLink{}
	for _, l := range lineage {
		for _, h := range l.Hops {
			if h.Kind != instrument.KindEgress || h.Parent < 0 || h.Parent >= len(l.Hops) {
				continue
			}
			p := l.Hops[h.Parent]
			if p.Kind != instrument.KindIngress {
				continue
			}
			k := [2]int{p.Seq, h.Seq}
			out[k] = append(out[k], groupLink{lineage: l.ID, name: l.Name, from: p.Ref, to: h.Ref})
		}
	}
	return out
}

func payloadOf(e instrument.Event, snapshots instrument.Snapshots) (telemetry.Payload, bool, error) {
	if e.Snapshot == "" {
		return telemetry.Payload{}, false, nil
	}
	p, err := snapshots.Payload(e.Snapshot)
	if err != nil {
		return telemetry.Payload{}, false, err
	}
	return p, true, nil
}

func attributesOf(m pcommon.Map) []GroupAttribute {
	out := make([]GroupAttribute, 0, m.Len())
	for k, v := range m.All() {
		out = append(out, GroupAttribute{Key: k, Value: v.AsString()})
	}
	return out
}

// blockRecords counts the records of each resource block.
func blockRecords(p telemetry.Payload) map[int]int {
	n := map[int]int{}
	for _, r := range telemetry.Records(p) {
		n[r.Resource]++
	}
	return n
}

var resourceRef = regexp.MustCompile(`^resource\[(\d+)\]`)

func resourceIndex(ref string) int {
	m := resourceRef.FindStringSubmatch(ref)
	if m == nil {
		return -1
	}
	i, err := strconv.Atoi(m[1])
	if err != nil {
		return -1
	}
	return i
}

func addSource(b *GroupBlockOut, from int) {
	for i := range b.From {
		if b.From[i].Block == from {
			b.From[i].Records++
			return
		}
	}
	b.From = append(b.From, GroupSource{Block: from, Records: 1})
	for i := len(b.From) - 1; i > 0 && b.From[i].Block < b.From[i-1].Block; i-- {
		b.From[i], b.From[i-1] = b.From[i-1], b.From[i]
	}
}
