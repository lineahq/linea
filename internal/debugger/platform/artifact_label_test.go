package platform

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func TestYAMLLabelNamesConfigurationsAndScenariosAsYAMLFiles(t *testing.T) {
	for _, tc := range []struct {
		kind        artifact.Kind
		label, want string
	}{
		{artifact.Configuration, "config.yaml", "config.yaml"},
		{artifact.Configuration, "otel.yml", "otel.yml"},
		{artifact.Scenario, "Otel.YAML", "Otel.YAML"},
		{artifact.Configuration, "config.bin", "config.bin.yaml"},
		{artifact.Scenario, "config.yaml.exe", "config.yaml.exe.yaml"},
		{artifact.Scenario, "  drop debug logs ", "drop debug logs.yaml"},
		{artifact.Scenario, "../escape", "../escape.yaml"},
		{artifact.Configuration, "", ""},
		{artifact.Fixture, "input.json", "input.json"},
		{artifact.Setup, "setup", "setup"},
	} {
		if got := yamlLabel(tc.kind, tc.label); got != tc.want {
			t.Errorf("yamlLabel(%s, %q) = %q, want %q", tc.kind, tc.label, got, tc.want)
		}
	}
	for _, long := range []string{strings.Repeat("x", 2000), strings.Repeat("é", 700)} {
		got := yamlLabel(artifact.Configuration, long)
		if len(got) > maxArtifactLabel || !strings.HasSuffix(got, ".yaml") || !utf8.ValidString(got) {
			t.Errorf("long label: %d bytes, valid %v, %q…", len(got), utf8.ValidString(got), got[len(got)-8:])
		}
	}
}

func TestSavedWorkAlwaysGetsAYAMLName(t *testing.T) {
	s, err := openMemory(memory.NewData(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := context.Background()
	info, err := s.GetWorkspaceInfo(ctx, testCaller)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Configuration, Label: "config.bin", Content: []byte("receivers: {}\n")})
	if err != nil || saved.Artifact.Label != "config.bin.yaml" {
		t.Fatalf("new artifact = %+v, %v", saved, err)
	}
	m := saved.Artifact
	revised, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, ArtifactID: m.ID, ExpectedGeneration: m.Generation, ExpectedHead: m.Head,
		Kind: artifact.Configuration, Label: "renamed", Content: []byte("receivers: {}\n")})
	if err != nil || revised.Artifact.Label != "renamed.yaml" {
		t.Fatalf("revision = %+v, %v", revised, err)
	}
	fixture, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: artifact.Fixture, Label: "input.json", Content: []byte("{}")})
	if err != nil || fixture.Artifact.Label != "input.json" {
		t.Fatalf("fixture = %+v, %v", fixture, err)
	}
}
