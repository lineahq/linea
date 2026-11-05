package scenario

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const fromRunAssertion = "assertions: [{name: original, target: {exporter: debug, signal: logs}, count: {equals: 0}}]\n"
const fromRunExtra = "version: 1\nassertions: [{name: appended, target: {exporter: debug, signal: logs}, count: {min: 0}}]\n"

func TestFromQuickRun(t *testing.T) {
	for _, tc := range []struct{ name, setup, address, own, extra string }{
		{},
		{"named", "version: 1\nmocks: {files: [{path: /a.log, content: café}]}\nenvironment: {DECLARED: 'verbatim'}\nexecution: {timeout: 3s, timestamps: recorded}\n", "10.0.0.2:1234", fromRunAssertion, fromRunExtra},
		{"", "", "", "", fromRunExtra},
		{"", "", "", fromRunAssertion, ""},
	} {
		t.Run(tc.name+tc.own+tc.extra, func(t *testing.T) {
			fixture := []byte("{\r\n  \"resourceLogs\": []\r\n}\r\n")
			raw, err := FromRun(RunSources{Mode: "quick", Receiver: "otlp", ClientAddress: tc.address, Fixture: fixture, Setup: []byte(tc.setup), Assertions: []byte(tc.own)}, tc.name, []byte(tc.extra))
			if err != nil {
				t.Fatal(err)
			}
			s, err := Parse(raw, "")
			if err != nil {
				t.Fatal(err)
			}
			if s.Version != 1 || s.Name != tc.name || len(s.Inputs) != 1 || s.Inputs[0].Receiver != "otlp" || s.Inputs[0].ClientAddress != tc.address || s.Inputs[0].Signal != "" {
				t.Fatalf("scenario = %+v", s)
			}
			if s.Inputs[0].Data.Style != yaml.DoubleQuotedStyle || s.Inputs[0].Data.Value != string(fixture) {
				t.Fatal("CRLF fixture did not round-trip with quoted fallback")
			}
			if tc.setup != "" && (s.Environment["DECLARED"] != "verbatim" || *s.Mocks.Files[0].Content != "café" || s.Execution.Timeout == 0) {
				t.Fatal("setup sections lost")
			}
			var names []string
			for _, a := range s.Assertions {
				names = append(names, a.Name)
			}
			var want []string
			if tc.own != "" {
				want = append(want, "original")
			}
			if tc.extra != "" {
				want = append(want, "appended")
			}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("assertion order = %v", names)
			}
		})
	}
}

func TestFromScenarioRunInlinesAllDependenciesInOrder(t *testing.T) {
	dir := t.TempDir()
	absolute := filepath.Join(dir, "absolute.json")
	input := []byte("{\r\n\"resourceLogs\": []\r\n}\r\n")
	mock := []byte("café\r\n東京\r\n")
	source := "# preserved comment\nversion: 1\nname: before\ninputs:\n  - file: input.json # input comment\n    receiver: otlp\n    signal: logs\n    format: otlp-json\n  - file: '" + absolute + "'\n  - data: {resourceLogs: []} # inline comment\nmocks:\n  files:\n    - path: /a.log\n      file: mock.log\n    - path: /b.log\n      file: '" + filepath.Join(dir, "absolute.log") + "'\n  kubernetes:\n    objects: [{kind: Pod, metadata: {name: inline}}]\n    files: [one.yaml, '" + filepath.Join(dir, "two.yaml") + "']\nenvironment: {DECLARED: café}\nexecution: {timestamps: recorded, timeout: 3s}\n" + fromRunAssertion
	files := map[string][]byte{filepath.Join(dir, "input.json"): input, absolute: input,
		filepath.Join(dir, "mock.log"): mock, filepath.Join(dir, "absolute.log"): mock,
		filepath.Join(dir, "one.yaml"): []byte("kind: Pod\nmetadata: {name: first}\n---\nkind: List\nitems: [{kind: Pod, metadata: {name: second}}]\n"),
		filepath.Join(dir, "two.yaml"): []byte("kind: Pod\nmetadata: {name: third}\n")}
	raw, err := FromRun(RunSources{Mode: "scenario", Scenario: []byte(source), Dir: dir, Files: files}, "after", []byte(fromRunExtra))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "file:") || len(s.Mocks.Kubernetes.Files) != 0 || s.Name != "after" {
		t.Fatalf("not inlined: %s", raw)
	}
	for _, comment := range []string{"preserved comment", "input comment", "inline comment"} {
		if !bytes.Contains(raw, []byte(comment)) {
			t.Fatalf("lost %s", comment)
		}
	}
	for _, in := range s.Inputs[:2] {
		if in.Data.Value != string(input) || in.Data.Style != yaml.DoubleQuotedStyle {
			t.Fatal("input bytes changed")
		}
	}
	for _, f := range s.Mocks.Files {
		if *f.Content != string(mock) {
			t.Fatal("mock CRLF/Unicode changed")
		}
	}
	if s.Inputs[2].Data.Kind != yaml.MappingNode {
		t.Fatal("inline input was replaced")
	}
	spec, err := s.MockSpec()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, obj := range spec.Objects {
		kinds = append(kinds, obj["kind"].(string))
	}
	if !reflect.DeepEqual(kinds, []string{"Pod", "Pod", "List", "Pod"}) {
		t.Fatal(kinds)
	}
	for i, name := range []string{"inline", "first", "", "third"} {
		if name != "" && spec.Objects[i]["metadata"].(map[string]any)["name"] != name {
			t.Fatal("manifest order changed")
		}
	}
	if len(s.Assertions) != 2 || s.Assertions[0].Name != "original" || s.Assertions[1].Name != "appended" {
		t.Fatal("assertions reordered")
	}
}

