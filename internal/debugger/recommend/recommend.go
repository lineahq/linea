// Package recommend turns a run's recorded evidence into reviewable
// suggestions: work the run shows was avoidable, settings the input never
// exercised, and orderings that risk data. Rules are pure functions over the
// report's sections; each has a stable ID. A recommendation is scoped to the
// run it came from and never changes the user's configuration.
package recommend

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// Kind says what a recommendation is about.
type Kind string

const (
	// KindOrdering: the same output with less work in another order.
	KindOrdering Kind = "ordering"
	// KindRedundancy: configuration that did nothing on this input.
	KindRedundancy Kind = "redundancy"
	// KindInefficiency: settings that make the component work in a way it
	// was likely not configured for, at this input's rate.
	KindInefficiency Kind = "inefficiency"
	// KindLossy: a suggestion that drops data, with what it would drop.
	KindLossy Kind = "lossy"
	// KindRisk: configuration that can lose or misplace data.
	KindRisk Kind = "risk"
	// KindUntested: configuration this input never exercised, such as a
	// condition no record matched. It suggests no change: the run shows
	// only that the input lacks what it targets.
	KindUntested Kind = "untested"
)

// Holds says on which inputs a recommendation's claim holds.
type Holds string

const (
	// HoldsAnyInput: the configuration, or the statement's own text, proves
	// it, whatever the input.
	HoldsAnyInput Holds = "any_input"
	// HoldsThisInput: this run's input shows it; another input may not.
	HoldsThisInput Holds = "this_input"
)

// Confidence says what a recommendation rests on.
type Confidence string

const (
	// ConfidenceStatic: the configuration alone.
	ConfidenceStatic Confidence = "static"
	// ConfidenceObserved: this run's recorded events.
	ConfidenceObserved Confidence = "observed"
	// ConfidenceVerifiedOnInput: the changed configuration ran on the same
	// input and produced the same output.
	ConfidenceVerifiedOnInput Confidence = "verified_on_input"
	// ConfidenceDiffersOnInput: the changed configuration ran on the same
	// input and its output differs.
	ConfidenceDiffersOnInput Confidence = "differs_on_input"
)

// Op is the kind of a suggested change.
type Op string

const (
	// OpSwap: replace a pipeline's processor list with Change.Processors.
	OpSwap Op = "swap"
	// OpRemoveStatement: remove one OTTL statement or filter condition.
	OpRemoveStatement Op = "remove_statement"
	// OpSetOption: set one option of a component.
	OpSetOption Op = "set_option"
)

// Output says what applying a recommendation does to what the exporters
// receive. Each rule states it; a verification confirms or refutes it on the
// run's input.
type Output string

const (
	// OutputUnchanged: the exporters receive the same records.
	OutputUnchanged Output = "unchanged"
	// OutputRegrouped: the same records, in other batches, requests or
	// resource blocks.
	OutputRegrouped Output = "regrouped"
	// OutputChanged: the exporters receive other records or values; that
	// is the change's purpose.
	OutputChanged Output = "changed"
)

// Benefit metrics: what the run shows a change would save.
const (
	// MetricEvaluationsWithoutEffect: condition or statement evaluations
	// that matched nothing or changed nothing.
	MetricEvaluationsWithoutEffect = "evaluations_without_effect"
	// MetricRecordsWithoutEffect: records a component received and passed
	// on unchanged.
	MetricRecordsWithoutEffect = "records_without_effect"
	// MetricRecordsNotProcessed: records earlier components would no
	// longer process.
	MetricRecordsNotProcessed = "records_not_processed"
	// MetricResourcesMerged: resource blocks the batch processor sent that
	// a regrouping after it would merge into equal ones.
	MetricResourcesMerged = "resource_blocks_merged"
)

// Evidence sections, named after the report fields they point into.
const (
	SectionTopology  = "topology"
	SectionStages    = "stages"
	SectionFiltering = "filtering"
	SectionOTTL      = "ottl"
	SectionBatching  = "batching"
	SectionGrouping  = "grouping"
	SectionSampling  = "sampling"
)

