package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/debugger/assertion"
)

func TestQuickAssertionsBundleRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Comments and CRLF are retained exactly, rather than serializing assertions.
	text := "# private assertion source\r\nversion: 1\r\n" + string(bytes.ReplaceAll([]byte(quickAssertions), []byte("\n"), []byte("\r\n")))
	req, err := Prepare(ctx, Request{ConfigYAML: captureConfig, Fixture: tracesFixture, AssertionsYAML: text})
	if err != nil {
		t.Fatal(err)
	}
	raw, info, err := EncodeBundle(req)
	if err != nil || info.Schema != builtInBundleSchema {
		t.Fatalf("bundle = %+v, %v", info, err)
	}
	var a bundleArchive
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Assertions, []byte(text)) {
		t.Fatal("bundle lost original assertion bytes")
	}
	restored, err := DecodeBundle(ctx, raw)
	if err != nil || restored.AssertionsYAML != text {
		t.Fatalf("decoded assertions differ: %v", err)
	}
	again, _, err := EncodeBundle(restored)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("bundle bytes changed: %v", err)
	}
	rep, err := Execute(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Assertions == nil || rep.Assertions.Outcome != assertion.Pass || rep.Provenance.Assertions == nil || *rep.Provenance.Assertions != sourceFromBytes("", []byte(text)) {
		t.Fatalf("decoded assertions were not evaluated/hashed: %+v", rep)
	}
	a.Request.Limits = nil // legacy bundles carry no limits
	for _, schema := range []int{1, 2} {
		a.Schema = schema
		legacy, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeBundle(ctx, legacy); !errors.Is(err, ErrBundleSchema) {
			t.Fatalf("schema %d carrying assertions = %v", schema, err)
		}
	}
	a.Schema, a.Mode, a.Fixture, a.Request.Fixture, a.FixtureFile = 3, "scenario", nil, "", ""
	a.Scenario = []byte("version: 1\ninputs: [{data: '{\"resourceSpans\":[]}'}]\n")
	mixed, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundle(ctx, mixed); !errors.Is(err, ErrBundle) {
		t.Fatalf("scenario carrying separate assertions = %v", err)
	}
}

