package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestEphemeralBundleReadsAreCopiesAndMetadataOmitsPrivateValues(t *testing.T) {
	const secret = "private-environment-binding"
	t.Setenv("OTEL_BUNDLE_PRIVATE", secret)
	collectorYAML := "receivers: {otlp: {protocols: {grpc: {}}}}\nprocessors: {resource: {attributes: [{key: private, value: '${env:OTEL_BUNDLE_PRIVATE}', action: upsert}]}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], processors: [resource], exporters: [otlp]}}}\n"
	s := New(Options{Environment: config.EnvironmentPolicy{Host: []string{"OTEL_BUNDLE_PRIVATE"}}, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}})
	t.Cleanup(s.Close)
	r := submit(t, s, engine.Request{ConfigYAML: collectorYAML, FixtureJSON: `{"resourceSpans":[]}`})
	waitRun(t, r)
	metadata, _ := json.Marshal(s.List())
	if bytes.Contains(metadata, []byte(secret)) || bytes.Contains(metadata, []byte("config_snapshot")) || bytes.Contains(metadata, []byte("fixture_base64")) {
		t.Fatal("private data escaped metadata")
	}
	raw, err := s.GetRunBundle(context.Background(), testCaller, r.ID())
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(raw)
	raw[0] = '!'
	again, err := s.GetRunBundle(context.Background(), testCaller, r.ID())
	if err != nil || !bytes.Equal(again, original) {
		t.Fatal("private read aliases retained bundle", err)
	}
	if err := os.Setenv("OTEL_BUNDLE_PRIVATE", "different"); err != nil {
		t.Fatal(err)
	}
	prepared, err := engine.DecodeBundle(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	private, _, err := engine.EncodeBundle(prepared)
	if err != nil || !bytes.Equal(private, original) {
		t.Fatal("restored environment changed bundle", err)
	}
}

func TestWorkspaceRejectsRunBundleMetadataMismatch(t *testing.T) {
	data := memory.NewData()
	s := openPlatform(t, data, Options{Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}})
	r := submit(t, s, engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`})
	waitRun(t, r)
	s.Close()
	// Rebuild the stored run in new storage with metadata that disagrees with
	// its bundle.
	ctx := context.Background()
	store := memory.Open(data, artifact.Limits{})
	if err := store.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	records, err := store.Runs(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("runs = %v, %v", records, err)
	}
	record := records[0]
	bundle, err := store.RunBundle(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.RunEvidence(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	var metadata runMetadata
	if err := json.Unmarshal(record.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Bundle.Bytes++
	tampered, _ := json.Marshal(metadata)
	bad := memory.New(artifact.Limits{})
	if err := bad.EnableRuns(ctx); err != nil {
		t.Fatal(err)
	}
	admitted, err := bad.AdmitRun(ctx, artifact.Admission{ID: record.ID, Project: record.Project, Metadata: record.Metadata, Bundle: bundle, Runtime: "builtin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.SaveRun(ctx, record.ID, record.Project, admitted.Generation, true, artifact.RunIndex{}, tampered, evidence); err != nil {
		t.Fatal(err)
	}
	// A terminal run is read when named, so the mismatch
	// fails that read rather than opening the workspace.
	reopened, err := Open(bad, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	if _, err := reopened.GetRun(ctx, testCaller, record.ID); !errors.Is(err, artifact.ErrCorrupt) || codeOf(err) != CodeStorageError {
		t.Fatalf("metadata/bundle mismatch accepted: %v", err)
	}
}

const bundleConfig = "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: example.invalid:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"

func TestWorkspaceBundlesSurviveRestartAndExplicitlyExecuteBothModes(t *testing.T) {
	data, sources := memory.NewData(), t.TempDir()
	configPath, inputPath, scenarioPath := filepath.Join(sources, "config.yaml"), filepath.Join(sources, "input.json"), filepath.Join(sources, "scenario.yaml")
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "traces", "traces.json"))
	if err != nil {
		t.Fatal(err)
	}
	for path, raw := range map[string][]byte{configPath: []byte(bundleConfig), inputPath: fixture, scenarioPath: []byte("version: 1\ninputs: [{file: input.json}]\n")} {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := openPlatform(t, data, Options{Paths: PathPolicy{Roots: []string{sources}}})
	var ids []string
	var original [][]byte
	for _, req := range []engine.Request{{Config: configPath, Fixture: inputPath, Compare: true}, {Config: configPath, Scenario: scenarioPath, Compare: true}} {
		r := submit(t, s, req)
		if got := waitRun(t, r); got.State != StateDone || got.Bundle == nil {
			t.Fatalf("run without bundle: %+v", got)
		}
		raw, err := s.GetRunBundle(context.Background(), testCaller, r.ID())
		if err != nil {
			t.Fatal(err)
		}
		info, err := engine.InspectBundle(raw)
		if err != nil || !reflect.DeepEqual(info, r.View(false).Bundle) {
			t.Fatal("metadata binding differs", err)
		}
		view := r.View(false)
		view.Bundle.Digest = "changed"
		if r.View(false).Bundle.Digest == "changed" {
			t.Fatal("metadata view aliases handle")
		}
		ids = append(ids, r.ID())
		original = append(original, raw)
	}
	s.Close()
	for _, path := range []string{configPath, inputPath, scenarioPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	s = openPlatform(t, data, Options{})
	for i, id := range ids {
		raw, err := s.GetRunBundle(context.Background(), testCaller, id)
		if err != nil || !bytes.Equal(raw, original[i]) {
			t.Fatal("bundle changed after restart", err)
		}
		req, err := engine.DecodeBundle(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		retry := submit(t, s, req)
		if retry.ID() == id {
			t.Fatal("explicit execution reused original run ID")
		}
		got := waitRun(t, retry)
		if got.State != StateDone || !got.Report.Comparison.Equal {
			t.Fatalf("captured execution failed: %+v", got)
		}
	}
}

func TestBundleQuotaRejectsAdmissionAndLegacyBundleReadsAreExplicit(t *testing.T) {
	s := openPlatformLimits(t, memory.NewData(), artifact.Limits{BundleBytes: 1}, Options{})
	if _, err := s.Submit(engine.Request{ConfigYAML: bundleConfig, FixtureJSON: `{"resourceSpans":[]}`}); !errors.Is(err, ErrPersistence) || !errors.Is(err, artifact.ErrLimit) {
		t.Fatalf("bundle admission quota = %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatal("bundle quota failure admitted run")
	}
	legacy := New(Options{Prepare: unprepared, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return report(req, "completed"), nil
	}})
	t.Cleanup(legacy.Close)
	r := submit(t, legacy, request("test double"))
	waitRun(t, r)
	if _, err := legacy.GetRunBundle(context.Background(), testCaller, r.ID()); !errors.Is(err, ErrBundleUnavailable) {
		t.Fatalf("missing bundle = %v", err)
	}
}
