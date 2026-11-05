package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/topology"
)

var multisignalConfig = filepath.Join("..", "..", "..", "testdata", "multisignal", "config.yaml")

// TestInspectDescribesValidConfig checks that the
// unmodified config validates, its topology needs no telemetry, sensitive
// values are redacted and the file is left untouched.
func TestInspectDescribesValidConfig(t *testing.T) {
	before, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := Inspect(context.Background(), InspectRequest{Config: multisignalConfig})
	if err != nil {
		t.Fatal(err)
	}
	if !ins.Valid || len(ins.Diagnostics) != 0 || ins.Topology == nil {
		t.Fatalf("inspection = %+v", ins)
	}
	if len(ins.Config.SHA256) != 64 {
		t.Fatalf("config hash = %q", ins.Config.SHA256)
	}

	modes := map[string]topology.Mode{
		"receiver/otlp":         topology.ModeSimulated,
		"processor/resource":    topology.ModeReal,
		"exporter/otlp/backend": topology.ModeCaptured,
		"exporter/debug":        topology.ModeReal,
	}
	for ref, want := range modes {
		c, ok := ins.Topology.Component(ref)
		if !ok || c.Mode != want {
			t.Errorf("%s: %+v, want mode %s", ref, c, want)
		}
	}

	effective, err := json.Marshal(ins.Effective)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(effective), "secret-token") || !strings.Contains(string(effective), "[REDACTED]") {
		t.Fatalf("sensitive header not redacted: %s", effective)
	}
	if !strings.Contains(string(effective), `"value":"dev"`) {
		t.Fatalf("effective config lacks the resolved env default: %s", effective)
	}

	after, err := os.ReadFile(multisignalConfig)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("inspection modified the configuration")
	}
}

// TestInspectWarnsAboutUnknownComponents: an unknown type keeps the
// configuration valid, with a warning and a stand-in.
func TestInspectWarnsAboutUnknownComponents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "receivers: {kafka: {}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [kafka], exporters: [nop]}}}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	ins, err := Inspect(context.Background(), InspectRequest{Config: path})
	if err != nil {
		t.Fatal(err)
	}
	if !ins.Valid || ins.Topology == nil || len(ins.Diagnostics) != 1 || ins.Diagnostics[0].Severity != config.SeverityWarning ||
		ins.Diagnostics[0].Code != config.CodeUnsupportedComponent || ins.Diagnostics[0].Component != "receiver/kafka" {
		t.Fatalf("inspection = %+v", ins)
	}
}

// TestInspectReportsMissingMockFixtures: a mocked component the scenario
// gives nothing usable to read is a warning, and the config stays valid.
func TestInspectReportsMissingMockFixtures(t *testing.T) {
	filelog, _ := receiverScenario("filelog")
	kube, _ := receiverScenario("kubernetes")
	const spans = "inputs: [{data: {resourceSpans: [{scopeSpans: [{spans: [{name: op}]}]}]}}]\n"
	file := func(path string) string {
		return "version: 1\nmocks: {files: [{path: " + path + ", content: x}]}\n"
	}
	objects := func(objs string) string {
		return "version: 1\n" + spans + "mocks: {kubernetes: {objects: [" + objs + "]}}\n"
	}
	for _, tc := range []struct {
		name, config, scenario, setup, component, want string
	}{
		{"filelog without scenario", filelog, "", "", "receiver/filelog", "no mocked files are declared"},
		{"filelog matched", filelog, file("/var/log/pods/shop_a_1/app/0.log"), "", "", ""},
		{"filelog excluded", filelog, file("/var/log/pods/shop_a_1/otel-collector/0.log"), "", "receiver/filelog", "reads none of the mocked files"},
		{"filelog elsewhere", filelog, file("/tmp/app.log"), "", "receiver/filelog", "reads none of the mocked files"},
		{"kubernetes without scenario", kube, "", "", "processor/k8sattributes", "no Kubernetes objects are mocked"},
		{"kubernetes without pods", kube, objects("{apiVersion: v1, kind: Namespace, metadata: {name: shop}}"), "", "processor/k8sattributes", "no Pod objects"},
		{"kubernetes invalid", kube, objects("{apiVersion: v1, kind: Service, metadata: {name: a}}"), "", "processor/k8sattributes", "invalid"},
		{"kubernetes with pods", kube, objects("{apiVersion: v1, kind: Pod, metadata: {name: a}}"), "", "", ""},
		{"quick setup with pods", kube, "", "mocks: {kubernetes: {objects: [{apiVersion: v1, kind: Pod, metadata: {name: a}}]}}", "", ""},
		{"quick setup with files", filelog, "", "mocks: {files: [{path: /var/log/pods/shop_a_1/app/0.log, content: x}]}", "", ""},
		{"quick setup without files", filelog, "", "environment: {A: b}", "receiver/filelog", "no mocked files are declared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ins, err := Inspect(context.Background(), InspectRequest{Config: tc.config, ScenarioYAML: tc.scenario, SetupYAML: tc.setup})
			if err != nil {
				t.Fatal(err)
			}
			if !ins.Valid {
				t.Fatalf("inspection invalid: %+v", ins.Diagnostics)
			}
			var gaps []config.Diagnostic
			for _, d := range ins.Diagnostics {
				if d.Code == CodeMockFixtureMissing {
					gaps = append(gaps, d)
				} else {
					t.Errorf("unexpected diagnostic %+v", d)
				}
			}
			if tc.want == "" {
				if len(gaps) != 0 {
					t.Fatalf("gaps %+v, want none", gaps)
				}
				return
			}
			if len(gaps) != 1 || gaps[0].Component != tc.component || gaps[0].Severity != config.SeverityWarning || !strings.Contains(gaps[0].Message, tc.want) {
				t.Fatalf("gaps %+v, want one warning for %s containing %q", gaps, tc.component, tc.want)
			}
		})
	}
}

