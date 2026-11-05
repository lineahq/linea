package engine

import (
	"context"
	"fmt"
	"testing"
)

func TestEmptyInputsRetainSignalThroughExecutionAndAnalysis(t *testing.T) {
	for _, signal := range []struct{ name, key string }{{"traces", "resourceSpans"}, {"metrics", "resourceMetrics"}, {"logs", "resourceLogs"}} {
		for _, mode := range []string{"quick", "scenario"} {
			t.Run(signal.name+"/"+mode, func(t *testing.T) {
				cfg := fmt.Sprintf("receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {%s: {receivers: [otlp], exporters: [otlp]}}}\n", signal.name)
				fixture := fmt.Sprintf(`{"%s":[]}`, signal.key)
				req := Request{ConfigYAML: cfg, FixtureJSON: fixture, Compare: true, Normalize: true}
				if mode == "scenario" {
					req.FixtureJSON = ""
					req.ScenarioYAML = fmt.Sprintf("version: 1\ninputs: [{data: %q}]\n", fixture)
				}
				report, err := Execute(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if report.Result.Status != "completed" || !report.Comparison.Equal {
					t.Fatalf("empty input result: %s, %+v", report.Result.Status, report.Comparison)
				}
				if len(report.Result.Snapshots) == 0 {
					t.Fatal("no recorded empty input")
				}
				for ref := range report.Result.Snapshots {
					payload, err := report.Result.Snapshots.Payload(ref)
					if err != nil || payload.Signal.String() != signal.name || payload.Counts().Items != 0 {
						t.Fatal("empty snapshot lost its signal", err)
					}
				}
			})
		}
	}
}
