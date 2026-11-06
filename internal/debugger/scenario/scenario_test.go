package scenario

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadTracesScenario(t *testing.T) {
	s, err := Load(filepath.Join("..", "..", "..", "testdata", "traces", "scenario.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name == "" || s.Timeout() != 30*time.Second || len(s.Environment) == 0 {
		t.Fatalf("scenario = %+v", s)
	}
	fixtures, err := s.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 1 || fixtures[0].Receiver != "otlp" || fixtures[0].Payload.Counts().Items != 3 {
		t.Fatalf("fixtures = %+v", fixtures)
	}
}

func TestLoadRejectsInvalidScenarios(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"wrong version", "version: 2\ninputs: [{file: a.json}]\n", "unsupported version"},
		{"no inputs", "version: 1\n", "at least one input or mocked file"},
		{"missing file", "version: 1\ninputs: [{receiver: otlp}]\n", "exactly one of file or data"},
		{"file and data", "version: 1\ninputs: [{file: a.json, data: {resourceLogs: []}}]\n", "exactly one of file or data"},
		{"unknown format", "version: 1\ninputs: [{file: a.json, format: otlp-proto}]\n", "unsupported format"},
		{"unknown signal", "version: 1\ninputs: [{file: a.json, signal: profiles}]\n", "unknown signal"},
		{"unsupported section", "version: 1\ninputs: [{file: a.json}]\nreplay: {}\n", "replay"},
		{"file without path", "version: 1\nmocks: {files: [{content: x}]}\n", "path is required"},
		{"file with content and file", "version: 1\nmocks: {files: [{path: /a.log, content: x, file: a.log}]}\n", "at most one of content or file"},
		{"exporter mode", "version: 1\ninputs: [{file: a.json}]\nmocks: {exporters: {mode: forward}}\n", "only capture"},
		{"client address", "version: 1\ninputs: [{file: a.json, client_address: checkout.shop}]\n", "not an IP address"},
		{"header name", "version: 1\ninputs: [{file: a.json, headers: {'x tenant': a}}]\n", "not a valid header name"},
		{"binary header", "version: 1\ninputs: [{file: a.json, headers: {trace-bin: a}}]\n", "not a valid header name"},
		{"header twice", "version: 1\ninputs: [{file: a.json, headers: {Space: a, space: b}}]\n", "are the same header"},
		{"header without value", "version: 1\ninputs: [{file: a.json, headers: {space: []}}]\n", "has no value"},
		{"header map value", "version: 1\ninputs: [{file: a.json, headers: {space: {a: b}}}]\n", "string or a list of strings"},
		{"default header", "version: 1\ninputs: [{file: a.json}]\ndefaults: {headers: {':path': a}}\n", "defaults.headers"},
		{"bad timeout", "version: 1\ninputs: [{file: a.json}]\nexecution: {timeout: soon}\n", "invalid duration"},
		{"negative after", "version: 1\ninputs: [{file: a.json, after: -1s}]\n", "must be positive"},
		{"unknown timestamps", "version: 1\ninputs: [{file: a.json}]\nexecution: {timestamps: now}\n", "execution.timestamps"},
		{"bad settle", "version: 1\ninputs: [{file: a.json}]\nexecution: {settle: later}\n", "invalid duration"},
		{"empty", "", "empty scenario"},
		{"assertion without target", assertions("{count: {equals: 1}}"), "exactly one of exporter, processor or endpoint"},
		{"assertion with both targets", assertions("{target: {exporter: a, processor: b, signal: logs}, count: {equals: 1}}"), "exactly one of exporter, processor or endpoint"},
		{"exporter and endpoint", assertions("{target: {exporter: a, endpoint: {component: extension/x, path: p}, signal: logs}, count: {equals: 1}}"), "exactly one of exporter, processor or endpoint"},
		{"endpoint without kind", assertions("{target: {endpoint: {component: livecapture, path: delivery}, signal: logs}, count: {equals: 1}}"), "want <kind>/<name>"},
		{"endpoint without path", assertions("{target: {endpoint: {component: extension/livecapture}, signal: logs}, count: {equals: 1}}"), "target.endpoint.path is required"},
		{"pipeline at endpoint", assertions("{target: {endpoint: {component: extension/x, path: p}, pipeline: logs, signal: logs}, count: {min: 1}}"), "processor targets only"},
		{"dropped at endpoint", assertions("{target: {endpoint: {component: extension/x, path: p}, signal: logs}, dropped: {equals: 1}}"), "processor targets only"},
		{"assertion without signal", assertions("{target: {exporter: a}, count: {equals: 1}}"), "signal is required"},
		{"assertion without condition", assertions("{target: {exporter: a, signal: logs}}"), "no condition"},
		{"dropped at exporter", assertions("{target: {exporter: a, signal: logs}, dropped: {equals: 1}}"), "processor targets only"},
		{"pipeline at exporter", assertions("{target: {exporter: a, pipeline: logs, signal: logs}, count: {min: 1}}"), "processor targets only"},
		{"equals with range", assertions("{target: {exporter: a, signal: logs}, count: {equals: 1, max: 2}}"), "cannot be combined"},
		{"inverted range", assertions("{target: {exporter: a, signal: logs}, count: {min: 3, max: 2}}"), "greater than max"},
		{"empty bound", assertions("{target: {exporter: a, signal: logs}, count: {}}"), "declare equals, min or max"},
		{"present and absent", assertions("{target: {exporter: a, signal: logs}, attributes: {present: [k], absent: [k]}}"), "both present and absent"},
		{"non-scalar equals", assertions("{target: {exporter: a, signal: logs}, attributes: {equals: {k: [1]}}}"), "must be a string, bool, int or double"},
		{"unknown type", assertions("{target: {exporter: a, signal: logs}, attributes: {types: {k: number}}}"), "unknown type"},
		{"unknown condition", assertions("{target: {exporter: a, signal: logs}, routed: true}"), "routed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scenario.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

// assertions returns a minimal scenario declaring one assertion.
func assertions(assertion string) string {
	return "version: 1\ninputs: [{file: a.json}]\nassertions: [" + assertion + "]\n"
}

func TestLoadParsesAssertions(t *testing.T) {
	s, err := Load(filepath.Join("..", "..", "..", "testdata", "multisignal", "tests", "scrubbing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Assertions) != 4 {
		t.Fatalf("assertions = %+v", s.Assertions)
	}
	creds := s.Assertions[1]
	if creds.Target.String() != "exporter otlp/backend (logs)" || creds.Attributes == nil ||
		creds.Attributes.Absent[0] != "user.password" || creds.ResourceAttributes.Equals["deployment.environment"] != "test" {
		t.Fatalf("credentials assertion = %+v", creds)
	}
	drop := s.Assertions[2]
	if drop.Target.String() != "processor filter/health in logs (logs)" || drop.Dropped.String() != "== 1" {
		t.Fatalf("drop assertion = %+v", drop)
	}
	if got := s.Assertions[3].Count.String(); got != ">= 1 and <= 1" {
		t.Fatalf("count bound = %q", got)
	}
}

func TestLoadParsesEndpointTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	yaml := assertions("{target: {endpoint: {component: extension/livecapture, path: delivery}, signal: logs}, count: {min: 1}}")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Assertions[0].Target.String(); got != "endpoint extension/livecapture delivery (logs)" {
		t.Fatalf("target = %q", got)
	}
}

func TestBoundHoldsAndExceeded(t *testing.T) {
	one, two := 1, 2
	tests := []struct {
		bound          Bound
		n              int
		holds, exceeds bool
	}{
		{Bound{Equals: &one}, 1, true, false},
		{Bound{Equals: &one}, 0, false, false},
		{Bound{Equals: &one}, 2, false, true},
		{Bound{Min: &one}, 5, true, false},
		{Bound{Min: &one, Max: &two}, 0, false, false},
		{Bound{Min: &one, Max: &two}, 3, false, true},
		{Bound{Max: &two}, 0, true, false},
	}
	for _, tt := range tests {
		if got := tt.bound.Holds(tt.n); got != tt.holds {
			t.Errorf("%s holds %d = %v", tt.bound, tt.n, got)
		}
		if got := tt.bound.Exceeded(tt.n); got != tt.exceeds {
			t.Errorf("%s exceeded by %d = %v", tt.bound, tt.n, got)
		}
	}
}

func TestFixturesChecksDeclaredSignal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logs.json"), []byte(`{"resourceLogs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "scenario.yaml")
	if err := os.WriteFile(path, []byte("version: 1\ninputs: [{file: logs.json, signal: traces}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fixtures(); err == nil || !strings.Contains(err.Error(), "declared signal traces") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseReadsInlineInputs(t *testing.T) {
	const logs = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"hi"}}]}]}]}`
	tests := []struct {
		name, yaml string
		wantErr    string
	}{
		{"json string", "version: 1\ninputs:\n  - data: '" + logs + "'\n", ""},
		{"yaml mapping", "version: 1\ninputs:\n  - signal: logs\n    data: " + logs + "\n", ""},
		{"relative file without directory", "version: 1\ninputs: [{file: a.json}]\n", "has no directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse([]byte(tt.yaml), "")
			if err != nil {
				t.Fatal(err)
			}
			fixtures, err := s.Fixtures()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fixtures) != 1 || fixtures[0].Payload.Signal.String() != "logs" || fixtures[0].Payload.Counts().Items != 1 {
				t.Fatalf("fixtures = %+v", fixtures)
			}
		})
	}
}

// TestFixturesMergeHeaders: an input's headers replace the defaults of the
// same name, matched case-insensitively, and names come out lowercased.
func TestFixturesMergeHeaders(t *testing.T) {
	const logs = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"hi"}}]}]}]}`
	yaml := "version: 1\ndefaults:\n  headers: {X-Tenant: acme, Space: prod}\ninputs:\n" +
		"  - data: '" + logs + "'\n" +
		"  - data: '" + logs + "'\n    headers: {space: [dev, qa], User-Agent: test}\n"
	s, err := Parse([]byte(yaml), "")
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := s.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string][]string{
		{"x-tenant": {"acme"}, "space": {"prod"}},
		{"x-tenant": {"acme"}, "space": {"dev", "qa"}, "user-agent": {"test"}},
	}
	for i, w := range want {
		if !reflect.DeepEqual(fixtures[i].Headers, w) {
			t.Errorf("input %d headers = %v, want %v", i, fixtures[i].Headers, w)
		}
	}
	plain, err := Parse([]byte("version: 1\ninputs:\n  - data: '"+logs+"'\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := plain.Fixtures(); f[0].Headers != nil {
		t.Errorf("headers without declarations = %v, want nil", f[0].Headers)
	}
}