func TestSchemaTwoBundlesKeepTheirBytes(t *testing.T) {
	req, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(req)
	if err != nil {
		t.Fatal(err)
	}
	var a bundleArchive
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	a.Schema, a.Request.Limits = 2, nil
	legacy, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeBundle(context.Background(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	again, info, err := EncodeBundle(restored)
	if err != nil || info.Schema != 2 || !bytes.Equal(legacy, again) {
		t.Fatalf("schema 2 bytes changed: %+v, %v", info, err)
	}
}

func TestBundleRoundTripExecutesBothModesWithoutSourceReads(t *testing.T) {
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fixture, err := os.ReadFile(tracesFixture)
			if err != nil {
				t.Fatal(err)
			}
			configPath, inputPath, scenarioPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "input.json"), filepath.Join(dir, "scenario.yaml")
			for path, raw := range map[string][]byte{configPath: []byte(captureConfig), inputPath: fixture, scenarioPath: []byte("# original\r\nversion: 1\r\ninputs: [{file: input.json}]\r\n")} {
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			req := Request{Config: configPath, Fixture: inputPath, Compare: true, Normalize: true}
			if mode == "scenario" {
				req.Fixture, req.Scenario = "", scenarioPath
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			prepared, err := Prepare(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			raw, info, err := EncodeBundle(prepared)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode != mode || info.Reproducibility != "unverified" || info.ReproductionReason == "" {
				t.Fatalf("bundle claims: %+v", info)
			}
			prepared.RunID = "runtime-only"
			again, other, err := EncodeBundle(prepared)
			if err != nil || !bytes.Equal(raw, again) || !reflect.DeepEqual(info, other) {
				t.Fatal("runtime identity changed bundle", err)
			}
			for _, path := range []string{configPath, inputPath, scenarioPath} {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			restored, err := DecodeBundle(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := Execute(ctx, restored)
			if err != nil || !rep.Comparison.Equal {
				t.Fatalf("restored execution: %v, %+v", err, rep)
			}
			if rep.Provenance.Config.Path != configPath {
				t.Fatal("original source label lost")
			}
			reencoded, _, err := EncodeBundle(restored)
			if err != nil || !bytes.Equal(raw, reencoded) {
				t.Fatal("restored input bytes changed", err)
			}
		})
	}
}

func TestBundleRejectsCorruptEnvelopeAndBuildMismatchWithoutBlockingInspection(t *testing.T) {
	prepared, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var original bundleArchive
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"schema", "mixed mode", "missing files", "unknown field", "missing input", "budget", "distribution"} {
		t.Run(kind, func(t *testing.T) {
			a := original
			switch kind {
			case "schema":
				a.Schema++
			case "mixed mode":
				a.Scenario = []byte("version: 1")
			case "missing files":
				a.Files = nil
			case "missing input":
				a.Fixture = nil
			case "budget":
				a.Files = map[string][]byte{"large": make([]byte, maxInputBytes)}
			case "distribution":
				a.Distribution.Arch = "different"
			}
			changed, _ := json.Marshal(a)
			if kind == "unknown field" {
				changed = append(changed[:len(changed)-1], []byte(`,"unknown":true}`)...)
			}
			_, err := DecodeBundle(context.Background(), changed)
			if kind == "distribution" {
				if !errors.Is(err, ErrDistribution) {
					t.Fatalf("build mismatch = %v", err)
				}
				if _, err := InspectBundle(changed); err != nil {
					t.Fatalf("old build cannot be inspected: %v", err)
				}
			} else if err == nil {
				t.Fatal("invalid bundle accepted")
			}
		})
	}
	prepared.prepared.distribution.Arch = "different"
	if _, err := Execute(context.Background(), prepared); !errors.Is(err, ErrDistribution) {
		t.Fatalf("worker skipped compatibility: %v", err)
	}
}

func TestBundleWithLegacySnapshotRetainsHistoricalMetadata(t *testing.T) {
	prepared, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var archive bundleArchive
	if err := json.Unmarshal(raw, &archive); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(archive.Config, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["schema"] = json.RawMessage(`1`)
	delete(snapshot, "environment_policy")
	archive.Config, _ = json.Marshal(snapshot)
	legacy, _ := json.Marshal(archive)
	info, err := InspectBundle(legacy)
	if err != nil || info.Environment != "" {
		t.Fatal("legacy metadata changed", err)
	}
	restored, err := DecodeBundle(context.Background(), legacy)
	if err != nil {
		t.Fatal("legacy snapshot became unreadable", err)
	}
	again, _, err := EncodeBundle(restored)
	if err != nil || !bytes.Equal(again, legacy) {
		t.Fatal("legacy bytes changed", err)
	}
	snapshot["schema"] = json.RawMessage(`999`)
	archive.Config, _ = json.Marshal(snapshot)
	future, _ := json.Marshal(archive)
	if _, err := InspectBundle(future); !errors.Is(err, ErrBundleSchema) {
		t.Fatal("unknown nested schema not classified", err)
	}
}

func TestBundleCompatibilityFollowsBuildIdentityNotTheExecutable(t *testing.T) {
	prepared, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, info, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var original bundleArchive
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	if info.Schema != builtInBundleSchema || original.Distribution.Build == nil || original.Distribution.Build.Dependencies == "" {
		t.Fatalf("new bundles must carry build identity: %+v %+v", info, original.Distribution.Build)
	}
	encode := func(a bundleArchive) []byte {
		changed, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	withBuild := func(change func(*buildIdentity)) bundleArchive {
		a := original
		b := *original.Distribution.Build
		change(&b)
		a.Distribution.Build = &b
		return a
	}

	rebuilt := original
	rebuilt.Distribution.Executable = "sha256:another-binary-of-the-same-source"
	if _, err := DecodeBundle(context.Background(), encode(rebuilt)); err != nil {
		t.Fatalf("a rebuild of the same source cannot replay: %v", err)
	}
	for name, a := range map[string]bundleArchive{
		"dependencies": withBuild(func(b *buildIdentity) { b.Dependencies = "sha256:other" }),
		"revision":     withBuild(func(b *buildIdentity) { b.Revision = "another-commit" }),
		"settings":     withBuild(func(b *buildIdentity) { b.Settings = append(b.Settings, "-tags=other") }),
	} {
		if _, err := DecodeBundle(context.Background(), encode(a)); !errors.Is(err, ErrDistribution) {
			t.Fatalf("%s mismatch = %v", name, err)
		}
	}
}

func TestSchemaOneBundlesKeepTheExactExecutableRule(t *testing.T) {
	prepared, err := Prepare(context.Background(), Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var a bundleArchive
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	digest, err := executableDigest()
	if err != nil {
		t.Skip("executable unreadable:", err)
	}
	a.Schema, a.Distribution.Build, a.Distribution.Executable, a.Request.Limits = 1, nil, digest, nil
	legacy, _ := json.Marshal(a)
	info, err := InspectBundle(legacy)
	if err != nil || info.Schema != 1 {
		t.Fatalf("legacy bundle unreadable: %v %+v", err, info)
	}
	restored, err := DecodeBundle(context.Background(), legacy)
	if err != nil {
		t.Fatal("same executable rejected:", err)
	}
	again, _, err := EncodeBundle(restored)
	if err != nil || !bytes.Equal(again, legacy) {
		t.Fatal("legacy bundle did not re-encode to its own bytes", err)
	}

	a.Distribution.Executable = "sha256:other"
	other, _ := json.Marshal(a)
	if _, err := DecodeBundle(context.Background(), other); !errors.Is(err, ErrDistribution) {
		t.Fatalf("legacy bundle from another executable = %v", err)
	}
	a.Distribution.Build = &buildIdentity{Main: "m@v", Dependencies: "sha256:x"}
	mixed, _ := json.Marshal(a)
	if _, err := InspectBundle(mixed); !errors.Is(err, ErrBundle) {
		t.Fatalf("schema 1 with build identity accepted: %v", err)
	}
}

// Labels and source references (schema 5) survive the round trip and are
// refused in older schemas, which could not record them.
func TestBundleRecordsLabelsAndSourceReferences(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	refs := &SourceRefs{Config: &SourceRef{ArtifactID: "a1", RevisionID: "r1"}, Fixture: &SourceRef{ArtifactID: "a2", RevisionID: "r2"}}
	req, err := Prepare(ctx, Request{ConfigYAML: captureConfig, ConfigLabel: "repo/config.yaml", FixtureJSON: `{"resourceSpans":[]}`, FixtureLabel: "input.json", Sources: refs})
	if err != nil {
		t.Fatal(err)
	}
	raw, info, err := EncodeBundle(req)
	if err != nil || info.Schema != 5 || !reflect.DeepEqual(info.Sources, refs) {
		t.Fatalf("bundle = %+v, %v", info, err)
	}
	restored, err := DecodeBundle(ctx, raw)
	if err != nil || restored.ConfigLabel != "repo/config.yaml" || restored.FixtureLabel != "input.json" || !reflect.DeepEqual(restored.Sources, refs) {
		t.Fatalf("decoded = %+v, %v", restored, err)
	}
	if again, _, err := EncodeBundle(restored); err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("bundle bytes changed: %v", err)
	}
	rep, err := Execute(ctx, restored)
	if err != nil || rep.Provenance.Config.Path != "repo/config.yaml" || rep.Provenance.Fixture.Path != "input.json" {
		t.Fatalf("provenance = %+v, %v", rep.Provenance, err)
	}
	var a bundleArchive
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	a.Schema = 4
	legacy, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(legacy); !errors.Is(err, ErrBundleSchema) {
		t.Fatalf("schema 4 carrying labels = %v", err)
	}
	for _, bad := range []Request{
		{ConfigYAML: captureConfig, FixtureJSON: `{}`, Config: "x.yaml", ConfigLabel: "y"},
		{Config: "x.yaml", ConfigLabel: "y", FixtureJSON: `{}`},
		{ConfigYAML: captureConfig, Fixture: "in.json", FixtureLabel: "y"},
		{ConfigYAML: captureConfig, Fixture: "in.json", Sources: &SourceRefs{Fixture: &SourceRef{ArtifactID: "a", RevisionID: "r"}}},
		{ConfigYAML: captureConfig, FixtureJSON: `{}`, Sources: &SourceRefs{}},
		{ConfigYAML: captureConfig, FixtureJSON: `{}`, Sources: &SourceRefs{Config: &SourceRef{ArtifactID: "a"}}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Validate(%+v) = %v", bad, err)
		}
	}
}

func TestBundleRecordsTheTargetRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := Prepare(ctx, Request{ConfigYAML: captureConfig, FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	builtIn, info, err := EncodeBundle(req)
	if err != nil || info.Schema != builtInBundleSchema {
		t.Fatalf("built-in bundle = %+v, %v", info, err)
	}
	target := &RuntimeTarget{ID: "sha256:aaaa", Name: "custom"}
	req.Runtime = target
	raw, info, err := EncodeBundle(req)
	if err != nil || info.Schema != 6 {
		t.Fatalf("runtime bundle = %+v, %v", info, err)
	}
	if bytes.Equal(builtIn, raw) {
		t.Fatal("the runtime target is not recorded")
	}

	// The built-in runtime refuses it, as does another runtime; its own
	// runtime decodes it and re-encodes the same bytes.
	if _, err := DecodeBundle(ctx, raw); !errors.Is(err, ErrDistribution) || !strings.Contains(err.Error(), "custom") {
		t.Fatalf("built-in decode = %v", err)
	}
	if _, err := DecodeBundleForRuntime(ctx, raw, "sha256:bbbb"); !errors.Is(err, ErrDistribution) {
		t.Fatalf("other runtime decode = %v", err)
	}
	if _, err := DecodeBundleForRuntime(ctx, builtIn, "sha256:aaaa"); !errors.Is(err, ErrDistribution) {
		t.Fatalf("runtime decode of a built-in bundle = %v", err)
	}
	restored, err := DecodeBundleForRuntime(ctx, raw, "sha256:aaaa")
	if err != nil || !reflect.DeepEqual(restored.Runtime, target) {
		t.Fatalf("decoded = %+v, %v", restored.Runtime, err)
	}
	if again, _, err := EncodeBundle(restored); err != nil || !bytes.Equal(raw, again) {
		t.Fatalf("bundle bytes changed: %v", err)
	}
	if rep, err := Execute(ctx, restored); err != nil || rep.Result == nil {
		t.Fatalf("execute = %v", err)
	}

	// Older schemas cannot carry a runtime, and schema 6 requires one.
	var a bundleArchive
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	a.Schema = 5
	legacy, _ := json.Marshal(a)
	if _, err := InspectBundle(legacy); !errors.Is(err, ErrBundleSchema) {
		t.Fatalf("schema 5 carrying a runtime = %v", err)
	}
	a.Schema, a.Runtime = 6, nil
	missing, _ := json.Marshal(a)
	if _, err := InspectBundle(missing); !errors.Is(err, ErrBundle) {
		t.Fatalf("schema 6 without a runtime = %v", err)
	}
	req.Runtime = &RuntimeTarget{Name: "no id"}
	if _, _, err := EncodeBundle(req); !errors.Is(err, ErrBundle) {
		t.Fatalf("runtime without an ID = %v", err)
	}
}
