package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
)

// TestRunsResolveReferencesFromTheScenarioOrStandIns: a ${file:} reference
// naming a mocks.files path reads the scenario's content, an unbound
// ${env:} name stands in, the run completes with both values, and the
// report and inspection say where each value came from.
func TestRunsResolveReferencesFromTheScenarioOrStandIns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"processors:\n  transform:\n    log_statements:\n      - context: log\n        statements:\n" +
		"          - set(log.attributes[\"token\"], \"${file:/run/lab/token}\")\n" +
		"          - set(log.attributes[\"region\"], \"${env:OTEL_REFERENCE_UNBOUND}\")\n" +
		"exporters: {debug: {}}\n" +
		"service: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [debug]}}}\n"
	scen := "version: 1\nmocks:\n  files:\n    - path: /run/lab/token\n      content: from-scenario\n" +
		"inputs:\n  - data: '{\"resourceLogs\":[{\"scopeLogs\":[{\"logRecords\":[{\"body\":{\"stringValue\":\"x\"}}]}]}]}'\n"
	pol := Policy{Paths: hostfs.Policy{Unrestricted: true}}
	req, err := PrepareWithPolicy(ctx, Request{ConfigYAML: cfg, ScenarioYAML: scen}, pol)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Result.Completed() {
		t.Fatal("run did not complete")
	}
	got := map[string]config.Reference{}
	for _, r := range rep.References {
		got[r.Scheme+":"+r.Target] = r
	}
	file, env := got["file:/run/lab/token"], got["env:OTEL_REFERENCE_UNBOUND"]
	if len(got) != 2 || file.Source != config.SourceScenario || env.Source != config.SourceStandIn || env.Reason != config.ReasonUnbound ||
		len(env.Components) != 1 || env.Components[0] != "processor/transform" {
		t.Fatalf("references %+v", rep.References)
	}
	var exported string
	for _, snap := range rep.Result.Snapshots {
		exported += string(snap)
	}
	if !strings.Contains(exported, "from-scenario") || !strings.Contains(exported, env.StandIn) {
		t.Fatal("the run did not use the scenario value and the stand-in")
	}

	ins, err := InspectWithPolicy(ctx, InspectRequest{ConfigYAML: cfg, ScenarioYAML: scen}, pol)
	if err != nil || !ins.Valid {
		t.Fatalf("inspection %+v, %v", ins, err)
	}
	var warned int
	for _, d := range ins.Diagnostics {
		if d.Code == config.CodeReferenceStandIn {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("diagnostics %+v", ins.Diagnostics)
	}
}
