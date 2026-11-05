package mcp

import (
	"context"
	"encoding/base64"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/samples"
)

// Saved work and samples that HTTP and the web already had.
// Content travels base64 encoded, as over HTTP; the text counts only.

type listArtifactsInput struct {
	Kind   string `json:"kind,omitempty" jsonschema:"Only this kind: configuration, fixture, scenario, setup or manifest; empty lists every kind."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Artifacts per page; 0 uses the server default."`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page; empty starts at the first page."`
}

type artifactsResult struct {
	platform.ArtifactPage
}

func (r artifactsResult) summary() string {
	return fmt.Sprintf("Artifacts: %d listed; more pages %t.", len(r.Items), r.NextCursor != "")
}

type getArtifactInput struct {
	ArtifactID string `json:"artifact_id" jsonschema:"Saved artifact ID."`
	RevisionID string `json:"revision_id,omitempty" jsonschema:"A revision to read with its content, such as the metadata's head_revision_id; empty reads metadata only."`
}

type artifactResult struct {
	Artifact platform.ArtifactMetadata  `json:"artifact"`
	Revision *platform.ArtifactRevision `json:"revision,omitempty"`
}

func (r artifactResult) summary() string {
	if r.Revision == nil {
		return fmt.Sprintf("Artifact: generation %d; no revision read.", r.Artifact.Generation)
	}
	return fmt.Sprintf("Artifact: generation %d; revision of %d bytes, base64 encoded.", r.Artifact.Generation, len(r.Revision.Content))
}

type saveArtifactInput struct {
	WorkspaceID        string `json:"workspace_id,omitempty" jsonschema:"Workspace the save expects; empty is the server's workspace."`
	ArtifactID         string `json:"artifact_id,omitempty" jsonschema:"Artifact to revise; empty creates a new artifact."`
	ExpectedGeneration uint64 `json:"expected_generation,omitempty" jsonschema:"The artifact generation this edit was based on."`
	ExpectedHead       string `json:"expected_head_revision_id,omitempty" jsonschema:"The head revision this edit was based on; a stale head is a conflict carrying the current metadata."`
	Kind               string `json:"kind" jsonschema:"configuration, fixture, scenario, setup or manifest."`
	Label              string `json:"label" jsonschema:"Display label of the artifact."`
	Content            string `json:"content_base64" jsonschema:"The original source bytes, base64 encoded; saved unchanged and never validated."`
}

type savedResult struct {
	platform.SavedArtifact
}

func (r savedResult) summary() string {
	return fmt.Sprintf("Artifact saved: generation %d, %d bytes.", r.Artifact.Generation, r.Revision.Bytes)
}

type samplesResult struct {
	Samples []samples.Sample `json:"samples"`
}

func (r samplesResult) summary() string {
	return fmt.Sprintf("Samples: %d listed.", len(r.Samples))
}

func addSavedWorkTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_artifacts", "List the saved artifacts of the server's workspace with its ID, in pages: metadata only, no content.",
		func(ctx context.Context, in listArtifactsInput) (summarized, error) {
			page, err := service.ListArtifacts(ctx, callerOf(ctx), platform.ListArtifactsRequest{Kind: platform.ArtifactKind(in.Kind), Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, err
			}
			return artifactsResult{*page}, nil
		})
	addTool(server, "linea_get_artifact", "Read a saved artifact's metadata and, with revision_id, that revision and its content base64 encoded.",
		func(ctx context.Context, in getArtifactInput) (summarized, error) {
			m, err := service.GetArtifact(ctx, callerOf(ctx), in.ArtifactID)
			if err != nil {
				return nil, err
			}
			out := artifactResult{Artifact: m}
			if in.RevisionID != "" {
				if out.Revision, err = service.GetArtifactRevision(ctx, callerOf(ctx), in.ArtifactID, in.RevisionID); err != nil {
					return nil, err
				}
			}
			return out, nil
		})
	addTool(server, "linea_save_artifact", "Save original bytes as a new artifact, or as a new revision of one when its expected generation and head match; a stale edit is a conflict carrying the current metadata. Content is never validated or rewritten.",
		func(ctx context.Context, in saveArtifactInput) (summarized, error) {
			content, err := base64.StdEncoding.DecodeString(in.Content)
			if err != nil {
				return nil, invalidRequest("content_base64 is not base64")
			}
			if in.WorkspaceID == "" {
				info, err := service.GetWorkspaceInfo(ctx, callerOf(ctx))
				if err != nil {
					return nil, err
				}
				in.WorkspaceID = info.ID
			}
			saved, err := service.SaveArtifact(ctx, callerOf(ctx), platform.SaveArtifactRequest{WorkspaceID: in.WorkspaceID, ArtifactID: in.ArtifactID,
				ExpectedGeneration: in.ExpectedGeneration, ExpectedHead: in.ExpectedHead, Kind: platform.ArtifactKind(in.Kind), Label: in.Label, Content: content})
			if err != nil {
				return nil, err
			}
			return savedResult{*saved}, nil
		})
	addTool(server, "linea_list_samples", "List the shipped samples: each one's configuration, scenario with inlined inputs, OTLP JSON input, receiver and expected outcome.",
		func(ctx context.Context, in struct{}) (summarized, error) {
			list, err := samples.List()
			if err != nil {
				return nil, err
			}
			return samplesResult{Samples: list}, nil
		})
}
