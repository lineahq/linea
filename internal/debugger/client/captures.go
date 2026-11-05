package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/lineahq/linea/internal/debugger/platform"
)

func captureCall[T any](ctx context.Context, c *Client, method, path string, body any) (*T, error) {
	raw, err := c.do(ctx, method, path, body, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, c.malformed(err)
	}
	return &result, nil
}

// ValidateCaptureRequest checks a capture selection without starting it.
func (c *Client) ValidateCaptureRequest(ctx context.Context, req platform.CaptureSelection) (*platform.CaptureValidation, error) {
	return captureCall[platform.CaptureValidation](ctx, c, http.MethodPost, "/api/v1/captures/validate", req)
}

// CreateCaptureSession starts a capture session.
func (c *Client) CreateCaptureSession(ctx context.Context, req platform.CaptureSelection) (*platform.CaptureSession, error) {
	return captureCall[platform.CaptureSession](ctx, c, http.MethodPost, "/api/v1/captures", req)
}

// ListCaptureSessions returns a page of capture sessions.
func (c *Client) ListCaptureSessions(ctx context.Context, req platform.CollectorPageRequest) (*platform.CaptureSessionPage, error) {
	return captureCall[platform.CaptureSessionPage](ctx, c, http.MethodGet, "/api/v1/captures"+collectorQuery(req), nil)
}

// GetCaptureSession returns a capture session, waiting until it is
// terminal when wait is set.
func (c *Client) GetCaptureSession(ctx context.Context, id string, wait bool) (*platform.CaptureSession, error) {
	path := "/api/v1/captures/" + url.PathEscape(id)
	if wait {
		path += "?wait=true"
	}
	return captureCall[platform.CaptureSession](ctx, c, http.MethodGet, path, nil)
}

// StopCaptureSession stops a capture session.
func (c *Client) StopCaptureSession(ctx context.Context, id string) (*platform.CaptureSession, error) {
	return captureCall[platform.CaptureSession](ctx, c, http.MethodPost, "/api/v1/captures/"+url.PathEscape(id)+"/stop", struct{}{})
}
