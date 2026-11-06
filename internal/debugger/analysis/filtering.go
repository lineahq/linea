package analysis

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/debugger/instrument"
)

// FilterVerification states whether an explanation of a filter processor's
// drops is confirmed by what the processor actually emitted.
type FilterVerification string

const (
	// FilterVerified: evaluating the configured conditions on the recorded
	// input, in the processor's order, reproduces the recorded output (or
	// its absence), so each drop is attributed to the condition that made
	// it.
	FilterVerified FilterVerification = "replay_verified"
	// FilterDiffers: the evaluation does not reproduce the recorded output;
	// no drop is attributed to a condition.
	FilterDiffers FilterVerification = "replay_differs"
	// FilterUnsupported: the processor filters this signal with settings
	// that are not OTTL (include/exclude match properties or expr), which
	// the replay does not evaluate.
	FilterUnsupported FilterVerification = "unsupported"
)

// Filtering explains one input of a filter processor: which condition
// removed each resource, scope, record or span event, and how often each
// condition was evaluated, matched and failed. It is computed outside the
// run by evaluating the configured conditions with the processor's own
// function set, and its drops are authoritative only when Verification is
// replay_verified.
type Filtering struct {
	Processor string `json:"processor"`
	Pipeline  string `json:"pipeline"`
	Signal    string `json:"signal"`
	// Input is the processor's stage.input and Output the seq of its
	// emission; absent when it emitted nothing for the input.
	Input        int                `json:"input"`
	Output       *int               `json:"output,omitempty"`
	Verification FilterVerification `json:"verification"`
	// Conditions are the configured conditions for the signal, in
	// configuration order.
	Conditions []FilterCondition `json:"conditions"`
	// Records counts the input's records (spans, log records or data
	// points) and Dropped those the processor did not pass on, including
	// every record of a request lost to a propagated error.
	Records int `json:"records"`
	Dropped int `json:"dropped"`
	// Drops lists every item a condition removed, in evaluation order; only
	// present when the replay is verified.
	Drops []FilterDrop `json:"drops,omitempty"`
	// Error is the error the processor returned for the input (error_mode
	// propagate); the pipeline then drops the whole request.
	Error string   `json:"error,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// FilterCondition is one configured condition and what it did to the
// input.
type FilterCondition struct {
	Index int `json:"index"`
	// Source is where the condition is configured, for example
	// "log_conditions[0].conditions[1]" or "logs.log_record[0]".
	Source string `json:"source"`
	// Context is the OTTL context it is evaluated in: configured, or
	// inferred from its paths as the processor does.
	Context   string `json:"context"`
	Condition string `json:"condition"`
	ErrorMode string `json:"error_mode"`
	// Evaluated counts the items it was evaluated on; Matched those it was
	// the first true condition for, and so removed; Records the records
	// those items carried.
	Evaluated int `json:"evaluated"`
	Matched   int `json:"matched"`
	Records   int `json:"records"`
	// Errors counts failed evaluations and Error is the first failure.
	Errors int    `json:"errors"`
	Error  string `json:"error,omitempty"`
}

// FilterDrop is one item a condition removed.
type FilterDrop struct {
	// Ref is the item's position in the input payload: "resource[0]",
	// "resource[0].scope[1]", "resource[0].scope[0].log_record[2]",
	// "resource[0].scope[0].span[1]", "resource[0].scope[0].span[1].event[0]",
	// "resource[0].scope[0].metric[2]" or
	// "resource[0].scope[0].metric[2].data_point[0]".
	Ref string `json:"ref"`
	// Condition is the index of the condition that removed it.
	Condition int `json:"condition"`
	// Records counts the records removed with it (0 for a span event).
	Records int `json:"records"`
	// Removed are the removed records the run's lineage followed into the
	// processor, in input order: each record's lineage ID and name.
	Removed []FilterRecord `json:"removed,omitempty"`
}

// FilterRecord is one record a drop removed.
type FilterRecord struct {
	Lineage string `json:"lineage"`
	Name    string `json:"name"`
	Ref     string `json:"ref"`
}

// NameDrops fills each drop's Removed from the lineage hops at the
// processor's input (seq): a record is removed with a drop when its
// position is the drop's or lies under it.
func NameDrops(f *Filtering, lineage []RecordLineage) {
	if len(f.Drops) == 0 {
		return
	}
	var in []FilterRecord
	for _, l := range lineage {
		for _, h := range l.Hops {
			if h.Seq == f.Input && h.Kind == instrument.KindIngress {
				in = append(in, FilterRecord{Lineage: l.ID, Name: l.Name, Ref: h.Ref})
			}
		}
	}
	for i := range f.Drops {
		d := &f.Drops[i]
		for _, r := range in {
			if r.Ref == d.Ref || strings.HasPrefix(r.Ref, d.Ref+".") {
				d.Removed = append(d.Removed, r)
			}
		}
		slices.SortFunc(d.Removed, func(a, b FilterRecord) int { return compareRefs(a.Ref, b.Ref) })
	}
}

// compareRefs orders payload positions by their indexes, so log_record[2]
// comes before log_record[10].
func compareRefs(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if c := cmp.Compare(refIndex(pa[i]), refIndex(pb[i])); c != 0 {
			return c
		}
		if c := strings.Compare(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(pa), len(pb))
}

func refIndex(part string) int {
	i := strings.LastIndexByte(part, '[')
	n, err := strconv.Atoi(strings.TrimSuffix(part[i+1:], "]"))
	if i < 0 || err != nil {
		return -1
	}
	return n
}
