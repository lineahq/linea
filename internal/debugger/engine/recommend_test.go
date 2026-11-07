package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
