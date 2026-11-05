package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

const scenarioRunAssertion = "assertions: [{name: original, target: {exporter: otlp, signal: traces}, count: {equals: 3}}]\n"
const scenarioRunExtra = "assertions: [{name: appended, target: {exporter: otlp, signal: traces}, count: {min: 1}}]\n"

func scenarioRunFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func scenarioRunSubmit(t *testing.T, s *Service, req RunRequest) (string, *Report) {
	t.Helper()
	r, err := s.SubmitRun(context.Background(), testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s.GetRunReport(context.Background(), testCaller, r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID, rep
}

func TestScenarioFromQuickRunPreservesRecordedExporterOutput(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	config := `receivers: {otlp: {protocols: {grpc: {}}}}
processors:
  k8sattributes:
    auth_type: serviceAccount
    extract: {metadata: [k8s.pod.name]}
    pod_association: [{sources: [{from: connection}]}]
exporters: {otlp: {endpoint: example.invalid:4317}}
service: {pipelines: {traces: {receivers: [otlp], processors: [k8sattributes], exporters: [otlp]}}}
`
	setup := `mocks:
  kubernetes:
    objects:
      - apiVersion: v1
        kind: Pod
        metadata: {name: from-run-pod, namespace: default, uid: test-pod}
        spec: {nodeName: node-one}
        status: {podIP: 10.0.0.9}
execution: {timestamps: recorded}
`
	id, before := scenarioRunSubmit(t, s, RunRequest{Mode: ModeQuick, Config: Source{Text: config}, Normalize: true,
		Quick: &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}, Receiver: "otlp", ClientAddress: "10.0.0.9", Setup: setup, Assertions: scenarioRunAssertion}})
	bundleBefore, err := s.GetRunBundle(context.Background(), testCaller, id)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := s.EvaluateAssertions(context.Background(), testCaller, id, AssertionsRequest{ScenarioYAML: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n" + scenarioRunAssertion, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	runBefore, _ := s.GetRun(context.Background(), testCaller, id)
	listBefore, _ := allRuns(context.Background(), s, testCaller, RunFilter{})
	reportBefore := evaluationJSON(t, before)
	evalBefore := evaluationJSON(t, evaluation)
	out, err := s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{Name: "converted", Assertions: scenarioRunExtra})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := scenario.Parse([]byte(out.YAML), "")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Inputs[0].ClientAddress != "10.0.0.9" || parsed.Assertions[0].Name != "original" || parsed.Assertions[1].Name != "appended" || out.Saved != nil {
		t.Fatal("source details lost")
	}
	_, after := scenarioRunSubmit(t, s, RunRequest{Mode: ModeScenario, Config: Source{Text: config}, Normalize: true, Scenario: &ScenarioInput{Scenario: Source{Text: out.YAML}}})
	if len(before.Result.Outputs) == 0 || !bytes.Contains(before.Result.Outputs[0].Data, []byte("from-run-pod")) {
		t.Fatal("mock/client address had no observed effect")
	}
	if !bytes.Equal(evaluationJSON(t, before.Result.Outputs), evaluationJSON(t, after.Result.Outputs)) {
		t.Fatal("created scenario exporter evidence differs from quick run")
	}
	reportAfter, _ := s.GetRunReport(context.Background(), testCaller, id, false)
	bundleAfter, _ := s.GetRunBundle(context.Background(), testCaller, id)
	evalAfter, _ := s.GetEvaluation(context.Background(), testCaller, evaluation.ID)
	runAfter, _ := s.GetRun(context.Background(), testCaller, id)
	if !bytes.Equal(reportBefore, evaluationJSON(t, reportAfter)) || !bytes.Equal(bundleBefore, bundleAfter) || !bytes.Equal(evalBefore, evaluationJSON(t, evalAfter)) || !bytes.Equal(evaluationJSON(t, runBefore), evaluationJSON(t, runAfter)) {
		t.Fatal("original evidence changed")
	}
	// Only the explicit submission above may add history; conversion itself did not.
	if len(listBefore) != 1 || len(s.List()) != 2 {
		t.Fatal("conversion executed a run")
	}
}

func TestScenarioFromRunInlinesCapturedFilesAfterMutation(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.json")
	mock := filepath.Join(dir, "mock.log")
	fixture := scenarioRunFixture(t)
	if err := os.WriteFile(input, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mock, []byte("café\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: PathPolicy{Roots: []string{dir}}})
	defer s.Close()
	id, before := scenarioRunSubmit(t, s, RunRequest{Mode: ModeScenario, Config: Source{Text: bundleConfig}, Normalize: true, Scenario: &ScenarioInput{Dir: dir, Scenario: Source{Text: "version: 1\ninputs: [{file: input.json}]\nmocks: {files: [{path: /captured.log, file: mock.log}]}\n" + scenarioRunAssertion}}})
	bundleBefore, _ := s.GetRunBundle(context.Background(), testCaller, id)
	reportBefore := evaluationJSON(t, before)
	first, err := s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{Assertions: scenarioRunExtra})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte(`{"resourceSpans":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mock); err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{Assertions: scenarioRunExtra})
	if err != nil {
		t.Fatal(err)
	}
	if first.YAML != second.YAML || strings.Contains(first.YAML, "file:") {
		t.Fatal("conversion consulted current files")
	}
	parsed, err := scenario.Parse([]byte(first.YAML), "")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Inputs[0].Data.Value != fixture || *parsed.Mocks.Files[0].Content != "café\r\n" || parsed.Assertions[1].Name != "appended" {
		t.Fatal("capture bytes lost")
	}
	reportAfter, _ := s.GetRunReport(context.Background(), testCaller, id, false)
	bundleAfter, _ := s.GetRunBundle(context.Background(), testCaller, id)
	if !bytes.Equal(bundleBefore, bundleAfter) || !bytes.Equal(reportBefore, evaluationJSON(t, reportAfter)) {
		t.Fatal("original scenario run changed")
	}
	_, after := scenarioRunSubmit(t, s, RunRequest{Mode: ModeScenario, Config: Source{Text: bundleConfig}, Normalize: true, Scenario: &ScenarioInput{Scenario: Source{Text: first.YAML}}})
	if !bytes.Equal(evaluationJSON(t, before.Result.Outputs), evaluationJSON(t, after.Result.Outputs)) {
		t.Fatal("captured scenario output changed")
	}
}

func TestScenarioFromRunEnvironmentNamesNeverCopyHostValues(t *testing.T) {
	const secret = "distinctive-d5-host-value-keep-private"
	t.Setenv("OTEL_D5_HOST", secret)
	config := strings.Replace(bundleConfig, "exporters:", "processors: {resource: {attributes: [{key: host, value: '${env:OTEL_D5_HOST}', action: upsert}, {key: declared, value: '${env:OTEL_D5_DECLARED}', action: upsert}]}}\nexporters:", 1)
	config = strings.Replace(config, "receivers: [otlp], exporters:", "receivers: [otlp], processors: [resource], exporters:", 1)
	for _, mode := range []InputMode{ModeQuick, ModeScenario} {
		t.Run(string(mode), func(t *testing.T) {
			s := New(Options{Environment: EnvironmentPolicy{Host: []string{"OTEL_D5_HOST"}}})
			defer s.Close()
			req := RunRequest{Mode: mode, Config: Source{Text: config}}
			setup := "environment: {OTEL_D5_DECLARED: 'user-declared-value'}\n"
			if mode == ModeQuick {
				req.Quick = &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}, Setup: setup}
			} else {
				req.Scenario = &ScenarioInput{Scenario: Source{Text: "version: 1\n" + setup + "inputs: [{data: '" + strings.TrimSpace(scenarioRunFixture(t)) + "'}]\n"}}
			}
			id, rep := scenarioRunSubmit(t, s, req)
			if !bytes.Contains(rep.Result.Outputs[0].Data, []byte(secret)) {
				t.Fatal("host value was not captured")
			}
			out, err := s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out.RequiredEnvironment, []string{"OTEL_D5_HOST"}) || strings.Contains(out.YAML, secret) || !strings.Contains(out.YAML, "user-declared-value") {
				t.Fatal("environment boundary violated")
			}
		})
	}
}

func TestScenarioFromRunSaveCreatesNewOriginAcrossReopen(t *testing.T) {
	data := memory.NewData()
	s := openPlatform(t, data, Options{})
	ctx := context.Background()
	info, _ := s.GetWorkspaceInfo(ctx, testCaller)
	base, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Scenario, Label: "existing", Content: []byte("original exact bytes\r\n")})
	if err != nil {
		t.Fatal(err)
	}
	baseBefore, _ := s.GetArtifactRevision(ctx, testCaller, base.Artifact.ID, base.Artifact.Head)
	id, rep := scenarioRunSubmit(t, s, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig}, Quick: &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}, Assertions: scenarioRunAssertion}})
	bundleBefore, _ := s.GetRunBundle(ctx, testCaller, id)
	reportBefore := evaluationJSON(t, rep)
	eval, err := s.EvaluateAssertions(ctx, testCaller, id, AssertionsRequest{ScenarioYAML: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n" + scenarioRunAssertion, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	evalBefore := evaluationJSON(t, eval)
	runBefore, _ := s.GetRun(ctx, testCaller, id)
	journalBefore, err := s.repository.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recordsBefore, err := s.repository.Evaluations(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var saved []*SavedArtifact
	var contents []string
	for _, label := range []string{"", "chosen"} {
		out, err := s.CreateScenarioFromRun(ctx, Local("scenario-test"), id, ScenarioFromRunRequest{Save: true, Label: label})
		if err != nil {
			t.Fatal(err)
		}
		got := out.Saved
		sum := sha256.Sum256(bundleBefore)
		if got == nil || got.Artifact.ID == base.Artifact.ID || got.Artifact.Kind != artifact.Scenario || got.Artifact.Generation != 1 || got.Revision.Parent != "" || got.Revision.Actor != LocalOwner || !reflect.DeepEqual(got.Revision.Origin, &artifact.Origin{Kind: "run", RunID: id, BaseDigest: hex.EncodeToString(sum[:])}) {
			t.Fatalf("saved = %+v", got)
		}
		want := "scenario.yaml"
		if label != "" {
			want = label + ".yaml"
		}
		if got.Artifact.Label != want {
			t.Fatal("label changed")
		}
		saved = append(saved, got)
		contents = append(contents, out.YAML)
	}
	if saved[0].Artifact.ID == saved[1].Artifact.ID {
		t.Fatal("saved a revision instead of a new artifact")
	}
	s.Close()
	s = openPlatform(t, data, Options{})
	for i, got := range saved {
		stored, err := s.GetArtifactRevision(ctx, testCaller, got.Artifact.ID, got.Artifact.Head)
		if err != nil || !reflect.DeepEqual(stored.Revision, got.Revision) || string(stored.Content) != contents[i] {
			t.Fatal("origin lost on reopen", err)
		}
	}
	baseAfter, _ := s.GetArtifactRevision(ctx, testCaller, base.Artifact.ID, base.Artifact.Head)
	head, _ := s.GetArtifact(ctx, testCaller, base.Artifact.ID)
	if !reflect.DeepEqual(baseBefore, baseAfter) || head != base.Artifact {
		t.Fatal("existing artifact changed")
	}
	reportAfter, _ := s.GetRunReport(ctx, testCaller, id, false)
	bundleAfter, _ := s.GetRunBundle(ctx, testCaller, id)
	evalAfter, _ := s.GetEvaluation(ctx, testCaller, eval.ID)
	runAfter, _ := s.GetRun(ctx, testCaller, id)
	journalAfter, err := s.repository.Runs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recordsAfter, err := s.repository.Evaluations(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(evaluationJSON(t, journalBefore), evaluationJSON(t, journalAfter)) || !bytes.Equal(evaluationJSON(t, recordsBefore), evaluationJSON(t, recordsAfter)) {
		t.Fatal("run journal/evaluation records changed")
	}
	if !bytes.Equal(reportBefore, evaluationJSON(t, reportAfter)) || !bytes.Equal(bundleBefore, bundleAfter) || !bytes.Equal(evalBefore, evaluationJSON(t, evalAfter)) || !bytes.Equal(evaluationJSON(t, runBefore), evaluationJSON(t, runAfter)) {
		t.Fatal("save changed run evidence")
	}
}

func TestScenarioFromRunErrors(t *testing.T) {
	s := New(Options{})
	defer s.Close()
	ctx := context.Background()
	_, err := s.CreateScenarioFromRun(ctx, testCaller, "unknown", ScenarioFromRunRequest{})
	wantCode(t, err, CodeNotFound)
	_, err = s.CreateScenarioFromRun(ctx, testCaller, "unknown", ScenarioFromRunRequest{Save: true, Assertions: "invalid"})
	wantCode(t, err, CodeUnavailable)
	if !errors.Is(err, ErrWorkspaceRequired) {
		t.Fatal(err)
	}
	id, _ := scenarioRunSubmit(t, s, RunRequest{Mode: ModeQuick, Config: Source{Text: bundleConfig}, Quick: &QuickInput{Fixture: Source{Text: scenarioRunFixture(t)}}})
	_, err = s.CreateScenarioFromRun(ctx, testCaller, id, ScenarioFromRunRequest{Assertions: "inputs: []\n"})
	wantCode(t, err, CodeInvalidRequest)
	if !strings.HasPrefix(err.Error(), "assertions: ") {
		t.Fatal(err)
	}
	_, err = s.CreateScenarioFromRun(ctx, Caller{}, id, ScenarioFromRunRequest{})
	wantCode(t, err, CodeInvalidRequest)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.CreateScenarioFromRun(cancelled, testCaller, id, ScenarioFromRunRequest{})
	wantCode(t, err, CodeCancelled)
	legacy := New(evaluationOptions())
	defer legacy.Close()
	legacyID := evaluationRun(t, legacy, ModeQuick)
	_, err = legacy.CreateScenarioFromRun(ctx, testCaller, legacyID, ScenarioFromRunRequest{})
	wantCode(t, err, CodeNotFound)
	if !errors.Is(err, ErrBundleUnavailable) {
		t.Fatal(err)
	}
	// An unreadable private envelope is storage_error with a safe message.
	h, _ := s.Get(id)
	h.mu.Lock()
	h.bundle = []byte("private-corrupt-content")
	h.mu.Unlock()
	_, err = s.CreateScenarioFromRun(ctx, testCaller, id, ScenarioFromRunRequest{})
	wantCode(t, err, CodeStorageError)
	if strings.Contains(err.Error(), "private-corrupt") {
		t.Fatal("private archive escaped error")
	}
}

func TestScenarioFromRunAcceptsNonterminalStatesWithoutExecution(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := New(Options{Execute: func(ctx context.Context, req engine.Request) (*engine.Report, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return report(req, "completed"), nil
	}})
	defer s.Close()
	active := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	<-started
	queued := submit(t, s, engine.Request{ConfigYAML: bundleConfig, ScenarioYAML: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n"})
	for _, h := range []*Handle{active, queued} {
		before := evaluationJSON(t, h.View(true))
		bundle, _ := s.GetRunBundle(context.Background(), testCaller, h.ID())
		if _, err := s.CreateScenarioFromRun(context.Background(), testCaller, h.ID(), ScenarioFromRunRequest{}); err != nil {
			t.Fatal(err)
		}
		afterBundle, _ := s.GetRunBundle(context.Background(), testCaller, h.ID())
		if !bytes.Equal(before, evaluationJSON(t, h.View(true))) || !bytes.Equal(bundle, afterBundle) {
			t.Fatal("nonterminal run changed")
		}
	}
	queued.Cancel()
	waitRun(t, queued)
	if _, err := s.CreateScenarioFromRun(context.Background(), testCaller, queued.ID(), ScenarioFromRunRequest{}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitRun(t, active)
}

func TestScenarioFromRunRefusesBinaryMockAndMissingCapture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binary.log")
	if err := os.WriteFile(path, []byte{0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{Paths: PathPolicy{Roots: []string{dir}}})
	defer s.Close()
	id, _ := scenarioRunSubmit(t, s, RunRequest{Mode: ModeScenario, Config: Source{Text: bundleConfig}, Scenario: &ScenarioInput{Dir: dir, Scenario: Source{Text: "version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\nmocks: {files: [{path: /binary.log, file: binary.log}]}\n"}}})
	before, _ := s.GetRunBundle(context.Background(), testCaller, id)
	_, err := s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{})
	wantCode(t, err, CodeInvalidRequest)
	if err.Error() != "mocks.files[0]: the captured file is not UTF-8 text and cannot be inlined" {
		t.Fatal(err)
	}
	after, _ := s.GetRunBundle(context.Background(), testCaller, id)
	if !bytes.Equal(before, after) {
		t.Fatal("refusal changed evidence")
	}
	// A syntactically readable envelope missing a dependency is corrupt evidence.
	var envelope map[string]any
	if err := json.Unmarshal(before, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["files"] = map[string]any{}
	corrupt, _ := json.Marshal(envelope)
	h, _ := s.Get(id)
	h.mu.Lock()
	h.bundle = corrupt
	h.mu.Unlock()
	_, err = s.CreateScenarioFromRun(context.Background(), testCaller, id, ScenarioFromRunRequest{})
	wantCode(t, err, CodeStorageError)
}