// policyCases are the environment bindings inspection and preview share with
// runs: the host value is read only by the legacy policy or an explicit
// import, a scenario-declared value wins over the host, and an unbound name
// stands in.
var policyCases = []struct {
	name     string
	policy   config.EnvironmentPolicy
	declared bool
	want     string
}{
	{"legacy reads the host", config.EnvironmentPolicy{LegacyHost: true}, false, "private-host-value"},
	{"explicit without import", config.EnvironmentPolicy{}, false, "linea-stand-in-"},
	{"explicit with import", config.EnvironmentPolicy{Host: []string{policyVariable}}, false, "private-host-value"},
	{"declared value wins", config.EnvironmentPolicy{Host: []string{policyVariable}}, true, "declared-value"},
}

const policyVariable = "OTEL_ENGINE_POLICY_VALUE"

func checkPolicyOutcome(t *testing.T, want string, valid bool, diags []config.Diagnostic, out []byte) {
	t.Helper()
	standIn := slices.ContainsFunc(diags, func(d config.Diagnostic) bool { return d.Code == config.CodeReferenceStandIn })
	if standIn != strings.HasPrefix(want, "linea-stand-in-") {
		t.Fatalf("diagnostics = %+v; stand-in warning expected: %t", diags, !standIn)
	}
	if want != "private-host-value" && strings.Contains(fmt.Sprint(diags)+string(out), "private-host-value") {
		t.Fatalf("the host value leaked: %+v %s", diags, out)
	}
	if !valid {
		t.Fatalf("diagnostics = %+v", diags)
	}
	if !strings.Contains(string(out), want) {
		t.Fatalf("result lacks %q: %s", want, out)
	}
}

func TestInspectionFollowsTheEnvironmentPolicy(t *testing.T) {
	t.Setenv(policyVariable, "private-host-value")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: bound, value: '${env:" + policyVariable + "}', action: upsert}]}}\nexporters: {nop: {}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [nop]}}}\n"
	for _, tt := range policyCases {
		t.Run(tt.name, func(t *testing.T) {
			req := InspectRequest{ConfigYAML: yaml}
			if tt.declared {
				req.ScenarioYAML = "version: 1\nenvironment: {" + policyVariable + ": declared-value}\ninputs: [{data: '{\"resourceSpans\": []}'}]\n"
			}
			ins, err := InspectWithEnvironment(context.Background(), req, tt.policy)
			if err != nil {
				t.Fatal(err)
			}
			effective, err := json.Marshal(ins.Effective)
			if err != nil {
				t.Fatal(err)
			}
			checkPolicyOutcome(t, tt.want, ins.Valid, ins.Diagnostics, effective)
		})
	}
}