// Recommendation is one suggestion with what it is derived from.
type Recommendation struct {
	// Rule is the stable rule ID, such as "filter.condition_never_matched".
	Rule       string     `json:"rule"`
	Kind       Kind       `json:"kind"`
	Confidence Confidence `json:"confidence"`
	// Holds is any_input when the configuration or the statement's text
	// proves the claim, this_input when only this run's input shows it.
	Holds Holds `json:"holds"`
	// Pipeline is empty for a recommendation about the whole configuration.
	Pipeline string `json:"pipeline,omitempty"`
	// Components are topology refs ("processor/filter/health").
	Components []string `json:"components"`
	// Subject names the item inside the components a rule is about, where
	// one rule can report several: a filter condition's or OTTL statement's
	// text, an attribute (`resource.attributes["k"]`), a sampling policy's
	// name. Rule, pipeline, components and subject identify a
	// recommendation across runs of changed configurations.
	Subject string `json:"subject,omitempty"`
	// Output is what applying the change does to what the exporters
	// receive, as the rule states it.
	Output   Output        `json:"output,omitempty"`
	Summary  string        `json:"summary"`
	Evidence []Evidence    `json:"evidence,omitempty"`
	Benefit  *Benefit      `json:"benefit,omitempty"`
	Change   *ConfigChange `json:"change,omitempty"`
	// Blockers name why a stronger suggestion was not made.
	Blockers []string `json:"blockers,omitempty"`
	// Coverage says what this input did not exercise.
	Coverage []string `json:"coverage,omitempty"`
}

// Evidence points into one report section. Ref is the component ref, with
// "#" and the item inside it where one applies: "#condition[0]",
// "#group[0].statement[2]", "#seq[7]", "#trace[<id>]", "#policy[<name>]"
// (a tail sampling policy by its configured name).
type Evidence struct {
	Section string `json:"section"`
	Ref     string `json:"ref"`
}

// Benefit is the work the run shows a change would save.
type Benefit struct {
	Metric string `json:"metric"`
	Count  int    `json:"count"`
	// Of is the total Count is part of, so it can be weighed: for the
	// record metrics every record a processor of the pipeline received,
	// for evaluations every evaluation of the component's statements or
	// conditions, for resource blocks every block the batch processor
	// sent. Zero when the run does not show it.
	Of int `json:"of,omitempty"`
}

// ConfigChange is a suggested configuration change. Applying it is up to the
// user; nothing changes their file.
type ConfigChange struct {
	Op Op `json:"op"`
	// Component is the configuration ID the change applies to.
	Component string `json:"component,omitempty"`
	// Processors is the suggested processor list of the pipeline (swap).
	Processors []string `json:"processors,omitempty"`
	// Group and Index locate a statement (transform: its group and position
	// in it) or a filter condition (Index only); Text is the statement.
	Group *int   `json:"group,omitempty"`
	Index *int   `json:"index,omitempty"`
	Text  string `json:"text,omitempty"`
	// Path is where the statement or condition is configured in the
	// component: "log_statements[0].statements[1]" (a flat statement list is
	// group 0), "log_conditions[0].conditions[1]" or "logs.log_record[0]".
	Path string `json:"path,omitempty"`
	// Option and Value are the setting (set_option): Value is YAML, a
	// JSON-encoded value where the rule knows it; an empty Value leaves the
	// value to the user.
	Option string `json:"option,omitempty"`
	Value  string `json:"value,omitempty"`
}

// Input is the evidence the rules read: the configuration's topology alone
// for static rules (inspect), every analysis section for a run.
type Input struct {
	Topology  *topology.Graph
	Stages    []analysis.Stage
	Filtering []*analysis.Filtering
	OTTL      []*ottldebug.Report
	Batching  []*analysis.Batching
	Grouping  []*analysis.Grouping
	Sampling  []*analysis.Sampling
	// Placements are what the run shows about moving each filter earlier.
	Placements []Placement
	// DeadWrites are attribute writes a later processor undid on every
	// record they reached.
	DeadWrites []DeadWrite
	// ContextLosses are client context reads a batch processor before the
	// reader does not pass on; computed from the configuration, so static
	// recommendations have them too.
	ContextLosses []ContextLoss
	// ExporterBatches are the exporters that batch in their own sending
	// queue; computed from the configuration.
	ExporterBatches []ExporterBatch
}

type rule struct {
	id string
	// output is what applying the rule's change does to the exporters'
	// records (Recommendation.Output); empty for a rule that suggests no
	// change.
	output Output
	run    func(Input) []Recommendation
}

