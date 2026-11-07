package recommend

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/ottldebug"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"github.com/lineahq/linea/internal/debugger/topology"
)

func graph(pipelines map[string][]string, unused ...string) *topology.Graph {
	g := &topology.Graph{Unused: unused}
	seen := map[string]bool{}
	for _, id := range slices.Sorted(maps.Keys(pipelines)) {
		p := topology.Pipeline{ID: id, Signal: strings.Split(id, "/")[0]}
		for _, proc := range pipelines[id] {
			ref := processorRef(proc)
			p.Processors = append(p.Processors, ref)
			if !seen[ref] {
				seen[ref] = true
				g.Components = append(g.Components, topology.Component{Ref: ref, Kind: topology.KindProcessor, ID: proc, Type: componentType(proc), Mode: topology.ModeReal})
			}
		}
		g.Pipelines = append(g.Pipelines, p)
	}
	return g
}

func rulesOf(recs []Recommendation) []string {
	out := []string{}
	for _, r := range recs {
		out = append(out, r.Rule+" "+strings.Join(r.Components, ","))
	}
	return out
}

// TestStaticRulesReadTheTopologyOnly: memory_limiter placement, unused
// components and chained batch processors come from the configuration alone.
func TestStaticRulesReadTheTopologyOnly(t *testing.T) {
	g := graph(map[string][]string{
		"logs":   {"attributes", "memory_limiter", "batch", "batch/2"},
		"traces": {"memory_limiter", "batch"},
	}, "exporter/unused")
	recs := Static(g, nil)
	want := []string{
		"config.memory_limiter_not_first processor/memory_limiter",
		"config.unused_component exporter/unused",
		"batch.chained processor/batch,processor/batch/2",
	}
	if got := rulesOf(recs); !slices.Equal(got, want) {
		t.Fatalf("rules = %q, want %q", got, want)
	}
	if c := recs[0].Change; c == nil || c.Op != OpSwap || !slices.Equal(c.Processors, []string{"memory_limiter", "attributes", "batch", "batch/2"}) {
		t.Fatalf("memory_limiter change = %+v", recs[0].Change)
	}
	for _, r := range recs {
		if r.Confidence != ConfidenceStatic {
			t.Fatalf("%s confidence = %s", r.Rule, r.Confidence)
		}
	}
}

func filtering(records, dropped int, verification analysis.FilterVerification, conds ...analysis.FilterCondition) *analysis.Filtering {
	return &analysis.Filtering{Processor: "filter/health", Pipeline: "logs", Signal: "logs", Verification: verification, Records: records, Dropped: dropped, Conditions: conds}
}

// TestFilterRulesUseVerifiedCounts: a filter that dropped nothing is one
// recommendation; otherwise each condition that never matched in the
// verified requests is, and a filter after batch is suggested before it.
func TestFilterRulesUseVerifiedCounts(t *testing.T) {
	health := analysis.FilterCondition{Index: 0, Source: "logs.log_record[0]", Condition: `IsMatch(body, "^health")`, Evaluated: 4, Matched: 1, Records: 1}
	debug := analysis.FilterCondition{Index: 1, Source: "logs.log_record[1]", Condition: "severity_number < 5", Evaluated: 3}
	cases := []struct {
		name       string
		processors []string
		filtering  []*analysis.Filtering
		want       []string
	}{
		{"no effect", []string{"filter/health"}, []*analysis.Filtering{filtering(4, 0, analysis.FilterVerified, debug)},
			[]string{"filter.no_effect processor/filter/health"}},
		{"a condition never matched", []string{"filter/health"}, []*analysis.Filtering{filtering(4, 1, analysis.FilterVerified, health, debug)},
			[]string{"filter.condition_never_matched processor/filter/health"}},
		{"only unverified requests", []string{"filter/health"}, []*analysis.Filtering{filtering(4, 1, analysis.FilterDiffers, health, debug)},
			[]string{}},
		{"after batch", []string{"batch", "transform", "filter/health"}, []*analysis.Filtering{filtering(4, 1, analysis.FilterVerified, health)},
			[]string{"filter.after_batch processor/batch,processor/filter/health"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := Recommend(Input{Topology: graph(map[string][]string{"logs": c.processors}), Filtering: c.filtering})
			if got := rulesOf(recs); !slices.Equal(got, c.want) {
				t.Fatalf("rules = %q, want %q", got, c.want)
			}
		})
	}
	recs := Recommend(Input{Topology: graph(map[string][]string{"logs": {"batch", "transform", "filter/health"}}), Filtering: []*analysis.Filtering{filtering(4, 1, analysis.FilterVerified, health)}})
	if c := recs[0].Change; !slices.Equal(c.Processors, []string{"filter/health", "batch", "transform"}) || len(recs[0].Blockers) != 1 {
		t.Fatalf("after batch = %+v, blockers %q", c, recs[0].Blockers)
	}
	recs = Recommend(Input{Topology: graph(map[string][]string{"logs": {"filter/health"}}), Filtering: []*analysis.Filtering{filtering(4, 1, analysis.FilterVerified, health, debug)}})
	if c := recs[0].Change; c.Op != OpRemoveStatement || *c.Index != 1 || c.Text != "severity_number < 5" || recs[0].Benefit.Count != 3 || recs[0].Benefit.Of != 7 {
		t.Fatalf("condition change = %+v, benefit %+v", c, recs[0].Benefit)
	}
}

