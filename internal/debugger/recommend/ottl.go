package recommend

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
)

// statementTotal is one transform statement's counts over the requests of
// one processor, in every pipeline that uses it, whose replay reproduces the
// output. pipeline is set when one pipeline uses it.
type statementTotal struct {
	processor, pipeline, signal, context string
	pipelines                            []string
	group, index                         int
	text                                 string
	matched, notMatched, errors          int
	unchanged                            int
	// duplicate: the previous statement of the group has the same text.
	duplicate bool
	requests  int
	unchecked int
}

func (t statementTotal) ref() string {
	return fmt.Sprintf("%s#group[%d].statement[%d]", processorRef(t.processor), t.group, t.index)
}

func (t statementTotal) name() string {
	return fmt.Sprintf("Statement %d of group %d of %s", t.index, t.group, t.processor)
}

func (t statementTotal) change() *ConfigChange {
	c := &ConfigChange{Op: OpRemoveStatement, Component: t.processor, Group: ptr(t.group), Index: ptr(t.index), Text: t.text}
	if list := map[string]string{"traces": "trace_statements", "logs": "log_statements", "metrics": "metric_statements"}[t.signal]; list != "" {
		c.Path = fmt.Sprintf("%s[%d].statements[%d]", list, t.group, t.index)
	}
	return c
}

func (t statementTotal) coverage() []string {
	out := []string{fmt.Sprintf("Observed on %s; another input may differ.", plural(t.requests, "request"))}
	if t.unchecked > 0 {
		out = append(out, fmt.Sprintf("Not counted: %s whose replay did not reproduce the output.", plural(t.unchecked, "request")))
	}
	return out
}

// statementTotals sums the verified OTTL replays per processor, pipeline and
// statement, in report order.
func statementTotals(in Input) []*statementTotal {
	pipelineOf := map[int]string{}
	for _, s := range in.Stages {
		if s.Kind != analysis.KindProcessor {
			continue
		}
		for _, input := range s.Inputs {
			pipelineOf[input.Seq] = s.Pipeline
		}
	}
	type key struct {
		processor    string
		group, index int
	}
	var out []*statementTotal
	byKey := map[key]*statementTotal{}
	requests, unchecked, pipelines := map[string]int{}, map[string]int{}, map[string][]string{}
	for _, r := range in.OTTL {
		if pl := pipelineOf[r.Input]; !slices.Contains(pipelines[r.Processor], pl) {
			pipelines[r.Processor] = append(pipelines[r.Processor], pl)
		}
		if r.Verification != ottldebug.VerificationReplayVerified {
			unchecked[r.Processor]++
			continue
		}
		requests[r.Processor]++
		for _, g := range r.Groups {
			if g.Status != ottldebug.StatusReplayed {
				continue
			}
			for i, s := range g.Statements {
				k := key{r.Processor, g.Index, s.Index}
				t, ok := byKey[k]
				if !ok {
					t = &statementTotal{processor: r.Processor, signal: r.Signal, context: g.Context, group: g.Index, index: s.Index, text: s.Text}
					t.duplicate = i > 0 && g.Statements[i-1].Text == s.Text
					byKey[k] = t
					out = append(out, t)
				}
				t.matched += s.Matched
				t.notMatched += s.NotMatched
				t.errors += len(s.Errors)
				t.unchanged += s.Unchanged
			}
		}
	}
	for _, t := range out {
		t.requests, t.unchecked, t.pipelines = requests[t.processor], unchecked[t.processor], pipelines[t.processor]
		if len(t.pipelines) == 1 {
			t.pipeline = t.pipelines[0]
		}
	}
	return out
}

// statementNeverMatched: a statement whose where clause was false on every
// item it was evaluated on.
func statementNeverMatched(in Input) []Recommendation {
	var out []Recommendation
	for _, t := range statementTotals(in) {
		if t.matched > 0 || t.notMatched == 0 || t.errors > 0 {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: t.pipeline,
			Components: []string{processorRef(t.processor)}, Subject: t.text,
			Summary: fmt.Sprintf("%s never matched: its where clause was false on all %s it was evaluated on in %s, so it changed nothing. If this input holds items it should change, the where clause is wrong rather than unneeded.",
				t.name(), plural(t.notMatched, contextNoun(t.context)), pipelinesPhrase(t.pipelines)),
			Evidence: []Evidence{{SectionOTTL, t.ref()}},
			Benefit:  &Benefit{Metric: MetricEvaluationsWithoutEffect, Count: t.notMatched},
			Change:   t.change(),
			Coverage: t.coverage(),
		})
	}
	return out
}

// statementNoChange: a statement that ran and changed nothing, because every
// item already held what it sets. Statements that write the cache change no
// item by design and are left out.
func statementNoChange(in Input) []Recommendation {
	var out []Recommendation
	for _, t := range statementTotals(in) {
		if !noChange(t) || t.duplicate {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: t.pipeline,
			Components: []string{processorRef(t.processor)}, Subject: t.text,
			Summary: fmt.Sprintf("%s ran on %s and changed none of them: each already held what it sets.",
				t.name(), plural(t.matched, contextNoun(t.context))),
			Evidence: []Evidence{{SectionOTTL, t.ref()}},
			Benefit:  &Benefit{Metric: MetricEvaluationsWithoutEffect, Count: t.matched},
			Change:   t.change(),
			Coverage: t.coverage(),
		})
	}
	return out
}

// duplicateStatement: a statement that repeats the one before it and
// changed nothing after it.
func duplicateStatement(in Input) []Recommendation {
	var out []Recommendation
	for _, t := range statementTotals(in) {
		if !t.duplicate || !noChange(t) {
			continue
		}
		out = append(out, Recommendation{
			Kind: KindRedundancy, Confidence: ConfidenceObserved, Pipeline: t.pipeline,
			Components: []string{processorRef(t.processor)}, Subject: t.text,
			Summary: fmt.Sprintf("%s repeats the statement before it and changed none of the %s it ran on.",
				t.name(), plural(t.matched, contextNoun(t.context))),
			Evidence: []Evidence{{SectionOTTL, t.ref()}},
			Benefit:  &Benefit{Metric: MetricEvaluationsWithoutEffect, Count: t.matched},
			Change:   t.change(),
			Coverage: t.coverage(),
		})
	}
	return out
}

func noChange(t *statementTotal) bool {
	return t.matched > 0 && t.unchanged == t.matched && t.errors == 0 && !strings.Contains(t.text, "cache[")
}

// contextNoun names an OTTL context's items.
func contextNoun(context string) string {
	switch context {
	case "log":
		return "log record"
	case "datapoint":
		return "data point"
	case "spanevent":
		return "span event"
	case "":
		return "item"
	}
	return context
}
