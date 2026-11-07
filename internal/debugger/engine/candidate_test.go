package engine

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// candidateConfig has every list shape a removal rewrites: grouped and flat
// transform statements, a filter's *_conditions groups and its deprecated
// per-context lists.
const candidateConfig = `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  transform/grouped:
    log_statements:
      - context: log
        statements:
          - set(log.attributes["a"], "1")
          - set(log.attributes["b"], "<2>")
      - context: resource
        statements: ['set(resource.attributes["c"], "3")']
  transform/flat:
    trace_statements:
      - set(span.attributes["a"], "1")
      - set(span.attributes["b"], "2")
  filter/groups:
    log_conditions:
      - context: log
        conditions: ['log.severity_text == "DEBUG"', 'log.body == "ping"']
  filter/legacy:
    logs:
      log_record: ['severity_text == "DEBUG"', 'body == "ping"']
  batch: {}
exporters: {otlp: {endpoint: example.invalid:4317}}
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform/grouped, filter/groups, filter/legacy, batch], exporters: [otlp]}
    traces: {receivers: [otlp], processors: [transform/flat], exporters: [otlp]}
`

// TestCandidateOverridesExpressEachChange: a swap overrides the pipeline's
// processors; a removed statement or condition replaces the whole list
// holding it, from the root configuration's text (a group it empties goes
// too); an option with a value overrides its key. A change without a value,
// a statement whose text differs, or an unknown location is refused.
func TestCandidateOverridesExpressEachChange(t *testing.T) {
	prepared, err := Prepare(context.Background(), Request{ConfigYAML: candidateConfig, Fixture: tracesFixture, Receiver: "otlp"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	n := func(i int) *int { return &i }
	remove := func(pipeline, component string, group *int, index int, text, path string) recommend.Recommendation {
		return recommend.Recommendation{Pipeline: pipeline, Change: &recommend.ConfigChange{Op: recommend.OpRemoveStatement, Component: component, Group: group, Index: n(index), Text: text, Path: path}}
	}
	option := func(component, key, value string) recommend.Recommendation {
		return recommend.Recommendation{Pipeline: "logs", Change: &recommend.ConfigChange{Op: recommend.OpSetOption, Component: component, Option: key, Value: value}}
	}
	cases := []struct {
		name string
		rec  recommend.Recommendation
		want []string
	}{
		{"swap", recommend.Recommendation{Pipeline: "logs/main", Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"filter/a", "batch"}}},
			[]string{`yaml:service::pipelines::logs/main::processors: ["filter/a","batch"]`}},
		{"grouped statement", remove("logs", "transform/grouped", n(0), 1, `set(log.attributes["b"], "<2>")`, "log_statements[0].statements[1]"),
			[]string{`yaml:processors::transform/grouped::log_statements: [{"context":"log","statements":["set(log.attributes[\"a\"], \"1\")"]},{"context":"resource","statements":["set(resource.attributes[\"c\"], \"3\")"]}]`}},
		{"last of a group", remove("logs", "transform/grouped", n(1), 0, `set(resource.attributes["c"], "3")`, "log_statements[1].statements[0]"),
			[]string{`yaml:processors::transform/grouped::log_statements: [{"context":"log","statements":["set(log.attributes[\"a\"], \"1\")","set(log.attributes[\"b\"], \"<2>\")"]}]`}},
		{"flat statement", remove("traces", "transform/flat", n(0), 0, `set(span.attributes["a"], "1")`, "trace_statements[0].statements[0]"),
			[]string{`yaml:processors::transform/flat::trace_statements: ["set(span.attributes[\"b\"], \"2\")"]`}},
		{"condition group", remove("logs", "filter/groups", nil, 1, `log.body == "ping"`, "log_conditions[0].conditions[1]"),
			[]string{`yaml:processors::filter/groups::log_conditions: [{"conditions":["log.severity_text == \"DEBUG\""],"context":"log"}]`}},
		{"legacy condition", remove("logs", "filter/legacy", nil, 0, `severity_text == "DEBUG"`, "logs.log_record[0]"),
			[]string{`yaml:processors::filter/legacy::logs::log_record: ["body == \"ping\""]`}},
		{"option", option("batch", "metadata_keys", `["tenant","region"]`),
			[]string{`yaml:processors::batch::metadata_keys: ["tenant","region"]`}},
		{"scalar option", option("batch", "timeout", "5s"), []string{`yaml:processors::batch::timeout: "5s"`}},
		{"no change", recommend.Recommendation{Pipeline: "logs"}, nil},
		{"option without value", option("tail_sampling", "decision_wait", ""), nil},
		{"option key", option("batch", "timeout: 1s\nx", "5s"), nil},
		{"stale text", remove("logs", "transform/grouped", n(0), 0, `set(log.attributes["a"], "2")`, "log_statements[0].statements[0]"), nil},
		{"unknown group", remove("logs", "transform/grouped", n(5), 0, `x`, "log_statements[5].statements[0]"), nil},
		{"no path", remove("logs", "transform/grouped", n(0), 0, `x`, ""), nil},
		{"flat group", remove("traces", "transform/flat", n(1), 0, `x`, "trace_statements[1].statements[0]"), nil},
		{"condition of a transform", remove("logs", "transform/grouped", nil, 0, `x`, "log_conditions[0].conditions[0]"), nil},
		{"mixed path", remove("logs", "filter/groups", nil, 0, `x`, "log_conditions[0].statements[0]"), nil},
		{"unknown path", remove("logs", "filter/groups", nil, 0, `x`, "somewhere[0]"), nil},
		{"not configured", remove("logs", "transform/missing", n(0), 0, `x`, "log_statements[0].statements[0]"), nil},
		{"not a statement list", remove("logs", "batch", n(0), 0, `x`, "log_statements[0].statements[0]"), nil},
		{"no pipeline", recommend.Recommendation{Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"batch"}}}, nil},
		{"separator", recommend.Recommendation{Pipeline: "logs", Change: &recommend.ConfigChange{Op: recommend.OpSwap, Processors: []string{"a::b"}}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := CandidateOverrides(c.rec, raw)
			if c.want == nil {
				if !errors.Is(err, ErrNotVerifiable) {
					t.Fatalf("overrides = %v, %v; want ErrNotVerifiable", got, err)
				}
				return
			}
			if err != nil || !slices.Equal(got, c.want) {
				t.Fatalf("overrides = %v, %v; want %v", got, err, c.want)
			}
			// The candidate resolves: the override is valid configuration.
			if _, err := CandidateRequest(context.Background(), raw, got); c.name != "swap" && err != nil {
				t.Fatalf("candidate: %v", err)
			}
		})
	}
}