// TestStatementRulesSeparateNeverMatchedUnchangedAndDuplicates: counts are
// summed over verified replays; a repeated statement is a duplicate, not
// also an unchanged one; cache writes are left out.
func TestStatementRulesSeparateNeverMatchedUnchangedAndDuplicates(t *testing.T) {
	stmt := func(i int, text string, matched, notMatched, unchanged int) ottldebug.Statement {
		return ottldebug.Statement{Index: i, Text: text, Matched: matched, NotMatched: notMatched, Unchanged: unchanged}
	}
	replay := func(input int, v ottldebug.Verification, stmts ...ottldebug.Statement) *ottldebug.Report {
		return &ottldebug.Report{Processor: "transform", Input: input, Signal: "logs", Verification: v,
			Groups: []ottldebug.Group{{Index: 0, Context: "log", Status: ottldebug.StatusReplayed, Statements: stmts}}}
	}
	statements := func(input int, v ottldebug.Verification) *ottldebug.Report {
		return replay(input, v,
			stmt(0, `set(attributes["a"], "x") where body == "never"`, 0, 2, 0),
			stmt(1, `set(attributes["env"], "prod")`, 2, 0, 2),
			stmt(2, `set(severity_text, "INFO")`, 2, 0, 0),
			stmt(3, `set(severity_text, "INFO")`, 2, 0, 2),
			stmt(4, `set(cache["x"], body)`, 2, 0, 2),
		)
	}
	in := Input{
		Topology: graph(map[string][]string{"logs": {"transform"}}),
		Stages: []analysis.Stage{{Component: "transform", Pipeline: "logs", Kind: analysis.KindProcessor, Type: "transform",
			Inputs: []analysis.StageInput{{Seq: 3}, {Seq: 5}, {Seq: 7}}}},
		OTTL: []*ottldebug.Report{statements(3, ottldebug.VerificationReplayVerified), statements(5, ottldebug.VerificationReplayVerified), statements(7, ottldebug.VerificationDisagrees)},
	}
	recs := Recommend(in)
	want := []string{
		"ottl.statement_never_matched processor/transform",
		"ottl.statement_no_change processor/transform",
		"ottl.duplicate_statement processor/transform",
	}
	if got := rulesOf(recs); !slices.Equal(got, want) {
		t.Fatalf("rules = %q, want %q", got, want)
	}
	for i, idx := range []int{0, 1, 3} {
		r := recs[i]
		if *r.Change.Index != idx || r.Pipeline != "logs" || len(r.Coverage) != 2 {
			t.Fatalf("%s: change %+v, pipeline %q, coverage %q", r.Rule, r.Change, r.Pipeline, r.Coverage)
		}
	}
	// Of counts every statement's evaluations in the two verified replays.
	if recs[0].Benefit.Count != 4 || recs[1].Benefit.Count != 4 || recs[0].Benefit.Of != 20 || recs[1].Benefit.Of != 20 {
		t.Fatalf("benefits = %+v, %+v", recs[0].Benefit, recs[1].Benefit)
	}
	for _, r := range recs {
		if r.Output != OutputUnchanged {
			t.Fatalf("%s output = %q", r.Rule, r.Output)
		}
	}
}

