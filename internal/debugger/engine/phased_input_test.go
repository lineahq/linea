package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func encoded(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	if err := Encode(&b, v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestInspectionPhasesMatchInspectWithPolicy(t *testing.T) {
	filelog, _ := receiverScenario("filelog")
	kube, _ := receiverScenario("kubernetes")
	dir := t.TempDir()
	token := filepath.ToSlash(filepath.Join(dir, "token.txt"))
	if err := os.WriteFile(token, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	envYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters:\n  otlp:\n    endpoint: '${env:PHASED_ENDPOINT}'\n    headers: {a: '${file:" + token + "}', b: '${env:PHASED_UNBOUND}'}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	wall := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['set(log.time, Now())']}]}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n"
	kafka := "receivers: {kafka: {}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [kafka], exporters: [nop]}}}\n"
	podFile := "version: 1\nmocks: {files: [{path: /var/log/pods/shop_a_1/app/0.log, existing: x, content: y}]}\n"
	pods := "version: 1\ninputs: [{data: {resourceSpans: [{scopeSpans: [{spans: [{name: op}]}]}]}}]\nmocks: {kubernetes: {objects: [{apiVersion: v1, kind: Pod, metadata: {name: a}}]}}\n"
	for _, tc := range []struct {
		name string
		req  InspectRequest
		fail bool
	}{
		{"valid", InspectRequest{Config: multisignalConfig}, false},
		{"invalid", InspectRequest{ConfigYAML: "receivers: [oops\n"}, false},
		{"unresolvable type", InspectRequest{ConfigYAML: "receivers: {otlp: {protocols: {nope: {}}}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], exporters: [nop]}}}\n"}, false},
		{"scenario env and files", InspectRequest{ConfigYAML: envYAML, ScenarioYAML: "version: 1\nenvironment: {PHASED_ENDPOINT: 'localhost:4317'}\ninputs: [{data: '{}'}]\n"}, false},
		{"setup mocks", InspectRequest{Config: filelog, SetupYAML: "mocks: {files: [{path: /var/log/pods/shop_a_1/app/0.log, content: x}]}"}, false},
		{"mock gaps", InspectRequest{Config: filelog}, false},
		{"mock gaps with objects", InspectRequest{Config: kube, ScenarioYAML: pods}, false},
		{"mock file times", InspectRequest{Config: filelog, ScenarioYAML: podFile}, false},
		{"wall clock", InspectRequest{ConfigYAML: wall}, false},
		{"wall clock rebased", InspectRequest{ConfigYAML: wall, SetupYAML: "execution: {timestamps: rebase}"}, false},
		{"stand-in component", InspectRequest{ConfigYAML: kafka}, false},
		{"mid-edit scenario", InspectRequest{ConfigYAML: kafka, ScenarioYAML: "version: [\n"}, false},
		{"bad request", InspectRequest{}, true},
		{"missing file", InspectRequest{Config: filepath.Join(dir, "absent.yaml")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("PHASED_UNBOUND", "")
			want, wantErr := InspectWithPolicy(ctx, tc.req, Policy{Paths: unrestricted})
			in, final, err := CaptureInspection(ctx, tc.req, Policy{Paths: unrestricted})
			if (err != nil) != (wantErr != nil) || tc.fail != (wantErr != nil) {
				t.Fatalf("errors: got %v, want %v", err, wantErr)
			}
			if wantErr != nil {
				return
			}
			got := final
			if in != nil {
				raw, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				var back InspectionInput
				if err := json.Unmarshal(raw, &back); err != nil {
					t.Fatal(err)
				}
				if got, err = CompleteInspection(ctx, &back); err != nil {
					t.Fatal(err)
				}
			}
			if encoded(t, got) != encoded(t, want) {
				t.Fatalf("phased result differs\n got %s\nwant %s", encoded(t, got), encoded(t, want))
			}
		})
	}
}

func TestPhasedInputsRejectUnknownFields(t *testing.T) {
	var in InspectionInput
	if err := json.Unmarshal([]byte(`{"source":{"sha256":"x"},"snapshot":{},"extra":1}`), &in); err == nil {
		t.Fatal("unknown field accepted")
	}
	var o OTTLInput
	if err := json.Unmarshal([]byte(`{"snapshot":{},"processor":"transform","input":{},"extra":1}`), &o); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestOTTLPhasesMatchEvaluateOTTLWithPolicy(t *testing.T) {
	logs, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "paging", "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	set := func(s string) string {
		return "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {transform: {log_statements: [{context: log, statements: ['" + s + "']}]}}\nexporters: {nop: {}}\nservice: {pipelines: {logs: {receivers: [otlp], processors: [transform], exporters: [nop]}}}\n"
	}
	env := set(`set(log.attributes["e"], "${env:PHASED_E}")`)
	for _, tc := range []struct {
		name string
		req  OTTLRequest
		fail bool
	}{
		{"valid", OTTLRequest{ConfigYAML: set(`set(log.attributes["a"], "b")`), Processor: "transform", Input: logs}, false},
		{"statement does not parse", OTTLRequest{ConfigYAML: set(`set(log.attributes[`), Processor: "transform", Input: logs}, false},
		{"invalid yaml", OTTLRequest{ConfigYAML: "receivers: [oops\n", Processor: "transform", Input: logs}, false},
		{"scenario env", OTTLRequest{ConfigYAML: env, Processor: "transform", Input: logs, ScenarioYAML: "version: 1\nenvironment: {PHASED_E: declared}\ninputs: [{data: '{}'}]\n"}, false},
		{"not configured", OTTLRequest{ConfigYAML: set(`set(log.attributes["a"], "b")`), Processor: "transform/other", Input: logs}, true},
		{"not a transform", OTTLRequest{ConfigYAML: set(`set(log.attributes["a"], "b")`), Processor: "batch", Input: logs}, true},
		{"no input", OTTLRequest{ConfigYAML: set(`set(log.attributes["a"], "b")`), Processor: "transform"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			want, wantErr := EvaluateOTTLWithPolicy(ctx, tc.req, Policy{Paths: unrestricted})
			in, final, err := CaptureOTTL(ctx, tc.req, Policy{Paths: unrestricted})
			var got *OTTLEvaluation
			if err == nil && in != nil {
				raw, merr := json.Marshal(in)
				if merr != nil {
					t.Fatal(merr)
				}
				var back OTTLInput
				if merr = json.Unmarshal(raw, &back); merr != nil {
					t.Fatal(merr)
				}
				got, err = CompleteOTTL(ctx, &back)
			} else {
				got = final
			}
			if (err != nil) != (wantErr != nil) || tc.fail != (wantErr != nil) {
				t.Fatalf("errors: got %v, want %v", err, wantErr)
			}
			if wantErr != nil {
				if err.Error() != wantErr.Error() {
					t.Fatalf("error %q, want %q", err, wantErr)
				}
				return
			}
			if encoded(t, got) != encoded(t, want) {
				t.Fatalf("phased result differs\n got %s\nwant %s", encoded(t, got), encoded(t, want))
			}
		})
	}
}
