package mcp

import (
	"context"
	"encoding/base64"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type artifactRevisionsInput struct {
	ArtifactID string `json:"artifact_id" jsonschema:"Saved artifact ID."`
	Limit      int    `json:"limit,omitempty" jsonschema:"Revisions per page; 0 uses the server default."`
	Cursor     string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page; empty starts at the newest revision."`
}

type artifactRevisionsResult struct {
	platform.ArtifactRevisionPage
}

func (r artifactRevisionsResult) summary() string {
	return fmt.Sprintf("Artifact revisions: %d listed, newest first; more pages %t.", len(r.Items), r.NextCursor != "")
}

type exportForCIInput struct {
	ConfigurationID string   `json:"configuration_id" jsonschema:"Saved configuration artifact ID; its head revision is exported."`
	ScenarioIDs     []string `json:"scenario_ids" jsonschema:"Saved scenario artifact IDs, from 1 to 32; each head revision is exported."`
}

type exportForCIResult struct {
	Name    string `json:"name"`
	Bytes   int    `json:"bytes"`
	Archive string `json:"archive_base64"`
}

func (r exportForCIResult) summary() string {
	return fmt.Sprintf("CI archive: %d bytes of zip, base64 encoded.", r.Bytes)
}

type importArtifactsInput struct {
	Archive string `json:"archive_base64" jsonschema:"A CI archive from linea_export_artifacts_for_ci, base64 encoded, possibly edited."`
}

type importArtifactsResult struct {
	platform.ArtifactImport
}

func (r importArtifactsResult) summary() string {
	counts := map[string]int{}
	for _, item := range r.Items {
		counts[item.Status]++
	}
	return fmt.Sprintf("Artifact import: %d created, %d revised, %d unchanged.", counts["created"], counts["revised"], counts["unchanged"])
}

// addArtifactTools exposes saved revisions and the CI archive. Archives
// travel as base64 text, as artifact content does over HTTP.
func addArtifactTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_artifact_revisions", "List a saved artifact's revision metadata newest first, in pages that stay stable while it gains revisions. No content.",
		func(ctx context.Context, in artifactRevisionsInput) (summarized, error) {
			page, err := service.ListArtifactRevisions(ctx, callerOf(ctx), platform.ListArtifactRevisionsRequest{ArtifactID: in.ArtifactID, Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, err
			}
			return artifactRevisionsResult{*page}, nil
		})
	addTool(server, "linea_export_artifacts_for_ci", "Export a saved configuration and scenarios at their head revisions as a zip for CI: collector/config.yaml, collector/tests/*.yaml, a GitHub Actions workflow and a manifest for import. Environment values are never included.",
		func(ctx context.Context, in exportForCIInput) (summarized, error) {
			exp, err := service.ExportArtifactsForCI(ctx, callerOf(ctx), platform.ExportCIRequest{Configuration: in.ConfigurationID, Scenarios: in.ScenarioIDs})
			if err != nil {
				return nil, err
			}
			return exportForCIResult{Name: exp.Name, Bytes: len(exp.Archive), Archive: base64.StdEncoding.EncodeToString(exp.Archive)}, nil
		})
	addTool(server, "linea_import_artifacts", "Import a CI archive: a changed file revises the artifact it was exported from, an unchanged one is left alone, and a file from another workspace becomes a new artifact. Refused with conflict when an artifact was revised since the export.",
		func(ctx context.Context, in importArtifactsInput) (summarized, error) {
			archive, err := base64.StdEncoding.DecodeString(in.Archive)
			if err != nil {
				return nil, invalidRequest("archive_base64 is not valid base64")
			}
			imp, err := service.ImportArtifacts(ctx, callerOf(ctx), archive)
			if err != nil {
				return nil, err
			}
			return importArtifactsResult{*imp}, nil
		})
}
