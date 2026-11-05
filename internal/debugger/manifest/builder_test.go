package manifest

import (
	"slices"
	"strings"
	"testing"
)

func TestParseTakesTheReleaseFromCoreComponents(t *testing.T) {
	m, err := Parse([]byte(`
dist:
  name: otelcol-contrib
receivers:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/receiver/filelogreceiver v0.162.0
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
providers:
  - gomod: go.opentelemetry.io/collector/confmap/provider/envprovider v1.68.0
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.CollectorVersion(); got != "0.162.0" {
		t.Fatalf("version = %q", got)
	}
	for name, raw := range map[string]string{
		"none":    "receivers:\n  - gomod: github.com/acme/r v1.0.0\n",
		"several": "receivers:\n  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0\nexporters:\n  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.161.0\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "otelcol_version") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestWarningsNameFieldsOnlyOCBUses(t *testing.T) {
	got := Warnings([]byte(`
dist:
  module: github.com/acme/otelcol
  name: acme
  otelcol_version: 0.162.0
  output_path: ./out
  build_tags: grpcnotrace
  debug_compilation: true
conf_resolver:
  default_uri_scheme: env
receivers: []
`))
	want := []string{
		"dist.module only steers OCB's own build and is ignored",
		"dist.output_path only steers OCB's own build and is ignored",
		"dist.build_tags only steers OCB's own build and is ignored",
		"dist.debug_compilation only steers OCB's own build and is ignored",
		"conf_resolver only steers OCB's own build and is ignored",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("warnings = %q", got)
	}
}

func TestParseReplacesTellsLocalDirectoriesApart(t *testing.T) {
	m := &Manifest{Replaces: []string{
		"github.com/hamba/avro/v2 => github.com/iskorotkov/avro/v2 v2.33.0",
		"github.com/acme/lib v1.0.0 => ../lib",
	}}
	got, err := m.ParseReplaces()
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Local() || got[0].NewVersion != "v2.33.0" || !got[1].Local() || got[1].OldVersion != "v1.0.0" || got[1].New != "../lib" {
		t.Fatalf("replaces = %+v", got)
	}
	if _, err := (&Manifest{Replaces: []string{"no arrow"}}).ParseReplaces(); err == nil {
		t.Fatal("a replace without => was accepted")
	}
}

func TestUpstreamModulesAreCoreAndContrib(t *testing.T) {
	for mod, want := range map[string]bool{
		"go.opentelemetry.io/collector/pdata":                                             true,
		"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl":              true,
		"go.opentelemetry.io/collector-foo/x":                                             false,
		"go.opentelemetry.io/otel":                                                        false,
		"github.com/open-telemetry/opentelemetry-collector-contrib-other/processor/thing": false,
	} {
		if Upstream(mod) != want {
			t.Errorf("Upstream(%q) = %v", mod, !want)
		}
	}
}