// TestSharedTransformIsOneRecommendation: a processor used by two pipelines
// has one configuration, so its statements are counted over both, and one
// that matched in either pipeline is not suggested.
func TestSharedTransformIsOneRecommendation(t *testing.T) {
	replay := func(input int, matched, notMatched int) *ottldebug.Report {
		return &ottldebug.Report{Processor: "transform", Input: input, Signal: "logs", Verification: ottldebug.VerificationReplayVerified,
			Groups: []ottldebug.Group{{Context: "log", Status: ottldebug.StatusReplayed, Statements: []ottldebug.Statement{
				{Index: 0, Text: "a", NotMatched: 2},
				{Index: 1, Text: "b", Matched: matched, NotMatched: notMatched, Unchanged: 0},
			}}}}
	}
	in := Input{
		Topology: graph(map[string][]string{"logs/a": {"transform"}, "logs/b": {"transform"}}),
		Stages: []analysis.Stage{
			{Component: "transform", Pipeline: "logs/a", Kind: analysis.KindProcessor, Inputs: []analysis.StageInput{{Seq: 3}}},
			{Component: "transform", Pipeline: "logs/b", Kind: analysis.KindProcessor, Inputs: []analysis.StageInput{{Seq: 4}}},
		},
		OTTL: []*ottldebug.Report{replay(3, 0, 2), replay(4, 1, 1)},
	}
	recs := Recommend(in)
	if len(recs) != 1 || recs[0].Rule != "ottl.statement_never_matched" || recs[0].Pipeline != "" || *recs[0].Change.Index != 0 {
		t.Fatalf("recs = %+v", recs)
	}
	if !strings.Contains(recs[0].Summary, "in pipelines logs/a, logs/b") || recs[0].Benefit.Count != 4 {
		t.Fatalf("summary = %q, benefit %+v", recs[0].Summary, recs[0].Benefit)
	}
}

// TestStageRulesNeedEveryInputUnchanged:processor.no_effect needs every
// input emitted without a difference, from a real component of a type
// without its own rule; groupbyattrs needs no moved key and no merge.
func TestStageRulesNeedEveryInputUnchanged(t *testing.T) {
	same := &analysis.Diff{Before: telemetry.Counts{Items: 3}, After: telemetry.Counts{Items: 3}}
	changed := &analysis.Diff{Before: telemetry.Counts{Items: 3}, After: telemetry.Counts{Items: 2}}
	stage := func(id string, diffs ...*analysis.Diff) analysis.Stage {
		s := analysis.Stage{Component: id, Pipeline: "logs", Kind: analysis.KindProcessor, Type: componentType(id), Signal: "logs", Emission: analysis.EmissionSynchronous}
		for i, d := range diffs {
			s.Inputs = append(s.Inputs, analysis.StageInput{Seq: i + 1, Outcome: analysis.OutcomeEmitted, Diff: d})
		}
		return s
	}
	out := 2
	in := Input{
		Topology: graph(map[string][]string{"logs": {"attributes/a", "attributes/b", "batch", "groupbyattrs", "groupbyattrs/merge"}}),
		Stages:   []analysis.Stage{stage("attributes/a", same, same), stage("attributes/b", same, changed), stage("batch", same)},
		Grouping: []*analysis.Grouping{
			{Processor: "groupbyattrs", Pipeline: "logs", Signal: "logs", Keys: []string{"host.name"}, Requests: []analysis.GroupRequest{
				{Input: 1, Output: &out, Before: make([]analysis.GroupBlockIn, 1), After: make([]analysis.GroupBlockOut, 1), Kept: 3}}},
			{Processor: "groupbyattrs/merge", Pipeline: "logs", Signal: "logs", Requests: []analysis.GroupRequest{
				{Input: 1, Output: &out, Before: make([]analysis.GroupBlockIn, 2), After: make([]analysis.GroupBlockOut, 1), Kept: 3}}},
		},
	}
	want := []string{"processor.no_effect processor/attributes/a", "grouping.no_effect processor/groupbyattrs"}
	recs := Recommend(in)
	if got := rulesOf(recs); !slices.Equal(got, want) {
		t.Fatalf("rules = %q, want %q", got, want)
	}
	// Of is every record the pipeline's processors received: 6 + 6 + 3.
	if recs[0].Benefit.Count != 6 || recs[0].Benefit.Of != 15 {
		t.Fatalf("benefit = %+v", recs[0].Benefit)
	}
}