// rules in catalog order, which is also the order recommendations are listed
// in.
var rules = []rule{
	{"config.memory_limiter_not_first", OutputUnchanged, memoryLimiterNotFirst},
	{"config.unused_component", OutputUnchanged, unusedComponent},
	{"batch.chained", OutputRegrouped, batchChained},
	{"batch.exporter_batches", OutputRegrouped, batchBeforeExporterBatching},
	{"filter.no_effect", "", filterNoEffect},
	{"filter.condition_never_matched", "", conditionNeverMatched},
	{"filter.after_batch", OutputUnchanged, filterAfterBatch},
	{"filter.move_earlier", OutputUnchanged, filterMoveEarlier},
	{"ottl.statement_never_matched", "", statementNeverMatched},
	{"ottl.statement_no_change", OutputUnchanged, statementNoChange},
	{"ottl.duplicate_statement", OutputUnchanged, duplicateStatement},
	{"attribute.dead_write", OutputUnchanged, attributeDeadWrite},
	{"processor.no_effect", OutputUnchanged, processorNoEffect},
	{"grouping.no_effect", OutputUnchanged, groupingNoEffect},
	{"batch.size_never_reached", OutputRegrouped, sizeNeverReached},
	{"batch.tiny_batchers", OutputRegrouped, tinyBatchers},
	{"batch.drops_client_context", OutputChanged, batchDropsClientContext},
	{"grouping.after_batch", OutputRegrouped, groupingAfterBatch},
	{"sampling.late_spans", OutputChanged, lateSpans},
	{"sampling.policy_never_decides", "", policyNeverDecides},
	{"sampling.policy_shadowed", OutputUnchanged, policyShadowed},
	{"sampling.late_in_pipeline", OutputUnchanged, samplingLateInPipeline},
}

// Recommend applies every rule. Rules whose sections are absent produce
// nothing, so the topology alone gives the static recommendations.
func Recommend(in Input) []Recommendation {
	out := []Recommendation{}
	if in.Topology == nil {
		return out
	}
	for _, r := range rules {
		found := r.run(in)
		for i := range found {
			found[i].Rule = r.id
			found[i].Output = r.output
			if found[i].Holds == "" {
				found[i].Holds = HoldsThisInput
				if found[i].Confidence == ConfidenceStatic {
					found[i].Holds = HoldsAnyInput
				}
			}
			if b := found[i].Benefit; b != nil && b.Of == 0 {
				b.Of = benefitTotal(in, r.id, found[i])
			}
			if found[i].Components == nil {
				found[i].Components = []string{}
			}
		}
		slices.SortStableFunc(found, func(a, b Recommendation) int {
			return cmp.Or(cmp.Compare(a.Pipeline, b.Pipeline), slices.Compare(a.Components, b.Components))
		})
		out = append(out, found...)
	}
	return out
}

// Static is the recommendations the configuration alone supports.
func Static(g *topology.Graph, losses []ContextLoss, exporters []ExporterBatch) []Recommendation {
	return Recommend(Input{Topology: g, ContextLosses: losses, ExporterBatches: exporters})
}

// processorRef is the topology ref of a processor ID.
func processorRef(id string) string { return string(topology.KindProcessor) + "/" + id }

// componentType is the type part of a component ID ("filter/health" is
// "filter").
func componentType(id string) string {
	t, _, _ := strings.Cut(id, "/")
	return t
}

// pipelineIDs are a pipeline's processors as configuration IDs.
func pipelineIDs(p topology.Pipeline) []string {
	out := make([]string, len(p.Processors))
	for i, ref := range p.Processors {
		out[i] = strings.TrimPrefix(ref, string(topology.KindProcessor)+"/")
	}
	return out
}

func pipeline(g *topology.Graph, id string) (topology.Pipeline, bool) {
	for _, p := range g.Pipelines {
		if p.ID == id {
			return p, true
		}
	}
	return topology.Pipeline{}, false
}

func ptr[T any](v T) *T { return &v }

// plural is "1 record" or "3 records".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// itemNoun names a signal's items: "log record", "span", "data point".
func itemNoun(signal string) string {
	switch signal {
	case "traces":
		return "span"
	case "metrics":
		return "data point"
	case "logs":
		return "log record"
	}
	return "item"
}

// batches reports whether processor id joins requests into batches: a
// batch processor, or a queue_batch processor unless the run's batching
// evidence shows it without a batch section (it then queues and sends every
// request as it is). Without a run every queue_batch counts.
func batches(in Input, pipeline, id string) bool {
	if !analysis.IsBatcher(componentType(id)) {
		return false
	}
	for _, b := range in.Batching {
		if b != nil && b.Processor == id && (pipeline == "" || b.Pipeline == pipeline) && b.Settings.Unbatched {
			return false
		}
	}
	return true
}
