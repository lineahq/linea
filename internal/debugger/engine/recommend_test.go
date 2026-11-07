package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/analysis"
	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/recommend"
)

// TestReportRecommendsFromItsEvidence runs samples whose evidence supports a
// recommendation: broken-routing's where clause never matches in the shared
// transform (one recommendation for both pipelines), and trace-sampling has
// spans that arrive after their trace's decision.
func TestReportRecommendsFromItsEvidence(t *testing.T) {
	cases := []struct {
		sample, rule, pipeline string
	}{
		{"broken-routing", "ottl.statement_never_matched", ""},
		{"trace-sampling", "sampling.late_spans", "traces"},
	}
	for _, c := range cases {
		t.Run(c.sample, func(t *testing.T) {
			dir := filepath.Join("..", "..", "..", "samples", c.sample)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{Config: filepath.Join(dir, "config.yaml"), Scenario: filepath.Join(dir, "scenario.yaml")})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Recommendations) != 1 {
				t.Fatalf("recommendations = %+v", rep.Recommendations)
			}
			r := rep.Recommendations[0]
			if r.Rule != c.rule || r.Pipeline != c.pipeline || len(r.Evidence) == 0 || r.Change == nil {
				t.Fatalf("recommendation = %+v", r)
			}
		})
	}
}

// TestInspectionRecommendsFromTheConfigurationOnly: inspect gives the
// static recommendations, without a run.
func TestInspectionRecommendsFromTheConfigurationOnly(t *testing.T) {
	ins, err := Inspect(context.Background(), InspectRequest{ConfigYAML: `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
processors:
  batch:
  memory_limiter:
    check_interval: 1s
    limit_mib: 100
  attributes:
    actions:
      - key: a
        action: delete
exporters:
  debug:
service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch, memory_limiter]
      exporters: [debug]
`})
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, r := range ins.Recommendations {
		rules = append(rules, r.Rule)
	}
	if len(rules) != 2 || rules[0] != "config.memory_limiter_not_first" || rules[1] != "config.unused_component" {
		t.Fatalf("rules = %q (valid %v, diagnostics %+v)", rules, ins.Valid, ins.Diagnostics)
	}
}

// TestFilterMovesEarlierOnlyWhereItsVerdictHolds runs samples/logs-cleanup:
// filter/health reads only the body, so its verdict is the same before
// resource and attributes/scrub and it may move first. A variant that also
// reads the attribute resource sets keeps the same verdict before
// attributes/scrub only, and names resource as the blocker. So does one
// reading an attribute resource is configured to insert but, on this input,
// never changed.
func TestFilterMovesEarlierOnlyWhereItsVerdictHolds(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dependent := strings.Replace(string(raw), `- IsMatch(body, "^healthcheck")`,
		`- IsMatch(body, "^healthcheck") and resource.attributes["deployment.environment"] == "demo"`, 1)
	if dependent == string(raw) {
		t.Fatal("the sample's condition changed; update the test")
	}
	// resource only inserts service.name, which every input record already
	// has: nothing changes on this input, yet another input may differ.
	configured := strings.Replace(strings.Replace(string(raw), `- IsMatch(body, "^healthcheck")`,
		`- IsMatch(body, "^healthcheck") and resource.attributes["service.name"] != nil`, 1),
		"        action: upsert\n", "        action: upsert\n      - key: service.name\n        value: unknown\n        action: insert\n", 1)
	if strings.Count(configured, "service.name") != 2 {
		t.Fatal("the sample's resource processor changed; update the test")
	}
	cases := []struct {
		name, config string
		order        []string
		blocked      string
	}{
		{"independent", string(raw), []string{"filter/health", "resource", "attributes/scrub", "transform/severity", "batch"}, ""},
		{"dependent", dependent, []string{"resource", "filter/health", "attributes/scrub", "transform/severity", "batch"}, `reads resource.attributes["deployment.environment"], which resource's configuration may set`},
		{"configured", configured, []string{"resource", "filter/health", "attributes/scrub", "transform/severity", "batch"}, `reads resource.attributes["service.name"], which resource's configuration may set`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep, err := Execute(ctx, Request{ConfigYAML: c.config, Scenario: filepath.Join(dir, "scenario.yaml")})
			if err != nil {
				t.Fatal(err)
			}
			var found []recommend.Recommendation
			for _, r := range rep.Recommendations {
				if r.Rule == "filter.move_earlier" {
					found = append(found, r)
				}
			}
			if len(found) != 1 || found[0].Change == nil {
				t.Fatalf("filter.move_earlier = %+v (all %+v)", found, rep.Recommendations)
			}
			r := found[0]
			if !slices.Equal(r.Change.Processors, c.order) || r.Confidence != recommend.ConfidenceObserved {
				t.Fatalf("change = %v, confidence %s", r.Change.Processors, r.Confidence)
			}
			if c.blocked == "" && len(r.Blockers) != 0 {
				t.Fatalf("blockers = %q, want none: the filter can move to the pipeline's start", r.Blockers)
			}
			if c.blocked != "" && (len(r.Blockers) != 1 || !strings.Contains(r.Blockers[0], c.blocked)) {
				t.Fatalf("blockers = %q, want one containing %q", r.Blockers, c.blocked)
			}
		})
	}
}

// TestTouchesMatchesReadsToRecordedChanges: a change touches a read at the
// same level and field, with the same key when both have one.
func TestTouchesMatchesReadsToRecordedChanges(t *testing.T) {
	rec, res := features.LevelRecord, features.LevelResource
	cases := []struct {
		read   features.FieldRead
		entity string
		path   string
		want   bool
	}{
		{features.FieldRead{Level: rec, Field: "attributes", Key: "route"}, "resource[0].scope[0].log_record[1]", `attributes["route"]`, true},
		{features.FieldRead{Level: rec, Field: "attributes", Key: "route"}, "resource[0].scope[0].log_record[1]", `attributes["other"]`, false},
		{features.FieldRead{Level: rec, Field: "attributes"}, "resource[0].scope[0].log_record[1]", `attributes["other"]`, true},
		{features.FieldRead{Level: res, Field: "attributes", Key: "env"}, "resource[0]", `attributes["env"]`, true},
		{features.FieldRead{Level: rec, Field: "attributes", Key: "env"}, "resource[0]", `attributes["env"]`, false},
		{features.FieldRead{Level: rec, Field: "time"}, "resource[0].scope[0].log_record[0]", "time_unix_nano", true},
		{features.FieldRead{Level: rec, Field: "status"}, "resource[0].scope[0].span[0]", "status.code", true},
		{features.FieldRead{Level: rec, Field: "body"}, "resource[0].scope[0].log_record[0]", "severity_text", false},
	}
	for _, c := range cases {
		if got := touches(c.read, analysis.Change{Entity: c.entity, Path: c.path}); got != c.want {
			t.Errorf("touches(%+v, %s %s) = %v, want %v", c.read, c.entity, c.path, got, c.want)
		}
	}
}