// TestBatchSizeNeverReachedStatesTheInputRate: with arrivals spread over
// time the summary extrapolates from their rate; arriving at once, it says
// the rate is unknown; a size send means the size was reached.
func TestBatchSizeNeverReachedStatesTheInputRate(t *testing.T) {
	at := func(ms float64) *float64 { return &ms }
	batching := func(flushes ...analysis.BatchFlush) *analysis.Batching {
		return &analysis.Batching{Processor: "batch", Pipeline: "logs", Signal: "logs",
			Settings: analysis.BatchSettings{SendBatchSize: 8192, Timeout: "200ms"},
			Arrivals: []analysis.BatchArrival{{Seq: 1, OffsetMS: at(0), Items: 20}, {Seq: 2, OffsetMS: at(1000), Items: 20}},
			Flushes:  flushes}
	}
	timeout := analysis.BatchFlush{Items: 8, Trigger: analysis.TriggerTimeout}
	cases := []struct {
		name string
		b    *analysis.Batching
		want string
	}{
		{"spread", batching(timeout, timeout, timeout, timeout, timeout), "At this input's rate of about 40 log records/s, send_batch_size 8192 fills after 3m25s; the 200ms timeout sent every batch instead, 8.0 log records per send on average."},
		{"at once", func() *analysis.Batching {
			b := batching(timeout)
			b.Arrivals[1].OffsetMS = at(0)
			return b
		}(), "Every send of batch came from its 200ms timeout, with at most 8 log records against send_batch_size 8192."},
		{"size reached", batching(timeout, analysis.BatchFlush{Items: 8192, Trigger: analysis.TriggerSize}), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := Recommend(Input{Topology: graph(map[string][]string{"logs": {"batch"}}), Batching: []*analysis.Batching{c.b}})
			if c.want == "" {
				if len(recs) != 0 {
					t.Fatalf("recs = %q", rulesOf(recs))
				}
				return
			}
			if len(recs) != 1 || !strings.HasPrefix(recs[0].Summary, c.want) {
				t.Fatalf("recs = %+v, want summary starting %q", recs, c.want)
			}
		})
	}
}

// TestLateSpansSuggestALongerDecisionWait: late spans are counted per
// processor, with the traces as evidence and no value chosen for the user.
func TestLateSpansSuggestALongerDecisionWait(t *testing.T) {
	in := Input{Topology: graph(map[string][]string{"traces": {"tail_sampling"}}), Sampling: []*analysis.Sampling{{
		Processor: "tail_sampling", Pipeline: "traces", DecisionWait: "1s",
		Traces: []analysis.SampledTrace{{TraceID: "a", LateSpans: 2}, {TraceID: "b"}, {TraceID: "c", LateSpans: 1}},
	}}}
	recs := Recommend(in)
	if len(recs) != 1 || recs[0].Rule != "sampling.late_spans" || recs[0].Benefit != nil || len(recs[0].Evidence) != 2 {
		t.Fatalf("recs = %+v", recs)
	}
	if c := recs[0].Change; c.Op != OpSetOption || c.Option != "decision_wait" || c.Value != "" {
		t.Fatalf("change = %+v", c)
	}
	if !strings.HasPrefix(recs[0].Summary, "3 spans of 2 traces reached tail_sampling after") {
		t.Fatalf("summary = %q", recs[0].Summary)
	}
}

