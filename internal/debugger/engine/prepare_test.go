package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const captureConfig = `receivers: {otlp: {protocols: {grpc: {}}}}
exporters: {otlp: {endpoint: example.invalid:4317}}
service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}
`

func TestPreparedScenarioKeepsSourcesMocksAndFreshDecodedInputs(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile(tracesFixture)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"input.json":    fixture,
		"app.log":       []byte("original\x00\xff\n"),
		"pods.yaml":     []byte("apiVersion: v1\nkind: Pod\nmetadata: {name: original}\n"),
		"scenario.yaml": []byte("# original source\r\nversion: 1\r\nname: original\r\ninputs: [{file: input.json, signal: traces, after: 1ms}]\r\nmocks:\r\n  files: [{path: /var/log/app.log, file: app.log, existing: before}]\r\n  kubernetes: {files: [pods.yaml]}\r\nexecution: {timeout: 2s, settle: 1ms, timestamps: rebase}\r\n"),
	}
	for name, raw := range files {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := Request{ConfigYAML: captureConfig, Scenario: filepath.Join(dir, "scenario.yaml")}
	prepared, err := Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := loadInputs(prepared, prepared.prepared.read)
	if err != nil {
		t.Fatal(err)
	}
	if data.name != "original" || !data.rebase || data.timeout.String() != "2s" || data.opts.Inputs[0].After.String() != "1ms" {
		t.Fatalf("scenario settings changed: %+v", data)
	}
	if !bytes.Equal(data.opts.Mocks.Files[0].Content, files["app.log"]) || string(data.opts.Mocks.Files[0].Existing) != "before" {
		t.Fatalf("mock bytes changed: %+v", data.opts.Mocks.Files)
	}
	if data.opts.Mocks.Objects[0]["metadata"].(map[string]any)["name"] != "original" {
		t.Fatal("manifest changed")
	}
	data.opts.Mocks.Files[0].Content[0] = 'X'
	data.opts.Mocks.Objects[0]["metadata"].(map[string]any)["name"] = "changed"
	data.opts.Inputs[0].Payload.Traces.ResourceSpans().At(0).Resource().Attributes().PutStr("mutated", "yes")
	again, err := loadInputs(prepared, prepared.prepared.read)
	if err != nil {
		t.Fatal(err)
	}
	if again.opts.Mocks.Files[0].Content[0] != 'o' || again.opts.Mocks.Objects[0]["metadata"].(map[string]any)["name"] != "original" {
		t.Fatal("decoded mock data was shared between executions")
	}
	if _, ok := again.opts.Inputs[0].Payload.Traces.ResourceSpans().At(0).Resource().Attributes().Get("mutated"); ok {
		t.Fatal("pdata was shared")
	}
	p, err := provenance(prepared, nil)
	if err != nil || *p.Scenario != sourceFromBytes(req.Scenario, files["scenario.yaml"]) {
		t.Fatalf("source provenance changed: %+v, %v", p, err)
	}
	if _, err := prepared.prepared.read(filepath.Join(dir, "new.json")); err == nil {
		t.Fatal("uncaptured host read accepted")
	}
	encoded, err := json.Marshal(prepared)
	if err != nil || bytes.Contains(encoded, []byte("original")) {
		t.Fatalf("private preparation escaped into JSON: %s, %v", encoded, err)
	}
	bundle, _, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeBundle(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	restoredData, err := loadInputs(restored, restored.prepared.read)
	if err != nil || !bytes.Equal(restoredData.opts.Mocks.Files[0].Content, files["app.log"]) || restoredData.opts.Mocks.Objects[0]["metadata"].(map[string]any)["name"] != "original" {
		t.Fatalf("bundle changed mock source bytes/objects: %v", err)
	}
}

func TestPreparedRequestRejectsSourceChangesButAllowsRuntimeIdentity(t *testing.T) {
	req, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.FixtureJSON = `{"resourceLogs":[]}`
	if _, err := Prepare(context.Background(), changed); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("changed preparation = %v", err)
	}
	req.RunID = "scheduler-id"
	if _, err := Prepare(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestPreparedScenarioKeepsRelativeBaseAfterWorkingDirectoryChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "input.json"), []byte(`{"resourceSpans":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	text := "version: 1\ninputs: [{file: input.json}]\n"
	if err := os.WriteFile(filepath.Join(dir, "scenario.yaml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, inline := range []bool{false, true} {
		t.Chdir(dir)
		req := Request{ConfigYAML: captureConfig, Scenario: "scenario.yaml"}
		if inline {
			req.Scenario, req.ScenarioYAML, req.ScenarioDir = "", text, "."
		}
		prepared, err := Prepare(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		t.Chdir(t.TempDir())
		if _, err := loadInputs(prepared, prepared.prepared.read); err != nil {
			t.Fatalf("relative scenario base changed: %v", err)
		}
	}
}

func TestPreparedFileMockExecutesAfterItsSourceIsDeleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("accepted mock record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "receivers: {filelog: {include: [/var/log/app.log], poll_interval: 10ms}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {logs: {receivers: [filelog], exporters: [otlp]}}}\n"
	req, err := Prepare(ctx, Request{ConfigYAML: config, ScenarioYAML: "version: 1\nmocks: {files: [{path: /var/log/app.log, file: app.log}]}\n", ScenarioDir: dir, Compare: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rep, err := Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Comparison.Equal || len(rep.Result.Outputs) == 0 {
		t.Fatalf("captured mock run disagrees: %+v", rep.Comparison)
	}
	for _, output := range rep.Result.Outputs {
		if !bytes.Contains(output.Data, []byte("accepted mock record")) {
			t.Fatalf("captured mock missing: %s", output.Data)
		}
	}
}

func TestPreparationLimitsCancellationAndDependencyValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	if _, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: strings.Repeat("x", int(maxInputBytes)+1)}); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("inline budget = %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "large.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxInputBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, Fixture: path}); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("file budget = %v", err)
	}
	if _, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, Fixture: dir}); err == nil {
		t.Fatal("non-regular input accepted")
	}
	setup := "mocks:\n  files:\n    - path: /var/log/app.log\n      file: '" + path + "'\n"
	if _, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`, SetupYAML: setup}); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("quick setup lost capture limit cause: %v", err)
	}
	for _, text := range []string{
		"version: 1\ninputs: [{data: '{\"resourceSpans\":[]}', signal: logs}]\n",
		"version: 1\ninputs: [{file: missing.json}]\n",
		"version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\nmocks: {kubernetes: {files: [missing.yaml]}}\n",
	} {
		if _, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, ScenarioYAML: text, ScenarioDir: dir}); err == nil {
			t.Fatalf("invalid dependency admitted: %s", text)
		}
	}
	c := &inputCapture{ctx: context.Background(), files: map[string][]byte{}, paths: unrestricted}
	for i := 0; i < maxInputFiles; i++ {
		c.files[filepath.Join(dir, strings.Repeat("x", i+1))] = nil
	}
	if _, err := c.read(path); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("file count = %v", err)
	}
}

