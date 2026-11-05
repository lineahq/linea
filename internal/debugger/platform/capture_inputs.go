package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/provenance"
	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// CaptureInputSelection picks captured records of one stream as a run input.
type CaptureInputSelection = provenance.CaptureInputSelection

// CaptureDeriveRequest derives a fixture for a configuration from
// captured records, optionally saving it.
type CaptureDeriveRequest struct {
	Selection  CaptureInputSelection `json:"selection"`
	ConfigYAML string                `json:"config_yaml"`
	SetupYAML  string                `json:"setup_yaml,omitempty"`
	Save       bool                  `json:"save,omitempty"`
	Label      string                `json:"label,omitempty"`
}

// CaptureDerivation records where a derived input came from.
type CaptureDerivation = provenance.CaptureDerivation

// CaptureSource is the identity of a captured Collector at admission.
type CaptureSource = provenance.CaptureSource

// CaptureFixture is a fixture derived from captured records.
type CaptureFixture struct {
	FixtureJSON string            `json:"fixture_json"`
	SetupYAML   string            `json:"setup_yaml"`
	Payload     json.RawMessage   `json:"payload"`
	Receiver    string            `json:"receiver"`
	Signal      string            `json:"signal"`
	Derivation  CaptureDerivation `json:"derivation"`
	Limitations []string          `json:"limitations"`
	Saved       *SavedArtifact    `json:"saved,omitempty"`
}

// CaptureScenarioRequest derives a scenario for a configuration from
// captured records, optionally saving it.
type CaptureScenarioRequest struct {
	ConfigYAML     string                  `json:"config_yaml"`
	Inputs         []CaptureInputSelection `json:"inputs"`
	Name           string                  `json:"name,omitempty"`
	SetupYAML      string                  `json:"setup_yaml,omitempty"`
	AssertionsYAML string                  `json:"assertions_yaml,omitempty"`
	Save           bool                    `json:"save,omitempty"`
	Label          string                  `json:"label,omitempty"`
}

// CaptureScenario is a scenario derived from captured records.
type CaptureScenario struct {
	YAML        string              `json:"yaml"`
	Export      string              `json:"export"`
	Digest      string              `json:"digest"`
	Derivations []CaptureDerivation `json:"derivations"`
	Limitations []string            `json:"limitations"`
	Saved       *SavedArtifact      `json:"saved,omitempty"`
}

// CaptureExportRequest exports either a fixture or a scenario.
type CaptureExportRequest struct {
	Fixture  *CaptureDeriveRequest   `json:"fixture,omitempty"`
	Scenario *CaptureScenarioRequest `json:"scenario,omitempty"`
}

// CaptureExport is a derived fixture or scenario as a reviewable file.
type CaptureExport struct {
	Format      string   `json:"format"`
	Content     string   `json:"content"`
	SetupYAML   string   `json:"setup_yaml,omitempty"`
	Digest      string   `json:"digest"`
	Limitations []string `json:"limitations"`
}

var captureLimitations = []string{
	"Captured records are a filtered and sampled subset, redacted unless taken at a raw point; retries may duplicate records.",
	"Receipt order does not establish original batching, complete traces, global event order or production timing.",
	"A mid-pipeline or egress capture is injected at the explicitly chosen receiver; this does not execute a production pipeline suffix.",
	"Assertions must be supplied separately.",
}

func captureDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (s *Service) beginCaptureDerivation(ctx context.Context, caller Caller, perm authz.Permission) (func(), error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return nil, err
	}
	r := s.collectors
	r.mu.Lock()
	err := r.check()
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case r.derivationGate <- struct{}{}:
		return func() { <-r.derivationGate }, nil
	default:
		return nil, &Error{
			Code:    CodeResourceExhausted,
			Message: "capture input derivation is busy; retry after another derivation completes",
		}
	}
}

