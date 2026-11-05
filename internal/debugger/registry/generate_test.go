package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lineahq/linea/internal/debugger/features"
	"github.com/lineahq/linea/internal/debugger/manifest"
)

func TestComponentsAreGeneratedFromTheManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "build", "linea-runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	const source = "build/linea-runtime.yaml"
	registry, err := manifest.GenerateRegistry(m, manifest.RegistryOptions{
		Package: "registry", Source: source, Overrides: manifest.RegistryOverrides,
	})
	if err != nil {
		t.Fatal(err)
	}
	variant, err := manifest.GenerateOTTLVariant(m.CollectorVersion(), source)
	if err != nil {
		t.Fatal(err)
	}
	featureSet, err := manifest.GenerateFeatureSet(m, features.Packages(), source)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := manifest.GenerateRelease(m, source)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string][]byte{
		"components.go": registry,
		filepath.Join("..", "compat", "ottl", "variant.go"): variant,
		filepath.Join("..", "featureset", "featureset.go"):  featureSet,
		filepath.Join("..", "release", "collector.go"):      rel,
	} {
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is stale; run go generate ./internal/debugger/registry", file)
		}
	}
}
