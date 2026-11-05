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

func catalogOf(t *testing.T, version string) *versions.Catalog {
	t.Helper()
	r := versions.Embedded().Release(version)
	if r == nil {
		t.Fatalf("%s is not in the support table", version)
	}
	return r.Catalog
}

func TestRetargetMovesEveryModuleToTheRelease(t *testing.T) {
	older, err := Retarget(catalogOf(t, "0.162.0"), catalogOf(t, "0.160.0"), build.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(older.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.CollectorVersion() != "0.160.0" || older.From != "0.162.0" || older.To != "0.160.0" {
		t.Fatalf("version %s, from %s, to %s", m.CollectorVersion(), older.From, older.To)
	}
	for _, mod := range slices.Concat(m.Modules(), providerModules(m)) {
		if !strings.HasSuffix(mod, " v0.160.0") && !strings.HasSuffix(mod, " v1.66.0") {
			t.Errorf("%s is not at 0.160", mod)
		}
	}
	if len(older.Missing) != 0 || len(older.Custom) != 0 || len(older.Moved) < len(m.Modules()) {
		t.Fatalf("missing %v, custom %v, moved %d", older.Missing, older.Custom, len(older.Moved))
	}
	if !bytes.Contains(older.Manifest, []byte("# Debug Collector distribution.")) {
		t.Fatal("the comments were not kept")
	}
	back, err := Retarget(catalogOf(t, "0.160.0"), catalogOf(t, "0.162.0"), older.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Manifest, build.Manifest) {
		t.Fatalf("moving back changed the manifest:\n%s", back.Manifest)
	}
}

func providerModules(m *manifest.Manifest) []string {
	var out []string
	for _, p := range m.Providers {
		out = append(out, p.ModulePath()+" "+p.Version())
	}
	return out
}

func TestRetargetNamesMissingAndCustomModules(t *testing.T) {
	raw := []byte(`dist:
  otelcol_version: "0.160.0"
exporters:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/exporter/mezmoexporter v0.160.0
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.160.0
processors:
  - gomod: github.com/example/stampprocessor v0.3.1 # ours
receivers:
  - gomod: github.com/open-telemetry/opentelemetry-collector-contrib/receiver/awss3receiver v0.160.0
replaces:
  - github.com/hamba/avro/v2 => github.com/iskorotkov/avro/v2 v2.33.0
  - github.com/example/lib => ../lib
`)
	out, err := Retarget(catalogOf(t, "0.160.0"), catalogOf(t, "0.162.0"), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Missing, []string{"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/mezmoexporter"}) {
		t.Fatalf("missing %v", out.Missing)
	}
	if !slices.Equal(out.Custom, []string{"github.com/example/stampprocessor"}) {
		t.Fatalf("custom %v", out.Custom)
	}
	text := string(out.Manifest)
	for _, want := range []string{
		`otelcol_version: "0.162.0"`,
		"debugexporter v0.162.0",
		// The receiver's type changed (awss3 to aws_s3); its module did not.
		"awss3receiver v0.162.0",
		"mezmoexporter v0.160.0",
		"stampprocessor v0.3.1 # ours",
		"avro/v2 v2.33.0",
		"github.com/example/lib => ../lib",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the moved manifest lacks %q:\n%s", want, text)
		}
	}
}

func TestRetargetSwapsTheReleasesReplaces(t *testing.T) {
	from, to := *catalogOf(t, "0.160.0"), *catalogOf(t, "0.162.0")
	from.Replaces = []string{"a.example/x => b.example/x v1.0.0", "c.example/y => d.example/y v1.0.0"}
	to.Replaces = []string{"a.example/x => b.example/x v1.1.0", "e.example/z => f.example/z v2.0.0"}
	raw := []byte("dist:\n  otelcol_version: 0.160.0\nreplaces:\n  - a.example/x => b.example/x v1.0.0\n  - c.example/y => d.example/y v1.0.0\n  - mine.example/m => ./m\n")
	out, err := Retarget(&from, &to, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := "dist:\n  otelcol_version: 0.162.0\nreplaces:\n  - a.example/x => b.example/x v1.1.0\n  - mine.example/m => ./m\n  - e.example/z => f.example/z v2.0.0\n"
	if string(out.Manifest) != want {
		t.Fatalf("manifest:\n%s\nwant:\n%s", out.Manifest, want)
	}
	if len(out.Notes) != 3 {
		t.Fatalf("notes %q", out.Notes)
	}
}