// CreateFixtureFromCapture derives a fixture from a capture's records.
// The capture is not changed.
func (s *Service) CreateFixtureFromCapture(ctx context.Context, caller Caller, id string, req CaptureDeriveRequest) (_ *CaptureFixture, err error) {
	defer s.audit(&caller, "CreateFixtureFromCapture", &err)()
	release, err := s.beginCaptureDerivation(ctx, caller, authz.CaptureRead)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.captureDataIn(caller, id); err != nil {
		return nil, err
	}
	caller.Project = s.captureProject(id)
	return s.createFixtureFromCapture(ctx, caller, id, req)
}

func (s *Service) createFixtureFromCapture(ctx context.Context, caller Caller, id string, req CaptureDeriveRequest) (*CaptureFixture, error) {
	if err := s.begin(ctx, caller, authz.CaptureRead); err != nil {
		return nil, err
	}
	sel := req.Selection
	if sel.Stream.CollectorID == "" || sel.Stream.Pipeline == "" || sel.Stream.Point == "" ||
		!slices.Contains([]string{"traces", "logs", "metrics"}, sel.Stream.Signal) ||
		!collectorText(sel.Receiver, 256, true) ||
		sel.Watermark < 1 ||
		len(sel.Sequences) > 1000 {
		return nil, captureInvalid(
			"choose one source Collector/pipeline/point/signal, a fixed watermark and explicit destination receiver",
		)
	}
	if len(req.ConfigYAML) == 0 || len(req.ConfigYAML) > 1<<20 {
		return nil, captureInvalid("destination config_yaml is required, at most 1 MiB")
	}
	if len(req.SetupYAML) > 256<<10 {
		return nil, captureInvalid("destination setup_yaml exceeds 256 KiB")
	}
	ins, err := s.InspectConfiguration(
		ctx,
		caller,
		InspectRequest{Config: Source{Text: req.ConfigYAML}, Setup: req.SetupYAML},
	)
	if err != nil {
		return nil, err
	}
	if !ins.Valid || ins.Topology == nil {
		return nil, captureInvalid("destination configuration is invalid")
	}
	found := false
	for _, p := range ins.Topology.Pipelines {
		if p.Signal == sel.Stream.Signal &&
			(slices.Contains(p.Receivers, sel.Receiver) || slices.Contains(p.Receivers, "receiver/"+sel.Receiver)) {
			found = true
		}
	}
	if !found {
		return nil, captureInvalid(
			fmt.Sprintf(
				"destination receiver %q does not accept %s in the current debugger configuration; choose a receiver used by a %s pipeline, or add that pipeline in the debugger",
				sel.Receiver,
				sel.Stream.Signal,
				sel.Stream.Signal,
			),
		)
	}
	r := s.collectors
	r.mu.Lock()
	if err := r.check(); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	c := r.sessions[id]
	if c == nil {
		r.mu.Unlock()
		return nil, &Error{Code: CodeNotFound, Message: "capture not found or expired"}
	}
	records, err := r.recordsLocked(ctx, c)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	if sel.Watermark > len(records) {
		r.mu.Unlock()
		return nil, captureInvalid("watermark exceeds available capture records")
	}
	wanted := map[int]bool{}
	for _, seq := range sel.Sequences {
		if seq < 1 || seq > sel.Watermark || wanted[seq] {
			r.mu.Unlock()
			return nil, captureInvalid("selected record sequences must be unique and within the watermark")
		}
		wanted[seq] = true
	}
	owned := []capturedRecord{}
	var source *CaptureSource
	for _, target := range c.view.Targets {
		if target.CollectorID == sel.Stream.CollectorID && target.Source != nil {
			copy := *target.Source
			source = &copy
		}
	}
	for _, record := range records[:sel.Watermark] {
		if record.Stream == sel.Stream && (len(wanted) == 0 || wanted[record.Sequence]) {
			record.Proto = slices.Clone(record.Proto)
			owned = append(owned, record)
		}
	}
	r.mu.Unlock()
	if len(wanted) == 0 && len(owned) > 1000 {
		return nil, captureInvalid(fmt.Sprintf("the stream has %d records up to the watermark; choose at most 1000 with sequences or a lower watermark", len(owned)))
	}
	if len(owned) == 0 || len(owned) > 1000 || len(wanted) > 0 && len(owned) != len(wanted) {
		return nil, captureInvalid("select 1–1000 records belonging to exactly the chosen source stream")
	}
	selection := sel
	selection.Sequences = []int{}
	sourceHash := sha256.New()
	payloads := []telemetry.Payload{}
	for _, record := range owned {
		if err := ctx.Err(); err != nil {
			return nil, classify(err, CodeCancelled)
		}
		p, err := telemetry.DecodeProto(record.Stream.Signal, record.Proto)
		if err != nil {
			return nil, &Error{Code: CodeStorageError, Message: "captured payload unavailable"}
		}
		fmt.Fprintf(sourceHash, "%d:%d:", record.Sequence, len(record.Proto))
		_, _ = sourceHash.Write(record.Proto)
		payloads = append(payloads, p)
		selection.Sequences = append(selection.Sequences, record.Sequence)
	}
	combined, _ := telemetry.Concat(payloads)
	raw, err := combined.JSON()
	if err != nil {
		return nil, &Error{Code: CodeStorageError, Message: "cannot materialize capture"}
	}
	if len(raw) > 4<<20 {
		return nil, &Error{
			Code:    CodeResourceExhausted,
			Message: "materialized fixture exceeds 4 MiB; select fewer records",
		}
	}
	result := &CaptureFixture{
		FixtureJSON: string(raw),
		Payload:     raw,
		Receiver:    sel.Receiver,
		Signal:      sel.Stream.Signal,
		Derivation: CaptureDerivation{
			SessionID:               id,
			Selection:               selection,
			SourceDigest:            hex.EncodeToString(sourceHash.Sum(nil)),
			Digest:                  captureDigest(raw),
			DestinationConfigDigest: captureDigest([]byte(req.ConfigYAML)),
			Records:                 len(owned),
		},
		Limitations: slices.Clone(captureLimitations),
	}
	result.Derivation.Source = source
	result.Derivation.SourceSensitivity = owned[0].Sensitivity
	if req.SetupYAML != "" {
		result.Derivation.DestinationSetupDigest = captureDigest([]byte(req.SetupYAML))
	}
	result.SetupYAML, err = captureInputSetup(req.SetupYAML, []CaptureDerivation{result.Derivation})
	if err != nil {
		return nil, err
	}
	if req.Save {
		repository, err := s.artifactStore(ctx, caller, authz.ArtifactWrite)
		if err != nil {
			return nil, err
		}
		label := req.Label
		if label == "" {
			label = "captured " + sel.Stream.Signal
		}
		origin, _ := json.Marshal(result.Derivation)
		if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
			return nil, err
		}
		result.Saved, err = saveArtifact(
			ctx, repository,
			artifact.SaveRequest{
				WorkspaceID: repository.Info().ID,
				Project:     caller.Project,
				Kind:        artifact.Fixture,
				Label:       label,
				Actor:       caller.Principal.ID,
				Content:     raw,
				Origin:      &artifact.Origin{Kind: "capture", BaseDigest: result.Derivation.Digest, Edit: origin},
			},
		)
		if err != nil {
			return nil, artifactError(err)
		}
	}
	return result, nil
}

