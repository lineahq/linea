package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lineahq/linea/internal/debugger/hostfs"
)

func TestReadScenarioSourceUsesPolicyAndKeepsDirectory(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	const source = "version: 1\r\ninputs: [{file: never-read.json}]\r\n"
	for _, directory := range []string{dir, outside} {
		if err := os.WriteFile(filepath.Join(directory, "scenario.yaml"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	policy := Policy{Paths: hostfs.Policy{Roots: []string{dir}}}
	for _, tc := range []struct {
		name, path, text, dir, wantDir string
	}{
		{"file", filepath.Join(dir, "scenario.yaml"), "", "ignored", dir},
		{"inline", "", source, "inline directory", "inline directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, gotDir, err := ReadScenarioSource(ctx, tc.path, tc.text, tc.dir, policy)
			if err != nil || string(raw) != source || gotDir != tc.wantDir {
				t.Fatalf("source = %q, dir = %q, %v", raw, gotDir, err)
			}
		})
	}
	for _, path := range []string{filepath.Join(outside, "scenario.yaml"), "scenario.yaml"} {
		if _, _, err := ReadScenarioSource(ctx, path, "", "", policy); !errors.Is(err, hostfs.ErrOutsideRoots) {
			t.Fatalf("refused path %s: %v", path, err)
		}
	}
	for _, src := range [][2]string{{"", ""}, {"never-read", source}} {
		if _, _, err := ReadScenarioSource(ctx, src[0], src[1], "", policy); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("invalid source: %v", err)
		}
	}
	if _, _, err := ReadScenarioSource(ctx, filepath.Join(dir, "scenario.yaml"), "", "", Policy{}); !errors.Is(err, hostfs.ErrNoRoots) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := ReadScenarioSource(cancelled, filepath.Join(dir, "scenario.yaml"), "", "", policy); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The source reader keeps admission's limit error, without allocating a
	// large file's content or reading any of the scenario's dependencies.
	large := filepath.Join(dir, "large.yaml")
	f, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxInputBytes)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadScenarioSource(ctx, large, "", "", policy); !errors.Is(err, ErrInputLimit) {
		t.Fatalf("large scenario: %v", err)
	}
}
