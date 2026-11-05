package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Projects and the library. A client works in the project its
// Linea-Project header or ?project= names; these tools list what it may
// use there and copy shared work into it.

type listProjectsInput struct{}

type mcpProject struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	BuiltIn             bool     `json:"built_in"`
	Archived            bool     `json:"archived"`
	DefaultRuntime      string   `json:"default_runtime,omitempty"`
	StorageLimit        int64    `json:"storage_limit,omitempty"`
	RetentionRuns       int      `json:"retention_runs,omitempty"`
	RetentionAgeSeconds int64    `json:"retention_age_seconds,omitempty"`
	Permissions         []string `json:"permissions"`
}

type projectsResult struct {
	Revision uint64       `json:"revision"`
	Projects []mcpProject `json:"projects"`
}

func (r projectsResult) summary() string {
	return fmt.Sprintf("Projects: %d listed at revision %d.", len(r.Projects), r.Revision)
}

type listLibraryInput struct {
	Kind   string `json:"kind,omitempty" jsonschema:"configuration, fixture or scenario; empty lists every kind."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Items per page; 0 uses the server default."`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor from the previous page."`
}

type libraryItem struct {
	Artifact platform.ArtifactMetadata `json:"artifact"`
	Audience []string                  `json:"audience"`
	SharedBy string                    `json:"shared_by,omitempty"`
	Shared   *time.Time                `json:"shared,omitempty"`
}

type libraryResult struct {
	Items      []libraryItem `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func (r libraryResult) summary() string {
	return fmt.Sprintf("Library: %d items listed; more pages %t.", len(r.Items), r.NextCursor != "")
}

type copyArtifactInput struct {
	ArtifactID string `json:"artifact_id" jsonschema:"The artifact to copy: one of the project's, or a library item shared with it."`
	RevisionID string `json:"revision_id,omitempty" jsonschema:"The revision to copy; empty copies the current one."`
	Label      string `json:"label,omitempty" jsonschema:"The copy's label; empty keeps the source's."`
}

type copyArtifactResult struct {
	platform.SavedArtifact
}

func (r copyArtifactResult) summary() string {
	return fmt.Sprintf("Copied into project %s as artifact %s.", r.Artifact.Project, r.Artifact.ID)
}

func addProjectTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_projects", "List the projects in which the caller holds a permission, with those permissions, and the library. Requests work in the project the client's Linea-Project header or ?project= names, else the default project.",
		func(ctx context.Context, _ listProjectsInput) (summarized, error) {
			l, err := service.ListProjects(ctx, callerOf(ctx))
			if err != nil {
				return nil, err
			}
			out := projectsResult{Revision: l.Revision, Projects: make([]mcpProject, 0, len(l.Projects))}
			for _, p := range l.Projects {
				perms := p.Permissions
				if perms == nil {
					perms = []string{}
				}
				out.Projects = append(out.Projects, mcpProject{ID: p.ID, Name: p.Name, BuiltIn: p.BuiltIn, Archived: p.Archived, DefaultRuntime: p.DefaultRuntime,
					StorageLimit: p.StorageLimit, RetentionRuns: p.RetentionRuns, RetentionAgeSeconds: int64(p.RetentionAge / time.Second), Permissions: perms})
			}
			return out, nil
		})
	addTool(server, "linea_list_library", "List the library: configurations, fixtures and scenarios an admin published for projects to use. Within a project it lists the items shared with that project; such a scenario or fixture can be named as a saved source there, or copied in with linea_copy_artifact.",
		func(ctx context.Context, in listLibraryInput) (summarized, error) {
			page, err := service.ListLibrary(ctx, callerOf(ctx), platform.ListLibraryRequest{Kind: platform.ArtifactKind(in.Kind), Limit: in.Limit, Cursor: in.Cursor})
			if err != nil {
				return nil, err
			}
			out := libraryResult{Items: make([]libraryItem, 0, len(page.Items)), NextCursor: page.NextCursor}
			for _, item := range page.Items {
				audience := item.Audience
				if audience == nil {
					audience = []string{}
				}
				out.Items = append(out.Items, libraryItem{Artifact: item.Artifact, Audience: audience, SharedBy: item.SharedBy, Shared: item.Shared})
			}
			return out, nil
		})
	addTool(server, "linea_copy_artifact", "Copy a revision of an artifact the caller can read into the client's project as a new artifact that keeps the revision's provenance, for example a library scenario to edit.",
		func(ctx context.Context, in copyArtifactInput) (summarized, error) {
			saved, err := service.CopyArtifact(ctx, callerOf(ctx), platform.CopyArtifactRequest{ArtifactID: in.ArtifactID, RevisionID: in.RevisionID, Label: in.Label})
			if err != nil {
				return nil, err
			}
			return copyArtifactResult{*saved}, nil
		})
}