// CreateScenarioFromCapture derives a scenario from a capture's records.
// The capture is not changed.
func (s *Service) CreateScenarioFromCapture(ctx context.Context, caller Caller, id string, req CaptureScenarioRequest) (_ *CaptureScenario, err error) {
	defer s.audit(&caller, "CreateScenarioFromCapture", &err)()
	release, err := s.beginCaptureDerivation(ctx, caller, authz.CaptureRead)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.captureDataIn(caller, id); err != nil {
		return nil, err
	}
	caller.Project = s.captureProject(id)
	if len(req.Inputs) < 1 || len(req.Inputs) > 16 || len(req.SetupYAML) > 256<<10 ||
		len(req.AssertionsYAML) > 256<<10 {
		return nil, captureInvalid("scenario requires 1–16 selections and bounded setup/assertions")
	}
	doc := &scenario.Scenario{Version: 1}
	if req.SetupYAML != "" {
		var err error
		doc, err = scenario.ParseSetup([]byte(req.SetupYAML))
		if err != nil {
			return nil, captureInvalid("invalid scenario setup")
		}
		doc.Version = 1
	}
	doc.Name = req.Name
	doc.Inputs = nil
	if req.AssertionsYAML != "" {
		var err error
		doc.Assertions, err = scenario.ParseAssertions([]byte(req.AssertionsYAML))
		if err != nil {
			return nil, captureInvalid("invalid capture scenario assertions")
		}
	}
	out := &CaptureScenario{Derivations: []CaptureDerivation{}, Limitations: slices.Clone(captureLimitations)}
	var bytes int
	for _, sel := range req.Inputs {
		fixture, err := s.createFixtureFromCapture(
			ctx,
			caller,
			id,
			CaptureDeriveRequest{ConfigYAML: req.ConfigYAML, SetupYAML: req.SetupYAML, Selection: sel},
		)
		if err != nil {
			return nil, err
		}
		bytes += len(fixture.Payload)
		if bytes > 4<<20 {
			return nil, &Error{Code: CodeResourceExhausted, Message: "materialized scenario exceeds 4 MiB"}
		}
		doc.Inputs = append(
			doc.Inputs,
			scenario.Input{
				Receiver: sel.Receiver,
				Signal:   sel.Stream.Signal,
				Data:     yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: string(fixture.Payload)},
			},
		)
		out.Derivations = append(out.Derivations, fixture.Derivation)
	}
	// Reuse the verified scenario-from-run node builder: zero Duration values
	// are omitted rather than marshalled into invalid duration scalars.
	first := doc.Inputs[0]
	raw, err := scenario.FromRun(
		scenario.RunSources{
			Mode:       "quick",
			Receiver:   first.Receiver,
			Fixture:    []byte(first.Data.Value),
			Setup:      []byte(req.SetupYAML),
			Assertions: []byte(req.AssertionsYAML),
		},
		req.Name,
		nil,
	)
	if err != nil {
		return nil, captureInvalid("cannot express capture inputs with the supplied setup/assertions")
	}
	var tree yaml.Node
	if yaml.Unmarshal(raw, &tree) != nil {
		return nil, &Error{Code: CodeInternal, Message: "scenario encoding failed"}
	}
	root := tree.Content[0]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "inputs" {
			for _, input := range doc.Inputs[1:] {
				var n yaml.Node
				if n.Encode(
					map[string]any{"receiver": input.Receiver, "signal": input.Signal, "data": input.Data.Value},
				) != nil {
					return nil, &Error{Code: CodeInternal, Message: "scenario input encoding failed"}
				}
				root.Content[i+1].Content = append(root.Content[i+1].Content, &n)
			}
		}
	}
	if err := setCaptureOrigins(root, out.Derivations); err != nil {
		return nil, err
	}
	raw, err = yaml.Marshal(&tree)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "scenario encoding failed"}
	}
	if _, err := scenario.Parse(raw, ""); err != nil {
		return nil, captureInvalid("derived scenario is invalid")
	}
	out.YAML = string(raw)
	out.Digest = captureDigest(raw)
	out.Export = "# Captured telemetry may contain sensitive data. Review before sharing.\n# Filtered/sampled/redacted subset; receipt order is not production timing.\n" + out.YAML
	if req.Save {
		repository, err := s.artifactStore(ctx, caller, authz.ArtifactWrite)
		if err != nil {
			return nil, err
		}
		origin, _ := json.Marshal(map[string]any{"capture_id": id, "derivations": out.Derivations})
		label := req.Label
		if label == "" {
			label = "captured scenario"
		}
		if err := s.workIn(&caller, authz.ArtifactWrite); err != nil {
			return nil, err
		}
		out.Saved, err = saveArtifact(
			ctx, repository,
			artifact.SaveRequest{
				WorkspaceID: repository.Info().ID,
				Project:     caller.Project,
				Kind:        artifact.Scenario,
				Content:     raw,
				Label:       label,
				Actor:       caller.Principal.ID,
				Origin:      &artifact.Origin{Kind: "capture", BaseDigest: out.Digest, Edit: origin},
			},
		)
		if err != nil {
			return nil, artifactError(err)
		}
	}
	return out, nil
}

