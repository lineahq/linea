package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// GetCaptureRecords returns a page of a capture's records.
func (c *Client) GetCaptureRecords(ctx context.Context, id string, req platform.CaptureRecordsRequest) (*platform.CaptureRecordsPage, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(req.Limit))
	q.Set("cursor", req.Cursor)
	q.Set("collector_id", req.Stream.CollectorID)
	q.Set("pipeline", req.Stream.Pipeline)
	q.Set("point", req.Stream.Point)
	q.Set("signal", req.Stream.Signal)
	return captureCall[platform.CaptureRecordsPage](ctx, c, http.MethodGet, "/api/v1/captures/"+url.PathEscape(id)+"/records?"+q.Encode(), nil)
}

// CreateFixtureFromCapture derives a fixture from captured records.
func (c *Client) CreateFixtureFromCapture(ctx context.Context, id string, req platform.CaptureDeriveRequest) (*platform.CaptureFixture, error) {
	return captureCall[platform.CaptureFixture](ctx, c, http.MethodPost, "/api/v1/captures/"+url.PathEscape(id)+"/fixtures", req)
}

// CreateScenarioFromCapture derives a scenario from captured records.
func (c *Client) CreateScenarioFromCapture(ctx context.Context, id string, req platform.CaptureScenarioRequest) (*platform.CaptureScenario, error) {
	return captureCall[platform.CaptureScenario](ctx, c, http.MethodPost, "/api/v1/captures/"+url.PathEscape(id)+"/scenario", req)
}

// ExportCaptureSelection exports a fixture or scenario derived from
// captured records.
func (c *Client) ExportCaptureSelection(ctx context.Context, id string, req platform.CaptureExportRequest) (*platform.CaptureExport, error) {
	return captureCall[platform.CaptureExport](ctx, c, http.MethodPost, "/api/v1/captures/"+url.PathEscape(id)+"/export", req)
}

// DeleteCaptureSession deletes a finished capture and its records.
func (c *Client) DeleteCaptureSession(ctx context.Context, id string) error {
	_, err := captureCall[struct{ Deleted bool }](ctx, c, http.MethodDelete, "/api/v1/captures/"+url.PathEscape(id), nil)
	return err
}
