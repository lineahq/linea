package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func collectorQuery(req platform.CollectorPageRequest) string {
	q := url.Values{}
	if req.Limit != 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Cursor != "" {
		q.Set("cursor", req.Cursor)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// ListCollectors returns a page of connected and recently seen Collectors.
func (c *Client) ListCollectors(ctx context.Context, req platform.CollectorPageRequest) (*platform.CollectorPage, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collectors"+collectorQuery(req), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var result platform.CollectorPage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, c.malformed(err)
	}
	return &result, nil
}

// GetCollector returns a Collector.
func (c *Client) GetCollector(ctx context.Context, id string) (*platform.Collector, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collectors/"+url.PathEscape(id), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var result platform.Collector
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, c.malformed(err)
	}
	return &result, nil
}

// ListCapturePoints returns a page of a Collector's capture points.
func (c *Client) ListCapturePoints(ctx context.Context, id string, req platform.CollectorPageRequest) (*platform.CapturePointPage, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/collectors/"+url.PathEscape(id)+"/capture-points"+collectorQuery(req), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var result platform.CapturePointPage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, c.malformed(err)
	}
	return &result, nil
}