func setCaptureOrigins(root *yaml.Node, origins []CaptureDerivation) error {
	if err := provenance.ValidateCaptures(origins); err != nil {
		return captureInvalid("capture provenance exceeds its document bound")
	}
	var node yaml.Node
	if err := node.Encode(origins); err != nil {
		return &Error{Code: CodeInternal, Message: "capture provenance encoding failed"}
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "capture_origins" {
			root.Content[i+1] = &node
			return nil
		}
	}
	root.Content = append(
		root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "capture_origins"},
		&node,
	)
	return nil
}

func captureInputSetup(setup string, origins []CaptureDerivation) (string, error) {
	if setup == "" {
		setup = "version: 1\n"
	}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(setup), &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", captureInvalid("destination setup must be a YAML mapping")
	}
	if err := setCaptureOrigins(doc.Content[0], origins); err != nil {
		return "", err
	}
	raw, err := yaml.Marshal(&doc)
	if err != nil {
		return "", &Error{Code: CodeInternal, Message: "capture setup encoding failed"}
	}
	return string(raw), nil
}

func inheritedCaptureSetup(setup string, source Source) (string, error) {
	if source.origin == nil || source.origin.Kind != "capture" ||
		source.origin.BaseDigest != captureDigest([]byte(source.Text)) {
		return setup, nil
	}
	var origin CaptureDerivation
	if decodeStored(source.origin.Edit, &origin) != nil {
		return "", &Error{Code: CodeStorageError, Message: "saved capture fixture provenance is unavailable"}
	}
	return captureInputSetup(setup, []CaptureDerivation{origin})
}

