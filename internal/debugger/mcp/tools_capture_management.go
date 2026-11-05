package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type collectorGroupResult struct{ platform.CollectorGroup }

func (r collectorGroupResult) summary() string {
	return fmt.Sprintf("Collector group %s: %s, revision %d.", r.ID, r.Kind, r.Revision)
}

type collectorGroupsResult struct{ platform.CollectorGroupPage }

func (r collectorGroupsResult) summary() string {
	return fmt.Sprintf("Collector groups: %d; more %t.", len(r.Items), r.NextCursor != "")
}

type collectorGroupMembersResult struct {
	platform.CollectorGroupMembersPage
}

func (r collectorGroupMembersResult) summary() string {
	return fmt.Sprintf("Group members: %d agents; more %t.", len(r.Items), r.NextCursor != "")
}

type collectorGroupIDInput struct {
	ID string `json:"id"`
}

type collectorGroupMembersInput struct {
	ID     string `json:"id"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type captureMetadataInput struct {
	SessionID string                         `json:"session_id"`
	Request   platform.CaptureMetadataUpdate `json:"request"`
}

func addCaptureManagementTools(server *sdk.Server, service *platform.Service) {
	addTool(server, "linea_get_collector_group", "Read a fleet group: listed agents or an agent selector. Groups are managed in fleet.", func(ctx context.Context, q collectorGroupIDInput) (summarized, error) {
		v, err := service.GetCollectorGroup(ctx, callerOf(ctx), q.ID)
		if err != nil {
			return nil, err
		}
		return collectorGroupResult{*v}, nil
	})
	addTool(server, "linea_list_collector_groups", "List fleet groups in bounded pages.", func(ctx context.Context, q collectorPageInput) (summarized, error) {
		v, err := service.ListCollectorGroups(ctx, callerOf(ctx), platform.CollectorPageRequest{Limit: q.Limit, Cursor: q.Cursor})
		if err != nil {
			return nil, err
		}
		return collectorGroupsResult{*v}, nil
	})
	addTool(server, "linea_list_collector_group_members", "List a fleet group's agents in bounded pages with the Collectors each links to live capture.", func(ctx context.Context, q collectorGroupMembersInput) (summarized, error) {
		v, err := service.ListCollectorGroupMembers(ctx, callerOf(ctx), q.ID, platform.CollectorPageRequest{Limit: q.Limit, Cursor: q.Cursor})
		if err != nil {
			return nil, err
		}
		return collectorGroupMembersResult{*v}, nil
	})
	addTool(server, "linea_update_capture_metadata", "Edit only capture name and description at the expected revision. Recorded data, selection, source provenance and retention stay immutable.", func(ctx context.Context, q captureMetadataInput) (summarized, error) {
		v, err := service.UpdateCaptureMetadata(ctx, callerOf(ctx), q.SessionID, q.Request)
		if err != nil {
			return nil, err
		}
		return captureSessionResult{*v}, nil
	})
}