func TestSharedInputsAreNotReadAgain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("shared mock record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "receivers: {filelog: {include: [/var/log/app.log], poll_interval: 10ms}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {logs: {receivers: [filelog], exporters: [otlp]}}}\n"
	scenario := "version: 1\nmocks: {files: [{path: /var/log/app.log, file: app.log}]}\n"
	before, err := Prepare(ctx, Request{ConfigYAML: config, ScenarioYAML: scenario, ScenarioDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ShareInputs(Request{ConfigYAML: config, ScenarioYAML: scenario, ScenarioDir: dir}, Request{ConfigYAML: config, ScenarioYAML: scenario, ScenarioDir: dir}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("sharing unprepared inputs: %v", err)
	}
	if _, err := ShareInputs(Request{ConfigYAML: config, ScenarioYAML: scenario + "# other\n", ScenarioDir: dir}, before); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("sharing other inputs: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(config, "10ms", "20ms", 1)
	after, err := ShareInputs(Request{ConfigYAML: other, ScenarioYAML: scenario, ScenarioDir: dir}, before)
	if err != nil {
		t.Fatal(err)
	}
	if after, err = Prepare(ctx, after); err != nil {
		t.Fatalf("preparing on shared inputs read the deleted file: %v", err)
	}
	b, _, err := EncodeBundle(before)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := EncodeBundle(after)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := ReadBundleParts(b)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := ReadBundleParts(a)
	if err != nil {
		t.Fatal(err)
	}
	if bp.InputsDigest() != ap.InputsDigest() || !strings.Contains(string(ap.Config), "20ms") || strings.Contains(string(bp.Config), "20ms") {
		t.Fatalf("digests %s and %s; after config %s", bp.InputsDigest(), ap.InputsDigest(), ap.Config)
	}
	rep, err := Execute(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range rep.Result.Outputs {
		if !bytes.Contains(output.Data, []byte("shared mock record")) {
			t.Fatalf("shared mock missing: %s", output.Data)
		}
	}
}

// A root source read from a path and the same text sent inline are the same
// input: only dependency files enter the digest by name.
func TestInputsDigestIsTheSameForAPathAndItsText(t *testing.T) {
	dir := t.TempDir()
	fixture, err := os.ReadFile(tracesFixture)
	if err != nil {
		t.Fatal(err)
	}
	scenario := "version: 1\ninputs: [{file: input.json, signal: traces}]\n"
	for name, data := range map[string][]byte{"input.json": fixture, "scenario.yaml": []byte(scenario)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := func(r Request) string {
		t.Helper()
		prepared, err := Prepare(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		raw, _, err := EncodeBundle(prepared)
		if err != nil {
			t.Fatal(err)
		}
		parts, err := ReadBundleParts(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parts.InputsDigest()
	}
	for _, pair := range [][2]Request{
		{{ConfigYAML: captureConfig, Fixture: filepath.Join(dir, "input.json")}, {ConfigYAML: captureConfig, FixtureJSON: string(fixture), FixtureLabel: "input.json"}},
		{{ConfigYAML: captureConfig, Scenario: filepath.Join(dir, "scenario.yaml")}, {ConfigYAML: captureConfig, ScenarioYAML: scenario, ScenarioDir: dir}},
	} {
		if path, text := digest(pair[0]), digest(pair[1]); path != text {
			t.Fatalf("path digest %s, inline %s", path, text)
		}
	}
	if quick, other := digest(Request{ConfigYAML: captureConfig, FixtureJSON: string(fixture)}), digest(Request{ConfigYAML: captureConfig, FixtureJSON: string(fixture) + " "}); quick == other {
		t.Fatal("different fixture bytes digest alike")
	}
}
