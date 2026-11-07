package recommend

import (
	"fmt"
	"strconv"
)

// DeadWrite is an attribute a processor set on records of a pipeline that a
// later processor in the same pipeline removed or set again on every one of
// them, with nothing between the two reading it. The engine computes it from
// lineage (deadWrites in internal/debugger/engine); the rule only reads it.
type DeadWrite struct {
	Pipeline, Signal string
	// Writer set the attribute; Overwriter removed it (Removed) or set it
	// again.
	Writer, Overwriter string
	// Level is resource, scope or record; Key the attribute key.
	Level, Key string
	Removed    bool
	// Records counts the records the write reached and lost.
	Records int
}

// attributeDeadWrite: a write no record kept, because a later processor
// removed or replaced the attribute before anything read it.
func attributeDeadWrite(in Input) []Recommendation {
	var out []Recommendation
	for _, d := range in.DeadWrites {
		if d.Records == 0 {
			continue
		}
		name := strconv.Quote(d.Key)
		if d.Level != "record" {
			name = d.Level + " attribute " + name
		} else {
			name = "attribute " + name
		}
		after := "set it again"
		if d.Removed {
			after = "removed it"
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: d.Pipeline,
			Components: []string{processorRef(d.Writer), processorRef(d.Overwriter)},
			Summary: fmt.Sprintf("%s set %s on %s in pipeline %s, and %s %s on each of them before any processor read it. If the value is meant to reach the output, %s undoes it; otherwise the write is not needed.",
				d.Writer, name, plural(d.Records, itemNoun(d.Signal)), d.Pipeline, d.Overwriter, after, d.Overwriter),
			Evidence: []Evidence{{SectionStages, processorRef(d.Writer)}, {SectionStages, processorRef(d.Overwriter)}},
			Benefit:  &Benefit{MetricRecordsWithoutEffect, d.Records},
			Coverage: []string{fmt.Sprintf("Observed on %s; on another input, records %s leaves alone keep the value.",
				plural(d.Records, itemNoun(d.Signal)), d.Overwriter)},
		})
	}
	return out
}
