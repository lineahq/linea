package catalog

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/build"
	"github.com/lineahq/linea/build/versions"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

func release(t *testing.T) *versions.Catalog {
	t.Helper()
	r := versions.Embedded().Release("0.162.0")
	if r == nil {
		t.Fatal("0.162.0 is not in the support table")
	}
	return r.Catalog
}

func TestThePresetsWriteManifests(t *testing.T) {
	cat := release(t)
	all, err := Compose(cat, build.Manifest, Request{Preset: PresetAll})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(all.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	supported := 0
	for _, c := range cat.Components {
		if c.Unsupported == "" {
			supported++
		}
	}
	if got := len(m.Modules()); got != supported || got < 200 {
		t.Fatalf("all: %d modules, the catalog supports %d", got, supported)
	}
	if slices.ContainsFunc(m.Modules(), func(s string) bool { return strings.Contains(s, "asapauthextension") }) {
		t.Fatal("all: an unsupported component is in the preset")
	}
	if m.CollectorVersion() != "0.162.0" || len(m.Providers) == 0 || !slices.Equal(m.Replaces, cat.Replaces) {
		t.Fatalf("all: version %s, providers %d, replaces %q", m.CollectorVersion(), len(m.Providers), m.Replaces)
	}

	builtIn, err := Compose(cat, build.Manifest, Request{Preset: PresetBuiltIn})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(builtIn.Manifest, build.Manifest) {
		t.Fatal("built-in: the manifest changed")
	}
	other := *cat
	other.CollectorVersion = "0.161.0"
	if _, err := Compose(&other, build.Manifest, Request{Preset: PresetBuiltIn}); err == nil {
		t.Fatal("built-in: another release was accepted")
	}
}

func TestDeriveSelectsWhatAConfigurationDefines(t *testing.T) {
	config := []byte(`
receivers:
  otlp:
  otlp/second:
processors:
  transform/clean:
  stamp:
exporters:
  debug:
extensions:
  health_check:
service:
  pipelines:
    logs: {receivers: [otlp], processors: [transform/clean, stamp], exporters: [debug]}
`)
	got, err := Compose(release(t), build.Manifest, Request{Config: config, Components: []string{"connectors/count"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(got.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0",
		"github.com/open-telemetry/opentelemetry-collector-contrib/processor/transformprocessor v0.162.0",
		"go.opentelemetry.io/collector/exporter/debugexporter v0.162.0",
		"github.com/open-telemetry/opentelemetry-collector-contrib/connector/countconnector v0.162.0",
		"github.com/open-telemetry/opentelemetry-collector-contrib/extension/healthcheckextension v0.162.0",
	}
	if !slices.Equal(m.Modules(), want) {
		t.Fatalf("modules = %q", m.Modules())
	}
	if !slices.Equal(got.Unknown, []string{"processors/stamp"}) || len(got.Warnings) != 0 {
		t.Fatalf("unknown %q, warnings %q", got.Unknown, got.Warnings)
	}
}

func TestComposeRefusesUnclearRequests(t *testing.T) {
	cat := release(t)
	for name, req := range map[string]Request{
		"nothing":         {},
		"preset and more": {Preset: PresetAll, Components: []string{"receivers/otlp"}},
		"unknown preset":  {Preset: "contrib"},
		"bad component":   {Components: []string{"otlp"}},
		"bad kind":        {Components: []string{"pipelines/otlp"}},
	} {
		if _, err := Compose(cat, build.Manifest, req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	got, err := Compose(cat, build.Manifest, Request{Components: []string{"extensions/asapclient"}})
	if err != nil || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "panics at start") {
		t.Fatalf("unsupported pick: %v, %+v", err, got)
	}
}

// TestDeprecatedTypesSelectTheirComponent derives a manifest from a
// configuration that names the OTLP exporters by their old types, which
// the 0.160 to 0.162 releases still accept.
func TestDeprecatedTypesSelectTheirComponent(t *testing.T) {
	config := []byte(`
exporters:
  otlp/backend:
  otlphttp:
  otlp_grpc/other:
`)
	got, err := Compose(release(t), build.Manifest, Request{Config: config})
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(got.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"go.opentelemetry.io/collector/exporter/otlpexporter v0.162.0",
		"go.opentelemetry.io/collector/exporter/otlphttpexporter v0.162.0",
	}
	if !slices.Equal(m.Modules(), want) || len(got.Unknown) != 0 {
		t.Fatalf("modules %q, unknown %q", m.Modules(), got.Unknown)
	}
	if !slices.Equal(got.Warnings, []string{
		"exporters/otlp is the deprecated name of exporters/otlp_grpc; the manifest includes exporters/otlp_grpc",
		"exporters/otlphttp is the deprecated name of exporters/otlp_http; the manifest includes exporters/otlp_http",
	}) {
		t.Fatalf("warnings %q", got.Warnings)
	}
}
