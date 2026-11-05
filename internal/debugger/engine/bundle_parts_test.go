package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadBundlePartsUsesCapturedRootsWithoutDistributionCheck(t *testing.T) {
	for _, mode := range []string{"quick", "scenario"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fixture := []byte(`{"resourceSpans":[]}`)
			inputPath := filepath.Join(dir, "input.json")
			if err := os.WriteFile(inputPath, fixture, 0o600); err != nil {
				t.Fatal(err)
			}
			scenarioPath := filepath.Join(dir, "scenario.yaml")
			scenarioRaw := []byte("version: 1\ninputs: [{file: input.json, receiver: otlp, client_address: 10.0.0.1}]\n")
			if err := os.WriteFile(scenarioPath, scenarioRaw, 0o600); err != nil {
				t.Fatal(err)
			}
			req := Request{ConfigYAML: captureConfig, Fixture: inputPath, Receiver: "otlp", ClientAddress: "10.0.0.1", SetupYAML: "environment: {DECLARED: user-value}\n", AssertionsYAML: quickAssertions}
			if mode == "scenario" {
				req = Request{ConfigYAML: captureConfig, Scenario: scenarioPath}
			}
			prepared, err := Prepare(context.Background(), req)
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
			archive.Distribution.OS = "another-os"
			raw, _ = json.Marshal(archive)
			before := bytes.Clone(raw)
			if err := os.Remove(inputPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(scenarioPath); err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeBundle(context.Background(), raw); !errors.Is(err, ErrDistribution) {
				t.Fatal("test bundle should require another distribution", err)
			}
			parts, err := ReadBundleParts(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parts.Mode != mode || !bytes.Equal(parts.Files[inputPath], fixture) || len(parts.HostEnvironment) != 0 {
				t.Fatalf("parts = %+v", parts)
			}
			if mode == "quick" && (!bytes.Equal(parts.Fixture, fixture) || parts.Receiver != "otlp" || parts.ClientAddress != "10.0.0.1" || string(parts.Setup) != req.SetupYAML || string(parts.Assertions) != quickAssertions) {
				t.Fatal("quick parts changed")
			}
			if mode == "scenario" && (!bytes.Equal(parts.Scenario, scenarioRaw) || parts.ScenarioDir != dir) {
				t.Fatal("scenario parts changed")
			}
			parts.Files[inputPath][0] = '!'
			again, err := ReadBundleParts(raw)
			if err != nil || !bytes.Equal(again.Files[inputPath], fixture) || !bytes.Equal(raw, before) {
				t.Fatal("parts alias archive bytes", err)
			}
		})
	}
}

func TestReadBundlePartsRequiredEnvironmentAndLegacySchemas(t *testing.T) {
	const name = "OTEL_PARTS_PRIVATE"
	t.Setenv(name, "parts-private-value:4317")
	req, err := Prepare(context.Background(), Request{ConfigYAML: "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: '${env:" + name + "}'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n", FixtureJSON: `{"resourceSpans":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := EncodeBundle(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []int{1, 2, 3} {
		var archive bundleArchive
		if err := json.Unmarshal(raw, &archive); err != nil {
			t.Fatal(err)
		}
		archive.Schema, archive.Request.Limits = schema, nil
		if schema == 1 {
			archive.Distribution.Build = nil
			archive.Distribution.Executable = "old-executable"
		}
		encoded, _ := json.Marshal(archive)
		parts, err := ReadBundleParts(encoded)
		if err != nil || !reflect.DeepEqual(parts.HostEnvironment, []string{name}) {
			t.Fatalf("schema %d names = %+v %v", schema, parts, err)
		}
	}
	for _, bad := range [][]byte{[]byte("private malformed source"), []byte(`{"bundle_schema":99}`)} {
		if _, err := ReadBundleParts(bad); err == nil {
			t.Fatal("invalid archive accepted")
		}
	}
}