// TestFilterMoveEarlierFollowsThePlacementChecks: the filter moves before
// the leading processors whose checks passed and names the first blocker;
// behind a batch, filter.after_batch takes the placement's checks instead of
// its "not checked" blocker.
func TestFilterMoveEarlierFollowsThePlacementChecks(t *testing.T) {
	health := analysis.FilterCondition{Index: 0, Condition: `IsMatch(body, "^health")`, Evaluated: 4, Matched: 1, Records: 1}
	fs := []*analysis.Filtering{filtering(4, 1, analysis.FilterVerified, health)}
	place := func(checks ...PlacementCheck) []Placement {
		return []Placement{{Filter: "filter/health", Pipeline: "logs", Signal: "logs", Requests: 1, Records: 4, Dropped: 1, Checks: checks}}
	}
	cases := []struct {
		name       string
		processors []string
		placements []Placement
		rule       string
		order      []string
		blockers   []string
	}{
		{"to the start", []string{"resource", "attributes", "filter/health"},
			place(PlacementCheck{Processor: "attributes"}, PlacementCheck{Processor: "resource"}),
			"filter.move_earlier processor/filter/health,processor/resource,processor/attributes",
			[]string{"filter/health", "resource", "attributes"}, nil},
		{"up to a blocker", []string{"memory_limiter", "transform", "filter/health"},
			place(PlacementCheck{Processor: "transform"}, PlacementCheck{Processor: "memory_limiter", Blocker: "guards"}),
			"filter.move_earlier processor/filter/health,processor/transform",
			[]string{"memory_limiter", "filter/health", "transform"}, []string{"guards"}},
		{"blocked at once", []string{"transform", "filter/health"},
			place(PlacementCheck{Processor: "transform", Blocker: "reads"}), "", nil, nil},
		{"after batch, checked", []string{"batch", "transform", "filter/health"},
			place(PlacementCheck{Processor: "transform"}, PlacementCheck{Processor: "batch", Blocker: "buffers"}),
			"filter.after_batch processor/batch,processor/filter/health",
			[]string{"filter/health", "batch", "transform"}, nil},
		{"after batch, dependent", []string{"batch", "transform", "filter/health"},
			place(PlacementCheck{Processor: "transform", Blocker: "reads"}),
			"filter.after_batch processor/batch,processor/filter/health",
			[]string{"filter/health", "batch", "transform"}, []string{"reads"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := Recommend(Input{Topology: graph(map[string][]string{"logs": c.processors}), Filtering: fs, Placements: c.placements})
			var found []Recommendation
			for _, r := range recs {
				if r.Rule == "filter.move_earlier" || r.Rule == "filter.after_batch" {
					found = append(found, r)
				}
			}
			if c.rule == "" {
				if len(found) != 0 {
					t.Fatalf("rules = %q, want none", rulesOf(found))
				}
				return
			}
			if got := rulesOf(found); len(got) != 1 || got[0] != c.rule {
				t.Fatalf("rules = %q, want %q", got, c.rule)
			}
			if r := found[0]; !slices.Equal(r.Change.Processors, c.order) || !slices.Equal(r.Blockers, c.blockers) {
				t.Fatalf("change = %v, blockers %q", r.Change.Processors, r.Blockers)
			}
		})
	}
}

// TestEveryRuleStatesItsOutput: each rule says what its change does to the
// exporters' records, and only the batching and grouping rules regroup.
func TestEveryRuleStatesItsOutput(t *testing.T) {
	for _, r := range rules {
		switch r.output {
		case OutputUnchanged, OutputChanged:
			if strings.HasPrefix(r.id, "batch.size") || r.id == "batch.tiny_batchers" {
				t.Errorf("%s output = %s, want regrouped", r.id, r.output)
			}
		case OutputRegrouped:
		default:
			t.Errorf("%s has no output", r.id)
		}
	}
}