func TestFromScenarioRunUnchangedBytesAndLiteralStyle(t *testing.T) {
	source := []byte("# keep spacing\r\nversion: 1\r\ninputs: [{data: '{\"resourceLogs\":[]}'}]\r\n")
	raw, err := FromRun(RunSources{Mode: "scenario", Scenario: source}, "", nil)
	if err != nil || !bytes.Equal(raw, source) {
		t.Fatalf("unchanged scenario = %q, %v", raw, err)
	}
	if _, err := Parse(raw, ""); err != nil {
		t.Fatal(err)
	}
	for _, empty := range []string{"assertions: []\n", "mocks: {kubernetes: {files: []}}\n", "assertions: []\nmocks: {kubernetes: {files: null}}\n"} {
		original := append(bytes.Clone(source), []byte(empty)...)
		result, err := FromRun(RunSources{Mode: "scenario", Scenario: original}, "", nil)
		if err != nil || !bytes.Equal(result, original) {
			t.Fatal("empty sections changed unchanged bytes", err)
		}
	}
	raw, err = FromRun(RunSources{Mode: "quick", Fixture: []byte("{\n\"resourceLogs\": []\n}\n")}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Parse(raw, "")
	if err != nil || s.Inputs[0].Data.Style != yaml.LiteralStyle {
		t.Fatal("literal style was not preferred", err)
	}
}

func TestFromRunRefusesNonUTF8AndMissingCaptures(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ doc, location string }{
		{"version: 1\ninputs: [{file: bad}]\n", "inputs[0]"},
		{"version: 1\nmocks: {files: [{path: /a.log, file: bad}]}\n", "mocks.files[0]"},
	} {
		_, err := FromRun(RunSources{Mode: "scenario", Scenario: []byte(tc.doc), Dir: dir, Files: map[string][]byte{filepath.Join(dir, "bad"): {0xff}}}, "", nil)
		if err == nil || err.Error() != tc.location+": the captured file is not UTF-8 text and cannot be inlined" {
			t.Fatal(err)
		}
		_, err = FromRun(RunSources{Mode: "scenario", Scenario: []byte(tc.doc), Dir: dir}, "", nil)
		if !errors.Is(err, ErrMissingCapture) {
			t.Fatal(err)
		}
	}
}

func TestFromRunHandlesEmptyFileKeysAndInheritedReferences(t *testing.T) {
	dir := t.TempDir()
	for _, source := range []string{
		"version: 1\ninputs: [{data: '{\"resourceLogs\":[]}', file: ''}]\nmocks: {files: [{path: /a.log, content: inline, file: ''}]}\n",
		"version: 1\ninputs:\n  - &input {file: input.json, receiver: otlp}\n  - {<<: *input}\n",
		"version: 1\ninputs: [{file: &path input.json}, {file: *path}]\n",
	} {
		raw, err := FromRun(RunSources{Mode: "scenario", Scenario: []byte(source), Dir: dir, Files: map[string][]byte{filepath.Join(dir, "input.json"): []byte(`{"resourceLogs":[]}`)}}, "renamed", nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Parse(raw, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Fixtures(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFromRunCapturedScalarLineBreaksRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, text := range []string{"café\r\n東京\r\n", "café\u0085東京\u2028next\u2029", "\x00control\tvalue\n", "no final newline", "\n\n", ""} {
		var content []byte
		if text != "" {
			content = []byte(text)
		}
		raw, err := FromRun(RunSources{Mode: "scenario", Scenario: []byte("version: 1\nmocks: {files: [{path: /a.log, file: text}]}\n"), Dir: dir, Files: map[string][]byte{filepath.Join(dir, "text"): content}}, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		s, err := Parse(raw, "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Mocks.Files[0].Content == nil || *s.Mocks.Files[0].Content != text || bytes.Contains(raw, []byte("!!binary")) {
			t.Fatalf("scalar changed: %q", text)
		}
	}
}

func TestFromRunVerificationRefusesRemainingHostReferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "matching.json")
	if err := os.WriteFile(path, []byte(`{"resourceLogs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := Parse([]byte("version: 1\ninputs: [{data: '{\"resourceLogs\":[]}'}]\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	// Even matching data on the host cannot satisfy verification: the output
	// must contain only inline sources, and its reader is closed to host access.
	raw := []byte("version: 1\ninputs: [{file: '" + path + "'}]\n")
	if err := verifyFromRun(original, raw, "", nil); err == nil {
		t.Fatal("verification accepted a remaining host reference")
	}
}