// TestParseReadsTheTimeline: each input's after reaches its fixture and
// execution.settle is reported; both are unset by default.
func TestParseReadsTheTimeline(t *testing.T) {
	const data = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"hi"}}]}]}]}`
	s, err := Parse([]byte("version: 1\ninputs:\n  - data: '"+data+"'\n  - after: 1500ms\n    data: '"+data+"'\nexecution: {settle: 2s}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := s.Fixtures()
	if err != nil {
		t.Fatal(err)
	}
	if fixtures[0].After != 0 || fixtures[1].After != 1500*time.Millisecond {
		t.Fatalf("after = %v, %v", fixtures[0].After, fixtures[1].After)
	}
	if s.Rebase() {
		t.Fatal("timestamps are rebased by default")
	}
	if s.Settle() != 2*time.Second {
		t.Fatalf("settle = %v", s.Settle())
	}
	setup, err := ParseSetup([]byte("execution: {settle: 3s, timestamps: rebase}\n"))
	if err != nil || setup.Settle() != 3*time.Second || !setup.Rebase() {
		t.Fatalf("setup settle = %v, %v", setup, err)
	}
}

// TestMockSpecReadsKubernetesObjects combines inline objects with a manifest
// file of several documents, one of them a List, and keeps the inputs'
// client addresses.
func TestMockSpecReadsKubernetesObjects(t *testing.T) {
	dir := t.TempDir()
	manifests := "apiVersion: v1\nkind: Namespace\nmetadata: {name: shop}\n---\n---\n" +
		"apiVersion: v1\nkind: List\nitems:\n  - {apiVersion: v1, kind: Pod, metadata: {name: a, namespace: shop}}\n"
	if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), []byte(manifests), 0o600); err != nil {
		t.Fatal(err)
	}
	const logs = `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"hi"}}]}]}]}`
	s, err := Parse([]byte("version: 1\ninputs:\n  - data: '"+logs+"'\n    client_address: 10.42.0.17\nmocks:\n  kubernetes:\n"+
		"    objects: [{apiVersion: v1, kind: Node, metadata: {name: worker-1}}]\n    files: [cluster.yaml]\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := s.MockSpec()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, o := range spec.Objects {
		kinds = append(kinds, o["kind"].(string))
	}
	if strings.Join(kinds, ",") != "Node,Namespace,List" {
		t.Fatalf("object kinds %v, want Node,Namespace,List", kinds)
	}
	fixtures, err := s.Fixtures()
	if err != nil || len(fixtures) != 1 || fixtures[0].ClientAddress != "10.42.0.17" {
		t.Fatalf("fixtures = %+v, %v; want the client address", fixtures, err)
	}
}

// TestParseSetup accepts the mocks, environment and execution of a quick run
// and rejects what only a scenario has.
func TestParseAssertions(t *testing.T) {
	const valid = "assertions: [{target: {exporter: otlp, signal: logs}, count: {equals: 1}}]\n"
	for _, tt := range []struct {
		name, yaml, wantErr string
	}{
		{"without version", valid, ""},
		{"version 1", "version: 1\n" + valid, ""},
		{"version 0", "version: 0\n" + valid, ""},
		{"inputs", valid + "inputs: []\n", "field inputs not found"},
		{"mocks", valid + "mocks: {}\n", "field mocks not found"},
		{"environment", valid + "environment: {}\n", "field environment not found"},
		{"execution", valid + "execution: {}\n", "field execution not found"},
		{"unknown key", valid + "unknown: true\n", "field unknown not found"},
		{"unknown assertion key", strings.Replace(valid, "count:", "unknown:", 1), "field unknown not found"},
		{"wrong version", "version: 2\n" + valid, "unsupported version 2 (want 1)"},
		{"empty document", "", "assertions: at least one assertion is required"},
		{"version only", "version: 1\n", "assertions: at least one assertion is required"},
		{"empty assertions", "assertions: []\n", "assertions: at least one assertion is required"},
		{"invalid assertion", "assertions: [{target: {exporter: otlp, signal: logs}, count: {equals: -1}}]\n", "assertions[0]: count: negative bound"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseAssertions([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].Target.Exporter != "otlp" || got[0].Count == nil || !got[0].Count.Holds(1) {
				t.Fatalf("assertions = %+v, %v", got, err)
			}
		})
	}
}

func TestParseSetup(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{"empty", "", ""},
		{"mocks without version", "environment: {TOKEN: x}\nmocks:\n  files: [{path: /var/log/a.log, content: hi}]\n", ""},
		{"version 1", "version: 1\nmocks: {kubernetes: {objects: [{apiVersion: v1, kind: Pod, metadata: {name: a}}]}}\n", ""},
		{"inputs", "inputs: [{receiver: otlp, file: a.json}]\n", "use a scenario"},
		{"assertions", "assertions: [{target: {exporter: debug, signal: logs}, count: {equals: 1}}]\n", "use a scenario"},
		{"unknown field", "mock: {}\n", "not found"},
		{"file without path", "mocks: {files: [{content: hi}]}\n", "path is required"},
		{"wrong version", "version: 2\n", "unsupported version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := ParseSetup([]byte(tt.yaml))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.MockSpec(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
