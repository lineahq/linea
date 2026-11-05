package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type collectorPageInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum entries, default 25, at most 100 and the platform page budget."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from the preceding page; restart paging after a catalog change."`
}

type collectorInput struct {
	CollectorID string `json:"collector_id" jsonschema:"Collector ID returned by linea_list_collectors."`
}

type capturePointsInput struct {
	collectorInput
	collectorPageInput
}

type collectorsResult struct{ platform.CollectorPage }

func (r collectorsResult) summary() string {
	return fmt.Sprintf("Collector discovery: %d collectors; fleet connected %t; more %t.", len(r.Items), r.Enabled, r.NextCursor != "")
}

type collectorResult struct{ platform.Collector }

func (r collectorResult) summary() string {
	return fmt.Sprintf("Collector found: capture enabled %t; generation %d; %d capture points, %d raw.", r.Enabled, r.Generation, r.PointCount, r.RawPointCount)
}

type capturePointsResult struct{ platform.CapturePointPage }

func (r capturePointsResult) summary() string {
	return fmt.Sprintf("Capture points: %d points; more %t.", len(r.Items), r.NextCursor != "")
}

func addCollectorTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_list_collectors", "List Collectors connected to fleet's live capture plugin in bounded pages. Enabled false means the debugger has no fleet connection. Does not start a capture.", func(ctx context.Context, in collectorPageInput) (summarized, error) {
		page, err := service.ListCollectors(ctx, callerOf(ctx), platform.CollectorPageRequest{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, err
		}
		return collectorsResult{*page}, nil
	})
	addTool(server, "linea_get_collector", "Read one fleet-connected Collector's identity and capture availability.", func(ctx context.Context, in collectorInput) (summarized, error) {
		item, err := service.GetCollector(ctx, callerOf(ctx), in.CollectorID)
		if err != nil {
			return nil, err
		}
		return collectorResult{*item}, nil
	})
	addTool(server, "linea_list_capture_points", "List the reported pipeline/point/signal catalog for a Collector. Raw points need fleet's raw permission.", func(ctx context.Context, in capturePointsInput) (summarized, error) {
		page, err := service.ListCapturePoints(ctx, callerOf(ctx), in.CollectorID, platform.CollectorPageRequest{Limit: in.Limit, Cursor: in.Cursor})
		if err != nil {
			return nil, err
		}
		return capturePointsResult{*page}, nil
	})
}
