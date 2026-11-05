package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

func sampleSources(t *testing.T) (config, input, scenario string) {
	t.Helper()
	sample := filepath.Join("..", "..", "..", "samples", "logs-cleanup")
	raw, err := os.ReadFile(filepath.Join(sample, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(sample, "input.json"))
	if err != nil {
		t.Fatal(err)
	}
	scenario = "version: 1\nname: by reference\ninputs:\n  - data: '" + strings.ReplaceAll(strings.TrimSpace(string(payload)), "\n", " ") + "'\n"
	return string(raw), string(payload), scenario
}

func saveRevision(t *testing.T, s *Service, kind ArtifactKind, content string) *ArtifactRef {
	t.Helper()
	info, err := s.GetWorkspaceInfo(context.Background(), testCaller)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.SaveArtifact(context.Background(), testCaller, SaveArtifactRequest{WorkspaceID: info.ID, Kind: kind, Label: string(kind), Content: []byte(content)})
	if err != nil {
		t.Fatal(err)
	}
	return &ArtifactRef{ArtifactID: saved.Artifact.ID, RevisionID: saved.Revision.Ref.RevisionID}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// A run by reference copies the revision's bytes at admission and records the
// revision; a new head saved afterwards changes neither the run nor its bundle.
func TestRunsByReferenceCopyTheRevision(t *testing.T) {
	config, input, scenario := sampleSources(t)
	ctx := context.Background()
	data := memory.NewData()
	s := openPlatform(t, data, Options{})
	configRef := saveRevision(t, s, artifact.Configuration, config)
	fixtureRef := saveRevision(t, s, artifact.Fixture, input)
	scenarioRef := saveRevision(t, s, artifact.Scenario, scenario)
	requests := map[InputMode]RunRequest{
		ModeQuick:    {Mode: ModeQuick, Config: Source{Artifact: configRef}, Quick: &QuickInput{Fixture: Source{Artifact: fixtureRef, Label: "input.json"}}},
		ModeScenario: {Mode: ModeScenario, Config: Source{Artifact: configRef, Label: "config.yaml"}, Scenario: &ScenarioInput{Scenario: Source{Artifact: scenarioRef}}},
	}
	runs := map[InputMode]string{}
	bundles := map[InputMode]string{}
	for mode, req := range requests {
		run, err := s.SubmitRun(ctx, testCaller, req)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		rep, err := s.GetRunReport(ctx, testCaller, run.ID, true)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if rep.Provenance.Config.SHA256 != digest(config) {
			t.Fatalf("%s config provenance = %+v", mode, rep.Provenance.Config)
		}
		got, err := s.GetRun(ctx, testCaller, run.ID)
		if err != nil || got.Bundle == nil || got.Bundle.Sources == nil || *got.Bundle.Sources.Config != *configRef {
			t.Fatalf("%s bundle = %+v, %v", mode, got.Bundle, err)
		}
		switch mode {
		case ModeQuick:
			if p := rep.Provenance; p.Config.Path != "" || p.Fixture == nil || p.Fixture.Path != "input.json" || p.Fixture.SHA256 != digest(input) {
				t.Fatalf("quick provenance = %+v %+v", p.Config, p.Fixture)
			}
			if src := got.Bundle.Sources; src.Fixture == nil || *src.Fixture != *fixtureRef || src.Scenario != nil {
				t.Fatalf("quick sources = %+v", src)
			}
		case ModeScenario:
			if p := rep.Provenance; p.Config.Path != "config.yaml" || p.Scenario == nil || p.Scenario.SHA256 != digest(scenario) {
				t.Fatalf("scenario provenance = %+v %+v", p.Config, p.Scenario)
			}
			if src := got.Bundle.Sources; src.Scenario == nil || *src.Scenario != *scenarioRef || src.Fixture != nil {
				t.Fatalf("scenario sources = %+v", src)
			}
		}
		runs[mode], bundles[mode] = run.ID, got.Bundle.Digest
	}

	// New heads leave the runs' captured sources alone, including after a restart.
	m, err := s.GetArtifact(ctx, testCaller, configRef.ArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveArtifact(ctx, testCaller, SaveArtifactRequest{WorkspaceID: m.WorkspaceID, ArtifactID: m.ID, ExpectedGeneration: m.Generation, ExpectedHead: m.Head,
		Kind: artifact.Configuration, Label: "changed", Content: []byte("receivers: {}\n")}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openPlatform(t, data, Options{})
	for mode, id := range runs {
		got, err := s.GetRun(ctx, testCaller, id)
		if err != nil || got.Bundle.Digest != bundles[mode] {
			t.Fatalf("%s after a new head: %+v, %v", mode, got.Bundle, err)
		}
		raw, err := s.GetRunBundle(ctx, testCaller, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.ReadBundleParts(raw); err != nil {
			t.Fatal(err)
		}
		rep, err := s.GetRunReport(ctx, testCaller, id, false)
		if err != nil || rep.Provenance.Config.SHA256 != digest(config) {
			t.Fatalf("%s report after a new head: %v", mode, err)
		}
	}
}

func TestArtifactReferencesAreChecked(t *testing.T) {
	config, input, _ := sampleSources(t)
	ctx := context.Background()
	s := openPlatform(t, memory.NewData(), Options{})
	configRef := saveRevision(t, s, artifact.Configuration, config)
	fixtureRef := saveRevision(t, s, artifact.Fixture, input)
	quick := func(cfg Source) RunRequest {
		return RunRequest{Mode: ModeQuick, Config: cfg, Quick: &QuickInput{Fixture: Source{Text: input}}}
	}
	for _, tc := range []struct {
		name string
		req  RunRequest
		code Code
	}{
		{"unknown artifact", quick(Source{Artifact: &ArtifactRef{ArtifactID: "missing", RevisionID: configRef.RevisionID}}), CodeNotFound},
		{"unknown revision", quick(Source{Artifact: &ArtifactRef{ArtifactID: configRef.ArtifactID, RevisionID: "missing"}}), CodeNotFound},
		{"wrong kind", quick(Source{Artifact: fixtureRef}), CodeInvalidRequest},
		{"no revision", quick(Source{Artifact: &ArtifactRef{ArtifactID: configRef.ArtifactID}}), CodeInvalidRequest},
		{"text and artifact", quick(Source{Text: config, Artifact: configRef}), CodeInvalidRequest},
		{"label beside a path", quick(Source{Path: "config.yaml", Label: "other"}), CodeInvalidRequest},
	} {
		_, err := s.SubmitRun(ctx, testCaller, tc.req)
		wantCode(t, err, tc.code)
		_, err = s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: tc.req.Config})
		wantCode(t, err, tc.code)
	}

	ephemeral := New(Options{})
	defer ephemeral.Close()
	_, err := ephemeral.SubmitRun(ctx, testCaller, quick(Source{Artifact: configRef}))
	wantCode(t, err, CodeUnavailable)
	_, err = ephemeral.CreateCandidate(ctx, testCaller, CandidateRequest{Base: Source{Artifact: configRef}, YAML: config})
	wantCode(t, err, CodeUnavailable)
}

func TestReferencedBasesAndLabelsAreRecorded(t *testing.T) {
	config, _, scenario := sampleSources(t)
	ctx := context.Background()
	s := openPlatform(t, memory.NewData(), Options{})
	configRef := saveRevision(t, s, artifact.Configuration, config)

	ins, err := s.InspectConfiguration(ctx, testCaller, InspectRequest{Config: Source{Artifact: configRef, Label: "saved config"}})
	if err != nil || !ins.Valid || ins.Config.Path != "saved config" || ins.Config.SHA256 != digest(config) {
		t.Fatalf("inspection = %+v, %v", ins, err)
	}

	candidate, err := s.CreateCandidate(ctx, testCaller, CandidateRequest{Base: Source{Artifact: configRef}, YAML: config + "\n", Save: true})
	if err != nil || candidate.Saved == nil {
		t.Fatalf("candidate = %+v, %v", candidate, err)
	}
	origin := candidate.Saved.Revision.Origin
	if origin == nil || origin.BaseRevision != configRef.RevisionID || origin.BaseDigest != digest(config) {
		t.Fatalf("origin = %+v", origin)
	}

	scenarioRef := saveRevision(t, s, artifact.Scenario, scenario)
	suite, err := s.SubmitSuite(ctx, testCaller, SubmitSuiteRequest{Config: Source{Text: config, Label: "repo/config.yaml"},
		Members: []ScenarioInput{{Scenario: Source{Artifact: scenarioRef}}, {Scenario: Source{Text: scenario, Label: "tests/inline.yaml"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if suite.Config != "repo/config.yaml" || suite.Members[0].Label != "inline scenario 1" || suite.Members[1].Label != "tests/inline.yaml" {
		t.Fatalf("suite labels = %q %+v", suite.Config, suite.Members)
	}
	waitSuite(t, s, suite)
	run, err := s.GetRun(ctx, testCaller, suite.Members[0].RunID)
	if err != nil || run.Bundle.Sources == nil || run.Bundle.Sources.Config != nil || *run.Bundle.Sources.Scenario != *scenarioRef {
		t.Fatalf("member bundle = %+v, %v", run.Bundle, err)
	}
	rep, err := s.GetRunReport(ctx, testCaller, suite.Members[1].RunID, true)
	if err != nil || rep.Provenance.Config.Path != "repo/config.yaml" || rep.Provenance.Scenario.Path != "tests/inline.yaml" {
		t.Fatalf("member provenance = %+v, %v", rep.Provenance, err)
	}
}
