package release

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestVersionsMatchOCBManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "build", "linea-runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Dist struct {
			OtelcolVersion string `yaml:"otelcol_version"`
		} `yaml:"dist"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Dist.OtelcolVersion != CollectorVersion {
		t.Fatalf("manifest dist %+v, want otelcol_version %s", m.Dist, CollectorVersion)
	}
}

func TestProductVersionIsTheBuildsVersion(t *testing.T) {
	for set, want := range map[string]string{"v0.2.0": "0.2.0", "0.2.0-rc.1": "0.2.0-rc.1", "": DevVersion} {
		if got := productVersion(set); got != want {
			t.Errorf("productVersion(%q) = %q, want %q", set, got, want)
		}
	}
}
