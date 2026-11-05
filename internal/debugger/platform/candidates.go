package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/statementedit"
)

// CreateCandidate validates and describes a proposal. Saving creates a new
// configuration artifact; it never changes a base head or a host file.
func (s *Service) CreateCandidate(ctx context.Context, caller Caller, req CandidateRequest) (_ *Candidate, err error) {
	defer s.audit(&caller, "CreateCandidate", &err)()
	if err := s.begin(ctx, caller, authz.ArtifactWrite); err != nil {
		return nil, err
	}
	var repository artifact.Repository
	if req.Save {
		var err error
		repository, err = s.artifactStore(ctx, caller, authz.ArtifactWrite)
		if err != nil {
			return nil, err
		}
	}
	if (req.YAML == "") == (req.Statement == nil) {
		return nil, classify(statementedit.ErrInvalid, CodeInvalidRequest)
	}
	baseRef, err := s.resolveSource(ctx, caller, &req.Base, artifact.Configuration, "base")
	if err != nil {
		return nil, err
	}
	base, err := engine.ReadConfigurationSource(req.Base.Path, req.Base.Text, s.policy)
	if err != nil {
		return nil, classify(err, CodeValidationFailed)
	}
	proposed := []byte(req.YAML)
	if req.Statement != nil {
		proposed, err = statementedit.Apply(base, *req.Statement)
		if err != nil {
			var stale *statementedit.StaleError
			if errors.As(err, &stale) {
				e := newError(CodeConflict, err)
				e.CurrentStatement = stale.Current
				return nil, e
			}
			return nil, classify(err, CodeInvalidRequest)
		}
	}
	ins, err := s.InspectConfiguration(ctx, caller, InspectRequest{Config: Source{Text: string(proposed)}, Runtime: req.Runtime})
	if err != nil {
		return nil, err
	}
	patch, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: candidateDiffLines(string(base)), B: candidateDiffLines(string(proposed)),
		FromFile: "base", ToFile: "candidate", Context: 3,
	})
	if err != nil {
		return nil, classify(err, CodeInternal)
	}
	out := &Candidate{Proposed: string(proposed), Patch: patch, Valid: ins.Valid, Diagnostics: ins.Diagnostics, AffectedPipelines: []string{}}
	var before *Inspection
	// The captured root bytes, rather than a second host read, are the base.
	if req.Statement == nil || !ins.Valid {
		before, err = s.InspectConfiguration(ctx, caller, InspectRequest{Config: Source{Text: string(base)}, Runtime: req.Runtime})
		if err != nil {
			return nil, err
		}
	}
	processors := []string{}
	if req.Statement != nil {
		processors = append(processors, req.Statement.Processor)
	} else {
		processors = changedProcessors(base, proposed)
	}
	graphs := []*Inspection{ins}
	if req.Statement != nil {
		if !ins.Valid {
			graphs = []*Inspection{before}
		}
	} else {
		graphs = append(graphs, before)
	}
	for _, inspection := range graphs {
		if inspection == nil || !inspection.Valid || inspection.Topology == nil {
			continue
		}
		for _, id := range processors {
			if component, ok := inspection.Topology.Component("processor/" + id); ok {
				out.AffectedPipelines = append(out.AffectedPipelines, component.Pipelines...)
			}
		}
	}
	slices.Sort(out.AffectedPipelines)
	out.AffectedPipelines = slices.Compact(out.AffectedPipelines)
	if req.Save && out.Valid {
		sum := sha256.Sum256(base)
		origin := &artifact.Origin{Kind: "candidate", BaseDigest: hex.EncodeToString(sum[:])}
		if baseRef != nil {
			origin.BaseRevision = baseRef.RevisionID
		}
		if req.Statement != nil {
			origin.Edit, err = json.Marshal(req.Statement)
			if err != nil {
				return nil, classify(err, CodeInternal)
			}
		}
		label := req.Label
		if label == "" {
			label = "candidate"
		}
		if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
			return nil, err
		}
		out.Saved, err = saveArtifact(ctx, repository, artifact.SaveRequest{WorkspaceID: repository.Info().ID, Project: caller.Project, Kind: artifact.Configuration,
			Label: label, Content: proposed, Actor: caller.Principal.ID, Origin: origin})
		if err != nil {
			return nil, artifactError(err)
		}
	}
	return out, nil
}

func changedProcessors(base, proposed []byte) []string {
	var a, b map[string]any
	decode := func(text []byte, into any) bool {
		d := yaml.NewDecoder(bytes.NewReader(text))
		return d.Decode(into) == nil && d.Decode(new(any)) == io.EOF
	}
	if !decode(base, &a) || !decode(proposed, &b) {
		return nil
	}
	old, _ := a["processors"].(map[string]any)
	next, _ := b["processors"].(map[string]any)
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range next {
		keys[k] = true
	}
	var changed []string
	for k := range keys {
		av, ap := old[k]
		bv, bp := next[k]
		if ap != bp || !reflect.DeepEqual(av, bv) {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}

// difflib.SplitLines adds a synthetic final line. Keep real line counts and
// supply the unified-diff marker as part of the final logical line instead.
func candidateDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.SplitAfter(text, "\n")
	last := len(lines) - 1
	if lines[last] == "" {
		return lines[:last]
	}
	lines[last] += "\n\\ No newline at end of file\n"
	return lines
}
