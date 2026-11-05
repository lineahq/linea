package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/topology"
)

// standInConfig uses one type of every kind the distribution lacks: a
// receiver, a processor, a connector between two traces pipelines and into a
// metrics pipeline, an exporter and an extension.
const standInConfig = `extensions:
  sigv4auth: {region: eu-west-1}
receivers:
  otlp: {protocols: {grpc: {}}}
  kafka: {brokers: [kafka:9092], topic: logs}
processors:
  geoip: {providers: {maxmind: {database_path: /etc/geoip/city.mmdb}}}
connectors:
  servicegraph: {store: {ttl: 2s}}
exporters:
  datadog: {api: {key: secret-key}}
  otlp/logs: {endpoint: logs.example.com:4317}
  otlp/metrics: {endpoint: metrics.example.com:4317}
service:
  extensions: [sigv4auth]
  pipelines:
    traces/in: {receivers: [otlp], processors: [geoip], exporters: [servicegraph]}
    traces/out: {receivers: [servicegraph], exporters: [datadog]}
    metrics/graph: {receivers: [servicegraph], exporters: [otlp/metrics]}
    logs: {receivers: [kafka], exporters: [otlp/logs]}
`

const standInScenario = `version: 1
inputs:
  - receiver: otlp
    data: '{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0102030405060708","name":"checkout"}]}]}]}'
  - receiver: kafka
    data: '{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"paid"}}]}]}]}'
assertions:
  - name: logs pass known components only
    target: {exporter: otlp/logs, signal: logs}
    count: {equals: 1}
  - name: traces pass the stand-in processor and connector
    target: {exporter: datadog, signal: traces}
    count: {equals: 1}
  - name: the cross-signal stand-in emits nothing
    target: {exporter: otlp/metrics, signal: metrics}
    count: {equals: 0}
  - name: the stand-in processor itself
    target: {processor: geoip, signal: traces}
    dropped: {equals: 0}
`

// TestUnknownComponentsRunAsStandIns: every unknown type validates with a
// warning and runs; assertions on data that passed a stand-in processor or
// connector are inconclusive, while one on an unaffected pipeline passes.
func TestUnknownComponentsRunAsStandIns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pol := Policy{Paths: hostfs.Policy{Unrestricted: true}}

	ins, err := InspectWithPolicy(ctx, InspectRequest{ConfigYAML: standInConfig}, pol)
	if err != nil || !ins.Valid {
		t.Fatalf("inspection %+v, %v", ins, err)
	}
	warned := map[string]bool{}
	for _, d := range ins.Diagnostics {
		if d.Code == config.CodeUnsupportedComponent {
			if d.Severity != config.SeverityWarning {
				t.Errorf("%s: severity %s", d.Component, d.Severity)
			}
			warned[d.Component] = true
		}
	}
	modes := map[string]topology.Mode{}
	for _, c := range ins.Topology.Components {
		modes[c.Ref] = c.Mode
	}
	for _, ref := range []string{"extension/sigv4auth", "receiver/kafka", "processor/geoip", "connector/servicegraph", "exporter/datadog"} {
		if !warned[ref] || modes[ref] != topology.ModeStandIn {
			t.Errorf("%s: warned %v, mode %s", ref, warned[ref], modes[ref])
		}
	}
	if modes["receiver/otlp"] != topology.ModeSimulated || modes["exporter/otlp/logs"] != topology.ModeCaptured {
		t.Errorf("known components changed modes: %v", modes)
	}
	// Untyped settings are kept, with their values redacted.
	if effective, _ := json.Marshal(ins.Effective); !strings.Contains(string(effective), "datadog") || strings.Contains(string(effective), "secret-key") {
		t.Error("the effective configuration shows a stand-in's setting value")
	}

	req, err := PrepareWithPolicy(ctx, Request{ConfigYAML: standInConfig, ScenarioYAML: standInScenario}, pol)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Result.Completed() {
		t.Fatalf("run %s: %s", rep.Result.Status, rep.Result.StatusReason)
	}
	got := map[string]assertion.Result{}
	for _, r := range rep.Assertions.Results {
		got[r.Name] = r
	}
	if r := got["logs pass known components only"]; r.Outcome != assertion.Pass {
		t.Errorf("unaffected pipeline: %+v", r)
	}
	for name, causes := range map[string][]string{
		"traces pass the stand-in processor and connector": {"processor/geoip (type geoip is not in the distribution)", "connector/servicegraph"},
		"the cross-signal stand-in emits nothing":          {"connector/servicegraph"},
		"the stand-in processor itself":                    {"processor/geoip"},
	} {
		r := got[name]
		if r.Outcome != assertion.Inconclusive || len(r.Checks) != 1 {
			t.Errorf("%s: %+v", name, r)
			continue
		}
		for _, cause := range causes {
			if !strings.Contains(r.Checks[0].Message, cause) {
				t.Errorf("%s: %q lacks %s", name, r.Checks[0].Message, cause)
			}
		}
	}
	// The data still flowed: the stand-ins forwarded it.
	if r := got["traces pass the stand-in processor and connector"]; r.Checks[0].Observed != "1" {
		t.Errorf("datadog observed %s traces", r.Checks[0].Observed)
	}
}

// TestStandInValuesMakeDownstreamAssertionsInconclusive: a real processor
// configured with a stand-in string runs, but what it did is not what it
// would do with the real value.
func TestStandInValuesMakeDownstreamAssertionsInconclusive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"processors:\n  transform:\n    log_statements:\n      - context: log\n        statements:\n" +
		"          - set(log.attributes[\"region\"], \"${env:OTEL_STAND_IN_REGION}\")\n" +
		"exporters: {otlp/a: {endpoint: a.example.com:4317}, otlp/b: {endpoint: b.example.com:4317}}\n" +
		"service:\n  pipelines:\n    logs/a: {receivers: [otlp], processors: [transform], exporters: [otlp/a]}\n" +
		"    logs/b: {receivers: [otlp], exporters: [otlp/b]}\n"
	scen := "version: 1\ninputs:\n  - data: '{\"resourceLogs\":[{\"scopeLogs\":[{\"logRecords\":[{\"body\":{\"stringValue\":\"x\"}}]}]}]}'\n" +
		"assertions:\n  - {name: a, target: {exporter: otlp/a, signal: logs}, count: {equals: 1}}\n" +
		"  - {name: b, target: {exporter: otlp/b, signal: logs}, count: {equals: 1}}\n"
	req, err := PrepareWithPolicy(ctx, Request{ConfigYAML: cfg, ScenarioYAML: scen}, Policy{Paths: hostfs.Policy{Unrestricted: true}})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := rep.Topology.Component("processor/transform")
	if len(c.StandInValues) != 1 || c.StandInValues[0] != "env:OTEL_STAND_IN_REGION" {
		t.Fatalf("stand-in values %v", c.StandInValues)
	}
	a, b := rep.Assertions.Results[0], rep.Assertions.Results[1]
	if a.Outcome != assertion.Inconclusive || !strings.Contains(a.Checks[0].Message, "env:OTEL_STAND_IN_REGION") || b.Outcome != assertion.Pass {
		t.Fatalf("assertions %+v", rep.Assertions.Results)
	}
}
