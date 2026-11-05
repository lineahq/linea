package registry

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/release"
	"go.yaml.in/yaml/v3"
)

type ocbManifest struct {
	Receivers []struct {
		GoMod string `yaml:"gomod"`
	} `yaml:"receivers"`
	Processors []struct {
		GoMod string `yaml:"gomod"`
	} `yaml:"processors"`
	Exporters []struct {
		GoMod string `yaml:"gomod"`
	} `yaml:"exporters"`
	Connectors []struct {
		GoMod string `yaml:"gomod"`
	} `yaml:"connectors"`
	Extensions []struct {
		GoMod string `yaml:"gomod"`
	} `yaml:"extensions"`
}

func TestModulesMatchOCBManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "build", "linea-runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m ocbManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, group := range [][]struct {
		GoMod string `yaml:"gomod"`
	}{m.Receivers, m.Processors, m.Exporters, m.Connectors, m.Extensions} {
		for _, c := range group {
			want = append(want, c.GoMod)
		}
	}
	got := slices.Clone(release.Modules)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("registry modules diverge from OCB manifest\n got: %v\nwant: %v", got, want)
	}
}

func TestModulesArePinnedInBuild(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("build info unavailable")
	}
	linked := map[string]string{}
	for _, dep := range info.Deps {
		linked[dep.Path] = dep.Version
	}
	for _, mod := range release.Modules {
		path, version, _ := strings.Cut(mod, " ")
		if linked[path] != version {
			t.Errorf("%s: linked version %q, manifest pins %q", path, linked[path], version)
		}
	}
}

func TestFactoriesRegisterOTLPExporterAlias(t *testing.T) {
	f, err := Factories()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"otlp", "otlp_grpc"} {
		if _, ok := f.Exporters[MustType(name)]; !ok {
			t.Errorf("exporter type %q not registered", name)
		}
	}
}
