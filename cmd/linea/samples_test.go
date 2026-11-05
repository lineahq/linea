package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/samples"
)

// TestSamplesGetWritesARunnableSample writes a sample out and then tests it
// from disk with the command the output prints, as a new user would.
func TestSamplesGetWritesARunnableSample(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mine")
	code, out, stderr := runCLI(t, "samples", "get", "logs-cleanup", "--dir", dir)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	config, scenario := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "scenario.yaml")
	if !strings.Contains(out, "linea test --config "+config+" --scenario "+scenario) {
		t.Fatalf("output does not say how to run it:\n%s", out)
	}
	for _, name := range samples.FileNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, stderr := runCLI(t, "test", "--config", config, "--scenario", scenario); code != exitOK {
		t.Fatalf("the written sample does not pass: exit %d: %s", code, stderr)
	}
}

func TestSamplesGetRefusesBadRequests(t *testing.T) {
	existing := t.TempDir()
	if err := os.WriteFile(filepath.Join(existing, "scenario.yaml"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no ID", []string{"samples", "get"}, "name the sample"},
		{"unknown ID", []string{"samples", "get", "nope", "--dir", t.TempDir()}, `no sample "nope"`},
		{"existing file", []string{"samples", "get", "logs-cleanup", "--dir", existing}, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			if code != exitError || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want %q", code, stderr, tc.want)
			}
		})
	}
	if raw, _ := os.ReadFile(filepath.Join(existing, "scenario.yaml")); string(raw) != "mine" {
		t.Fatalf("an existing file was overwritten: %q", raw)
	}
}

func TestSamplesGetSaysWhenTheSampleFailsAsShipped(t *testing.T) {
	code, out, _ := runCLI(t, "samples", "get", "broken-routing", "--dir", t.TempDir())
	if code != exitOK || !strings.Contains(out, "fail as shipped") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestTerminalIsFalseForBuffersAndFiles(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if terminal(&bytes.Buffer{}) || terminal(f) {
		t.Fatal("a buffer or a file is not a terminal")
	}
}
