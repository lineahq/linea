package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// ListArtifactRevisions returns one page of a saved artifact's revision
// metadata, newest first.
func (c *Client) ListArtifactRevisions(ctx context.Context, req platform.ListArtifactRevisionsRequest) (*platform.ArtifactRevisionPage, error) {
	q := url.Values{}
	if req.Limit != 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Cursor != "" {
		q.Set("cursor", req.Cursor)
	}
	path := "/api/v1/artifacts/" + url.PathEscape(req.ArtifactID) + "/revisions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var page platform.ArtifactRevisionPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, c.malformed(err)
	}
	return &page, nil
}

// ExportArtifactsForCI returns the CI zip archive of a saved configuration
// and scenarios at their head revisions.
func (c *Client) ExportArtifactsForCI(ctx context.Context, req api.ExportForCIRequest) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/artifacts/export", req, http.StatusOK)
}

// ImportArtifacts imports a CI archive into the server's workspace.
func (c *Client) ImportArtifacts(ctx context.Context, archive []byte) (*platform.ArtifactImport, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/artifacts/import", api.ImportRequest{Archive: archive}, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out platform.ArtifactImport
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// RunLog reads the Collector log tail a failed or partial run kept.
func (c *Client) RunLog(ctx context.Context, id string) (*platform.RunLog, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+url.PathEscape(id)+"/log", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out platform.RunLog
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// ListArtifacts returns one page of the workspace's artifact metadata as the
// server sent it.
func (c *Client) ListArtifacts(ctx context.Context, req platform.ListArtifactsRequest) ([]byte, error) {
	q := url.Values{}
	if req.Kind != "" {
		q.Set("kind", string(req.Kind))
	}
	if req.Limit != 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Cursor != "" {
		q.Set("cursor", req.Cursor)
	}
	path := "/api/v1/artifacts"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
}

// GetArtifact returns a saved artifact's metadata as the server sent it.
func (c *Client) GetArtifact(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(id), nil, http.StatusOK)
}

// GetArtifactRevision returns one revision with its content as the server
// sent it.
func (c *Client) GetArtifactRevision(ctx context.Context, id, revision string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(id)+"/revisions/"+url.PathEscape(revision), nil, http.StatusOK)
}

// SaveArtifact saves a new artifact or revision; a stale edit is an *Error
// with the current metadata.
func (c *Client) SaveArtifact(ctx context.Context, req api.SaveRequest) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/api/v1/artifacts", req, http.StatusCreated)
}

// Samples returns the shipped samples as the server sent them.
func (c *Client) Samples(ctx context.Context) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/samples", nil, http.StatusOK)
}

// ListAuditEvents returns a page of the server's audit log, newest first, as
// the server sent it.
func (c *Client) ListAuditEvents(ctx context.Context, f platform.AuditFilter) ([]byte, error) {
	q := url.Values{}
	for name, value := range map[string]string{"principal": f.Principal, "operation": f.Operation, "outcome": string(f.Outcome), "cursor": f.Cursor} {
		if value != "" {
			q.Set(name, value)
		}
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.Format(time.RFC3339))
	}
	if f.Limit != 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/api/v1/audit-events"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
}
