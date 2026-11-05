package scenario

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInlineReadsEveryReferenceFromTheHost(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("input.json", `{"resourceLogs": []}`)
	write("mock.log", "café\n")
	write("pods.yaml", "kind: Pod\nmetadata: {name: first}\n")
	source := "version: 1\ninputs:\n  - file: input.json\n    receiver: otlp\n  - file: input.json\nmocks:\n  files: [{path: /a.log, file: mock.log}]\n  kubernetes: {files: [pods.yaml]}\n"
	raw, err := Inline([]byte(source), dir)
	if err != nil {
		t.Fatal(err)
	}
	// Parsed without a directory, as a server that never sees dir does.
	s, err := Parse(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := s.Fixtures()
	if err != nil || len(fixtures) != 2 || fixtures[0].Receiver != "otlp" {
		t.Fatalf("fixtures = %+v, %v", fixtures, err)
	}
	spec, err := s.MockSpec()
	if err != nil || len(spec.Files) != 1 || string(spec.Files[0].Content) != "café\n" || len(spec.Objects) != 1 {
		t.Fatalf("mocks = %+v, %v", spec, err)
	}
}

func TestInlineLeavesSelfContainedScenarioUnchanged(t *testing.T) {
	source := []byte("# kept\nversion: 1\ninputs: [{data: {resourceLogs: []}}]\n")
	raw, err := Inline(source, "")
	if err != nil || !bytes.Equal(raw, source) {
		t.Fatalf("Inline = %q, %v", raw, err)
	}
}

func TestInlineNamesTheMissingReference(t *testing.T) {
	_, err := Inline([]byte("version: 1\ninputs: [{data: {resourceLogs: []}}]\nmocks: {files: [{path: /a.log, file: missing.log}]}\n"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "mocks.files[0]") {
		t.Fatalf("err = %v", err)
	}
}
