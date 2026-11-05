package mcp

import "github.com/lineahq/linea/internal/debugger/platform"

// A source contains exactly one nonempty inline text or saved revision. The
// server reads no host file, so a client sends a file's content as text. A
// revision's bytes are copied into the run. A label, such as the file's
// path on the client, names inline text in the run.
type source struct {
	Text     *string      `json:"text,omitempty" jsonschema:"Inline source content, such as a file's text; supply exactly one of text or artifact."`
	Artifact *artifactRef `json:"artifact,omitempty" jsonschema:"A saved revision in the server's workspace; supply exactly one of text or artifact."`
	Label    string       `json:"label,omitempty" jsonschema:"Optional name of inline text, such as its file's path on the client; the server never reads it."`
}

type artifactRef struct {
	ArtifactID string `json:"artifact_id" jsonschema:"Saved artifact ID."`
	RevisionID string `json:"revision_id" jsonschema:"Revision ID of that artifact; runs pin this exact revision."`
}

func (s source) platform() (platform.Source, error) {
	set := 0
	for _, given := range []bool{s.Text != nil, s.Artifact != nil} {
		if given {
			set++
		}
	}
	if set != 1 {
		return platform.Source{}, invalidRequest("a source requires exactly one of text or artifact")
	}
	if a := s.Artifact; a != nil {
		if a.ArtifactID == "" || a.RevisionID == "" {
			return platform.Source{}, invalidRequest("an artifact source requires artifact_id and revision_id")
		}
		return platform.Source{Artifact: &platform.ArtifactRef{ArtifactID: a.ArtifactID, RevisionID: a.RevisionID}}, nil
	}
	if s.Text != nil && *s.Text != "" {
		return platform.Source{Text: *s.Text, Label: s.Label}, nil
	}
	return platform.Source{}, invalidRequest("source text must not be empty")
}

func scenarioSource(s *source) (*platform.ScenarioInput, error) {
	if s == nil {
		return nil, nil
	}
	src, err := s.platform()
	if err != nil {
		return nil, err
	}
	// No Dir is invented for inline text: its input files must be inline.
	return &platform.ScenarioInput{Scenario: src}, nil
}
