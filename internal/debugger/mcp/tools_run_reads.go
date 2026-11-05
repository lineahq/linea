package mcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Run, suite and candidate reads that HTTP and the CLI already had.
// Structured results use the HTTP JSON field names; the
// text counts only.

type runSource struct {
	Label    string                `json:"label,omitempty"`
	Artifact *platform.ArtifactRef `json:"artifact,omitempty"`
	SHA256   string                `json:"sha256"`
	Text     string                `json:"text"`
}

type runInputsResult struct {
	RunID               string     `json:"run_id"`
	Mode                string     `json:"mode"`
	Config              runSource  `json:"config"`
	Fixture             *runSource `json:"fixture,omitempty"`
	Scenario            *runSource `json:"scenario,omitempty"`
	Setup               *runSource `json:"setup,omitempty"`
	Assertions          *runSource `json:"assertions,omitempty"`
	Receiver            string     `json:"receiver,omitempty"`
	ClientAddress       string     `json:"client_address,omitempty"`
	Files               []string   `json:"files"`
	RequiredEnvironment []string   `json:"required_environment"`
}

func (r runInputsResult) summary() string {
	sources := 1
	for _, s := range []*runSource{r.Fixture, r.Scenario, r.Setup, r.Assertions} {
		if s != nil {
			sources++
		}
	}
	return fmt.Sprintf("Run inputs: %s run; %s, %s, %s.", r.Mode, plural(sources, "root source"), plural(len(r.Files), "input file"),
		plural(len(r.RequiredEnvironment), "required environment name"))
}

func runInputsOf(in *platform.RunInputs) runInputsResult {
	source := func(s *platform.RunSource) *runSource {
		if s == nil {
			return nil
		}
		return &runSource{Label: s.Label, Artifact: s.Artifact, SHA256: s.SHA256, Text: s.Text}
	}
	return runInputsResult{RunID: in.RunID, Mode: string(in.Mode), Config: *source(&in.Config), Fixture: source(in.Fixture), Scenario: source(in.Scenario),
		Setup: source(in.Setup), Assertions: source(in.Assertions), Receiver: in.Receiver, ClientAddress: in.ClientAddress,
		Files: orEmpty(in.Files), RequiredEnvironment: orEmpty(in.RequiredEnvironment)}
}

type suiteInfo struct {
	ID      string        `json:"id"`
	Created time.Time     `json:"created"`
	Config  string        `json:"config"`
	Members []suiteMember `json:"members"`
}

type suiteMember struct {
	Label string `json:"label"`
	RunID string `json:"run_id,omitempty"`
	Error string `json:"error,omitempty"`
}

type suitesResult struct {
	Suites []suiteInfo `json:"suites"`
}

func (r suitesResult) summary() string {
	members := 0
	for _, s := range r.Suites {
		members += len(s.Members)
	}
	return fmt.Sprintf("Suites: %d listed with %s.", len(r.Suites), plural(members, "member"))
}

type snapshotInput struct {
	RunID string `json:"run_id" jsonschema:"A finished run's ID."`
	Ref   string `json:"ref" jsonschema:"Snapshot reference from the run's report, such as a stage's input or output snapshot."`
}

type snapshotResult struct {
	RunID string `json:"run_id"`
	Ref   string `json:"ref"`
	// Snapshot is the recorded OTLP JSON, base64 encoded.
	Snapshot string `json:"snapshot_base64"`
	Bytes    int    `json:"bytes"`
}

func (r snapshotResult) summary() string {
	return fmt.Sprintf("Snapshot: %d bytes of recorded OTLP JSON.", r.Bytes)
}

type createCandidateInput struct {
	Config       source              `json:"config" jsonschema:"Base Collector configuration source; never written."`
	Edit         *statementEditInput `json:"edit,omitempty" jsonschema:"Addressed statement edit; supply exactly one of edit or proposed_yaml."`
	ProposedYAML *string             `json:"proposed_yaml,omitempty" jsonschema:"Complete proposed configuration YAML; supply exactly one of proposed_yaml or edit."`
	Save         bool                `json:"save,omitempty" jsonschema:"Save a valid proposal as a new artifact in the server's workspace."`
	Label        string              `json:"label,omitempty" jsonschema:"Label of the saved artifact."`
	Runtime      string              `json:"runtime,omitempty" jsonschema:"Built runtime to validate on; empty is the built-in registry."`
}

