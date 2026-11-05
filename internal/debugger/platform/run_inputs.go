package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
)

// RunInputs are the sources a run was admitted with, read back from its
// captured bundle so a client can open them as drafts. They never carry
// provider-captured file contents or environment values: the configuration is
// its root text with references unexpanded, and captured files and required
// environment values are listed by name only.
type RunInputs struct {
	RunID string
	Mode  InputMode
	// Config is the root configuration; Fixture and Setup belong to quick runs,
	// Scenario to scenario runs. Assertions are a quick run's own assertions.
	Config                               RunSource
	Fixture, Scenario, Setup, Assertions *RunSource
	Receiver, ClientAddress              string
	// Files names the captured input files other than the root sources,
	// relative to the scenario's directory where they are under it.
	Files []string
	// RequiredEnvironment names the environment values the configuration read.
	RequiredEnvironment []string
}

// RunSource is one root source of a run: its submitted path or label, the
// saved revision it was submitted by, and its captured bytes.
type RunSource struct {
	Label    string
	Artifact *ArtifactRef
	SHA256   string
	Text     string
}

// GetRunInputs returns a run's admitted sources in any state. It reads only
// the run's captured bundle, never the host.
func (s *Service) GetRunInputs(ctx context.Context, caller Caller, runID string) (_ *RunInputs, err error) {
	defer s.audit(&caller, "GetRunInputs", &err)()
	if err := s.begin(ctx, caller, authz.RunRead); err != nil {
		return nil, err
	}
	if err := s.runIn(ctx, caller, authz.RunRead, runID); err != nil {
		return nil, err
	}
	raw, err := s.getRunBundle(ctx, runID)
	if err != nil {
		return nil, err
	}
	parts, err := engine.ReadBundleParts(raw)
	if err != nil {
		e := newError(CodeStorageError, err)
		e.Message = "cannot read captured execution bundle"
		return nil, e
	}
	refs := parts.Sources
	if refs == nil {
		refs = &SourceRefs{}
	}
	in := &RunInputs{RunID: runID, Mode: InputMode(parts.Mode), Config: runSource(parts.ConfigName, refs.Config, parts.Config),
		Receiver: parts.Receiver, ClientAddress: parts.ClientAddress, Files: []string{}, RequiredEnvironment: parts.HostEnvironment}
	if in.RequiredEnvironment == nil {
		in.RequiredEnvironment = []string{}
	}
	if in.Mode == ModeQuick {
		fixture := runSource(parts.FixtureName, refs.Fixture, parts.Fixture)
		in.Fixture = &fixture
	} else {
		scenario := runSource(parts.ScenarioName, refs.Scenario, parts.Scenario)
		in.Scenario = &scenario
	}
	if len(parts.Setup) != 0 {
		setup := runSource("", nil, parts.Setup)
		in.Setup = &setup
	}
	if len(parts.Assertions) != 0 {
		assertions := runSource("", nil, parts.Assertions)
		in.Assertions = &assertions
	}
	for name := range parts.Files {
		if name == parts.FixtureFile || name == parts.ScenarioFile {
			continue
		}
		in.Files = append(in.Files, inputFileName(parts.ScenarioDir, name))
	}
	slices.Sort(in.Files)
	return in, nil
}

func runSource(label string, ref *ArtifactRef, raw []byte) RunSource {
	sum := sha256.Sum256(raw)
	return RunSource{Label: label, Artifact: ref, SHA256: hex.EncodeToString(sum[:]), Text: string(raw)}
}

// inputFileName names a captured file relative to the scenario's directory, or
// by its base name outside it, so host directories are not repeated.
func inputFileName(dir, name string) string {
	if dir != "" {
		if rel, err := filepath.Rel(dir, name); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(name)
}
