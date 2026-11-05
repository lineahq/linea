package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// GetCollectorGroup returns a Collector group.
func (c *Client) GetCollectorGroup(ctx context.Context, id string) (*platform.CollectorGroup, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collector-groups/"+url.PathEscape(id), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var v platform.CollectorGroup
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, c.malformed(err)
	}
	return &v, nil
}

// ListCollectorGroups returns a page of Collector groups.
func (c *Client) ListCollectorGroups(ctx context.Context, q platform.CollectorPageRequest) (*platform.CollectorGroupPage, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collector-groups"+collectorQuery(q), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var v platform.CollectorGroupPage
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, c.malformed(err)
	}
	return &v, nil
}

// ListCollectorGroupMembers returns a page of a group's Collectors.
func (c *Client) ListCollectorGroupMembers(ctx context.Context, id string, q platform.CollectorPageRequest) (*platform.CollectorGroupMembersPage, error) {
	raw, err := c.do(
		ctx,
		http.MethodGet,
		"/api/v1/collector-groups/"+url.PathEscape(id)+"/members"+collectorQuery(q),
		nil,
		http.StatusOK,
	)
	if err != nil {
		return nil, err
	}
	var v platform.CollectorGroupMembersPage
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, c.malformed(err)
	}
	return &v, nil
}

// UpdateCaptureMetadata renames or describes a capture session.
func (c *Client) UpdateCaptureMetadata(ctx context.Context, id string, q platform.CaptureMetadataUpdate) (*platform.CaptureSession, error) {
	raw, err := c.do(ctx, http.MethodPatch, "/api/v1/captures/"+url.PathEscape(id), q, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var v platform.CaptureSession
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, c.malformed(err)
	}
	return &v, nil
}