type candidateResult struct {
	ProposedYAML      string                  `json:"proposed_yaml"`
	Patch             string                  `json:"patch"`
	AffectedPipelines []string                `json:"affected_pipelines"`
	Valid             bool                    `json:"valid"`
	Diagnostics       []platform.Diagnostic   `json:"diagnostics"`
	Saved             *platform.SavedArtifact `json:"saved,omitempty"`
}

func (r candidateResult) summary() string {
	errs, warns := 0, 0
	for _, d := range r.Diagnostics {
		if d.Severity == "error" {
			errs++
		} else {
			warns++
		}
	}
	return fmt.Sprintf("Candidate: valid %t; %s, %s; %s affected; saved %t.", r.Valid, plural(errs, "error"), plural(warns, "warning"),
		plural(len(r.AffectedPipelines), "pipeline"), r.Saved != nil)
}

func addRunReadTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_get_run_inputs", "Read the root sources a run was admitted with: the configuration, fixture, scenario, setup and assertions texts with their labels and digests. Never provider-captured or environment values.",
		func(ctx context.Context, in runInput) (summarized, error) {
			inputs, err := service.GetRunInputs(ctx, callerOf(ctx), in.RunID)
			if err != nil {
				return nil, err
			}
			return runInputsOf(inputs), nil
		})
	addTool(server, "linea_get_snapshot", "Read one recorded snapshot of a finished run, base64 encoded, by the reference its report names.",
		func(ctx context.Context, in snapshotInput) (summarized, error) {
			data, err := service.GetSnapshot(ctx, callerOf(ctx), in.RunID, in.Ref)
			if err != nil {
				return nil, err
			}
			return snapshotResult{RunID: in.RunID, Ref: in.Ref, Snapshot: base64.StdEncoding.EncodeToString(data), Bytes: len(data)}, nil
		})
	addTool(server, "linea_list_suites", "List the retained scenario suites with their members. linea_get_test_results with suite_id reads one suite's states and outcomes.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			suites, err := service.ListSuites(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			out := suitesResult{Suites: make([]suiteInfo, 0, len(suites))}
			for _, s := range suites {
				info := suiteInfo{ID: s.ID, Created: s.Created, Config: s.Config, Members: make([]suiteMember, 0, len(s.Members))}
				for _, m := range s.Members {
					info.Members = append(info.Members, suiteMember{Label: m.Label, RunID: m.RunID, Error: m.Error})
				}
				out.Suites = append(out.Suites, info)
			}
			return out, nil
		})
	addTool(server, "linea_create_candidate", "Validate a proposed configuration or one addressed statement edit without running it: the proposed text, a patch, the affected pipelines and diagnostics. Optionally save a valid proposal as a new artifact. linea_test_statement also runs it.",
		func(ctx context.Context, in createCandidateInput) (summarized, error) {
			if (in.Edit == nil) == (in.ProposedYAML == nil) {
				return nil, invalidRequest("supply exactly one of edit or proposed_yaml")
			}
			base, err := in.Config.platform()
			if err != nil {
				return nil, err
			}
			req := platform.CandidateRequest{Base: base, Save: in.Save, Label: in.Label, Runtime: in.Runtime}
			if in.ProposedYAML != nil {
				req.YAML = *in.ProposedYAML
			} else if req.Statement, err = in.Edit.platform(); err != nil {
				return nil, err
			}
			c, err := service.CreateCandidate(ctx, callerOf(ctx), req)
			if err != nil {
				return nil, err
			}
			out := candidateResult{ProposedYAML: c.Proposed, Patch: c.Patch, AffectedPipelines: orEmpty(c.AffectedPipelines), Valid: c.Valid,
				Diagnostics: c.Diagnostics, Saved: c.Saved}
			if out.Diagnostics == nil {
				out.Diagnostics = []platform.Diagnostic{}
			}
			return out, nil
		})
}
