package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/hostfs"
	"github.com/lineahq/linea/internal/debugger/registry"
)

func TestUnavailableReferencesResolveToStandIns(t *testing.T) {
	factories, err := registry.Factories()
	if err != nil {
		t.Fatal(err)
	}
	root, outside := t.TempDir(), t.TempDir()
	missing := filepath.ToSlash(filepath.Join(root, "missing.txt"))
	refused := filepath.ToSlash(filepath.Join(outside, "secret.txt"))
	writeConfigFile(t, refused, "never-read")
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\n" +
		"exporters:\n  otlp:\n    endpoint: localhost:4317\n    headers:\n" +
		"      a: '${file:" + missing + "}'\n" +
		"      b: '${file:" + refused + "}'\n" +
		"      c: '${file:/run/lab/token}'\n" +
		"      d: '${env:OTEL_STAND_IN_UNBOUND}'\n" +
		"      e: '${vault:secret/data/x}'\n" +
		"service: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	opts := CaptureOptions{
		Paths: hostfs.Policy{Roots: []string{root}},
		Files: map[string][]byte{"/run/lab/token": []byte("from-scenario")},
	}
	resolved, err := CaptureSourceWith(context.Background(), Source{YAML: yaml}, factories, nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	where := []string{"exporter/otlp"}
	want := []Reference{
		{Scheme: "env", Target: "OTEL_STAND_IN_UNBOUND", Source: SourceStandIn, Reason: ReasonUnbound, StandIn: standIn("env:OTEL_STAND_IN_UNBOUND"), Components: where},
		{Scheme: "file", Target: "/run/lab/token", Source: SourceScenario, Components: where},
		{Scheme: "file", Target: missing, Source: SourceStandIn, Reason: ReasonMissing, StandIn: standIn("file:" + missing), Components: where},
		{Scheme: "file", Target: refused, Source: SourceStandIn, Reason: ReasonOutsideRoots, StandIn: standIn("file:" + refused), Components: where},
		{Scheme: "vault", Target: "secret/data/x", Source: SourceStandIn, Reason: ReasonUnsupportedScheme, StandIn: standIn("vault:secret/data/x"), Components: where},
	}
	if !reflect.DeepEqual(resolved.References, want) {
		t.Fatalf("references\n got %+v\nwant %+v", resolved.References, want)
	}
	if diags := StandInDiagnostics(resolved.References); len(diags) != 4 || diags[0].Code != CodeReferenceStandIn || diags[0].Severity != SeverityWarning || diags[0].Component != "exporter/otlp" {
		t.Fatalf("diagnostics %+v", diags)
	}

	// The snapshot replays the same values without the host or the scenario.
	raw, err := json.Marshal(resolved.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "never-read") {
		t.Fatal("a refused file was read")
	}
	decoded, err := DecodeSnapshot(raw, CaptureLimits{})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := decoded.Resolve(context.Background(), factories)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Effective(resolved.Config)
	b, _ := Effective(replayed.Config)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(replayed.References, want) {
		t.Fatal("replay differs")
	}
}

func TestStandInsInTypedSettingsNameTheirReference(t *testing.T) {
	factories, _ := registry.Factories()
	yaml := "receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: localhost:4317, timeout: '${env:OTEL_STAND_IN_TIMEOUT}'}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}\n"
	_, err := CaptureSourceWith(context.Background(), Source{YAML: yaml}, factories, nil, CaptureOptions{Paths: hostfs.Policy{Unrestricted: true}})
	diags := AsDiagnostics(err)
	if len(diags) != 1 || diags[0].Severity != SeverityError || !strings.Contains(diags[0].Hint, "${env:OTEL_STAND_IN_TIMEOUT} (unbound)") {
		t.Fatalf("diagnostics %+v", diags)
	}
}

func TestConfigurationPathIsNeverAStandIn(t *testing.T) {
	factories, _ := registry.Factories()
	_, err := CaptureSourceWith(context.Background(), Source{Path: filepath.Join(t.TempDir(), "absent.yaml")}, factories, nil, CaptureOptions{Paths: hostfs.Policy{Unrestricted: true}})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing configuration accepted", err)
	}
}

func TestDecodedSnapshotsRejectForgedStandIns(t *testing.T) {
	for _, raw := range []string{
		// Schema 2 has no stand-ins.
		`{"schema":2,"uris":["yaml:a: b"],"values":{"env:X":{"content_base64":"` + b64(standIn("env:X")) + `","binding":"stand_in","reason":"unbound"}},"environment_policy":{}}`,
		// A stand-in carries exactly its derived string.
		`{"schema":3,"uris":["yaml:a: b"],"values":{"env:X":{"content_base64":"` + b64("private") + `","binding":"stand_in","reason":"unbound"}},"environment_policy":{}}`,
		// Reasons belong to their scheme.
		`{"schema":3,"uris":["yaml:a: b"],"values":{"file:/x":{"content_base64":"` + b64(standIn("file:/x")) + `","binding":"stand_in","reason":"unbound"}},"environment_policy":{}}`,
		// Other schemes have only stand-ins.
		`{"schema":3,"uris":["yaml:a: b"],"values":{"vault:x":{"content_base64":"` + b64("v") + `"}},"environment_policy":{}}`,
	} {
		if _, err := DecodeSnapshot([]byte(raw), CaptureLimits{}); !errors.Is(err, ErrSnapshot) {
			t.Fatal("forged snapshot accepted", raw, err)
		}
	}
}

func b64(s string) string {
	raw, _ := json.Marshal([]byte(s))
	return strings.Trim(string(raw), `"`)
}
