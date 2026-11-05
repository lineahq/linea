package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// ListProjects returns the projects the caller may see and the project
// set's revision.
func (c *Client) ListProjects(ctx context.Context) (*api.Projects, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Projects](c, raw)
}

// CreateProject creates a project at the revision in req.
func (c *Client) CreateProject(ctx context.Context, req api.ProjectRequest) (*api.Projects, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/projects", req, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Projects](c, raw)
}

// UpdateProject replaces project id's name and settings.
func (c *Client) UpdateProject(ctx context.Context, id string, req api.ProjectRequest) (*api.Projects, error) {
	raw, err := c.do(ctx, http.MethodPut, "/api/v1/projects/"+url.PathEscape(id), req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Projects](c, raw)
}

// SetProjectArchived archives or unarchives project id at revision.
func (c *Client) SetProjectArchived(ctx context.Context, revision uint64, id string, archived bool) (*api.Projects, error) {
	action := "/unarchive"
	if archived {
		action = "/archive"
	}
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(id)+action, api.ProjectRevision{Revision: revision}, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Projects](c, raw)
}

// ListLibrary returns one page of the library; kind may be empty.
func (c *Client) ListLibrary(ctx context.Context, kind, cursor string, limit int) (*api.LibraryPage, error) {
	q := url.Values{}
	if kind != "" {
		q.Set("kind", kind)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/library"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.LibraryPage](c, raw)
}

// SetLibraryAudience shares a library artifact with projects.
func (c *Client) SetLibraryAudience(ctx context.Context, artifactID string, req api.SharingRequest) (*api.Sharing, error) {
	raw, err := c.do(ctx, http.MethodPut, "/api/v1/library/"+url.PathEscape(artifactID)+"/audience", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Sharing](c, raw)
}

// SetRuntimeAvailability limits the projects a runtime runs work for.
func (c *Client) SetRuntimeAvailability(ctx context.Context, runtime string, req api.SharingRequest) (*api.Sharing, error) {
	raw, err := c.do(ctx, http.MethodPut, "/api/v1/runtimes/"+url.PathEscape(runtime)+"/availability", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return decodeAs[api.Sharing](c, raw)
}

// CopyArtifact copies an artifact revision into the client's project.
func (c *Client) CopyArtifact(ctx context.Context, artifactID string, req api.CopyRequest) (*platform.SavedArtifact, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/copies", req, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	return decodeAs[platform.SavedArtifact](c, raw)
}

func decodeAs[T any](c *Client, raw []byte) (*T, error) {
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}
