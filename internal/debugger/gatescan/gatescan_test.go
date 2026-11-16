package gatescan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestGenerateFindsEachGatesOwners scans a listed worker: a gate a
// component registers through another package's constant is owned by that
// component, one a Collector package registers by the Collector, and the go
// commands run for linux/amd64 without the workspace.
func TestGenerateFindsEachGatesOwners(t *testing.T) {
	root := t.TempDir()
	write := func(dir, name, src string) string {
		t.Helper()
		d := filepath.Join(root, dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return d
	}
	const mod = "example.com/worker"
	pkgs := []listed{
		{ImportPath: mod + "/cmd/linea-worker", Imports: []string{mod + "/internal/debugger/registry"}},
		{
			ImportPath: mod + "/internal/debugger/registry",
			Dir: write("registry", "components.go", `package registry

func componentTypes() []entry { return []entry{{"example.com/comp", nil}} }
`),
			Imports: []string{"example.com/comp", "example.com/otelcol"},
		},
		{
			ImportPath: "example.com/comp",
			Dir: write("comp", "gate.go", `package comp

import (
	"go.opentelemetry.io/collector/featuregate"

	"example.com/names"
)

var g = featuregate.GlobalRegistry().MustRegister(names.Prefix+".gate", featuregate.StageAlpha)
`),
			GoFiles: []string{"gate.go"},
			Imports: []string{featuregatePkg, "example.com/names"},
		},
		{ImportPath: "example.com/names", Dir: write("names", "names.go", "package names\n\nconst Prefix = \"comp\"\n"), GoFiles: []string{"names.go"}},
		{
			ImportPath: "example.com/otelcol",
			Dir: write("otelcol", "gate.go", `package otelcol

import "go.opentelemetry.io/collector/featuregate"

func init() { featuregate.GlobalRegistry().Register("collector.gate", featuregate.StageBeta) }
`),
			GoFiles: []string{"gate.go"},
			Imports: []string{featuregatePkg},
		},
		{ImportPath: featuregatePkg},
	}
	var calls []string
	list := func(env []string, args ...string) ([]byte, error) {
		if !slices.Contains(env, "GOOS=linux") || !slices.Contains(env, "GOARCH=amd64") {
			t.Errorf("go %v runs without GOOS=linux GOARCH=amd64: %v", args, env)
		}
		calls = append(calls, strings.Join(args, " "))
		if args[1] == "-m" {
			if !slices.Contains(env, "GOWORK=off") {
				t.Errorf("go list -m runs in the workspace: %v", env)
			}
			return []byte(mod + "\n"), nil
		}
		var out []byte
		for _, p := range pkgs {
			b, _ := json.Marshal(p)
			out = append(out, b...)
		}
		return out, nil
	}
	src, err := Generate(list, "the test")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"from the test and the worker's source",
		`"collector.gate": {collector: true},`,
		`"comp.gate":      {packages: []string{"example.com/comp"}},`,
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("gateowners.go lacks %q:\n%s", want, src)
		}
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "list ") {
			t.Errorf("the scan ran go %s; it may only list packages", c)
		}
	}
}
