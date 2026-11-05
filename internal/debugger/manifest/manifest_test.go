package manifest

import (
	"strings"
	"testing"
)

func TestParseReadsOCBManifests(t *testing.T) {
	m, err := Parse([]byte(`
dist:
  name: acme
  otelcol_version: 0.162.0
  output_path: ./out
receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0
processors:
  - gomod: github.com/acme/collector v1.2.3
    import: github.com/acme/collector/processor/scrubprocessor
    path: ../acme
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.162.0
replaces:
  - github.com/acme/lib => ../lib
future_builder_field: kept
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Dist.OtelcolVersion != "0.162.0" || m.Dist.OutputPath != "./out" {
		t.Fatalf("dist = %+v", m.Dist)
	}
	p := m.Processors[0]
	if p.ModulePath() != "github.com/acme/collector" || p.Version() != "v1.2.3" ||
		p.ImportPath() != "github.com/acme/collector/processor/scrubprocessor" || p.Path != "../acme" {
		t.Fatalf("processor = %+v", p)
	}
	want := []string{
		"go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0",
		"github.com/acme/collector v1.2.3",
		"go.opentelemetry.io/collector/exporter/debugexporter v0.162.0",
	}
	if got := m.Modules(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("modules = %v", got)
	}
}

func TestParseRejectsIncompleteEntries(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no version": {"receivers:\n  - gomod: github.com/acme/r\n", "receivers[0]"},
		"no gomod":   {"exporters:\n  - import: github.com/acme/e\n", "exporters[0]"},
		"bad name":   {"processors:\n  - gomod: github.com/acme/p v1.0.0\n    name: not-valid\n", "not a Go identifier"},
		"telemetry":  {"telemetry:\n  import: github.com/acme/t\n", "telemetry"},
		"not yaml":   {"receivers: [", "manifest:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPackageNamesFollowOCB(t *testing.T) {
	for _, tc := range []struct {
		mod  Module
		want string
	}{
		{Module{GoMod: "go.opentelemetry.io/collector/receiver/otlpreceiver v0.162.0"}, "otlpreceiver"},
		{Module{GoMod: "github.com/acme/thing/v2 v2.0.0"}, "thing"},
		{Module{GoMod: "github.com/acme/my-processor v1.0.0"}, "myprocessor"},
		{Module{GoMod: "github.com/acme/x v1.0.0", Name: "custom"}, "custom"},
		{Module{GoMod: "github.com/acme/x v1.0.0", Import: "github.com/acme/x/proc/fooprocessor"}, "fooprocessor"},
	} {
		if got := tc.mod.packageName(); got != tc.want {
			t.Errorf("%+v: name %q, want %q", tc.mod, got, tc.want)
		}
	}
}