// ExportCaptureSelection derives a fixture or scenario and returns it as
// a file; nothing is saved.
func (s *Service) ExportCaptureSelection(ctx context.Context, caller Caller, id string, req CaptureExportRequest) (_ *CaptureExport, err error) {
	defer s.audit(&caller, "ExportCaptureSelection", &err)()
	if err := s.begin(ctx, caller, authz.CaptureRead); err != nil {
		return nil, err
	}
	if err := s.captureDataIn(caller, id); err != nil {
		return nil, err
	}
	if (req.Fixture == nil) == (req.Scenario == nil) {
		return nil, captureInvalid("export requires exactly one fixture or scenario selection")
	}
	if req.Fixture != nil {
		copy := *req.Fixture
		copy.Save = false
		v, err := s.CreateFixtureFromCapture(ctx, caller, id, copy)
		if err != nil {
			return nil, err
		}
		return &CaptureExport{
			Format:      "otlp-json",
			Content:     v.FixtureJSON,
			SetupYAML:   v.SetupYAML,
			Digest:      v.Derivation.Digest,
			Limitations: v.Limitations,
		}, nil
	}
	copy := *req.Scenario
	copy.Save = false
	v, err := s.CreateScenarioFromCapture(ctx, caller, id, copy)
	if err != nil {
		return nil, err
	}
	return &CaptureExport{
		Format:      "scenario-yaml",
		Content:     v.Export,
		Digest:      captureDigest([]byte(v.Export)),
		Limitations: v.Limitations,
	}, nil
}
