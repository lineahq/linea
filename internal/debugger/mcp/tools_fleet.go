package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// fleetResult carries fleet's answer as the structured result and a
// server-computed text of counts, states and hex IDs only: names, selectors
// and attributes are fleet user content and stay out of the text.
type fleetResult struct {
	v    any
	text string
}

func (r fleetResult) summary() string              { return r.text }
func (r fleetResult) MarshalJSON() ([]byte, error) { return json.Marshal(r.v) }

// fleetTool registers a fleet tool whose result text is computed from its
// answer.
func fleetTool[In, Out any](server *sdk.Server, name, description string, call func(context.Context, In) (*Out, error), text func(*Out) string) {
	addTool(server, name, description, func(ctx context.Context, in In) (summarized, error) {
		v, err := call(ctx, in)
		if err != nil {
			return nil, err
		}
		return fleetResult{v, text(v)}, nil
	})
}

func more(cursor string) bool { return cursor != "" }

type fleetOverviewInput struct {
	GroupBy string `json:"group_by,omitempty" jsonschema:"Count agents by service.name, service.version, host.name, k8s.namespace.name or k8s.pod.name."`
}

type fleetAgentsInput struct {
	Limit     int    `json:"limit,omitempty" jsonschema:"Maximum entries; the platform bounds the page."`
	Cursor    string `json:"cursor,omitempty" jsonschema:"Opaque cursor from the preceding page."`
	Connected *bool  `json:"connected,omitempty" jsonschema:"Only connected (true) or disconnected (false) agents."`
	Healthy   *bool  `json:"healthy,omitempty" jsonschema:"Only healthy (true) or unhealthy (false) agents."`
	Selector  string `json:"selector,omitempty" jsonschema:"Fleet selector, such as service.name=cart,k8s.namespace.name in (shop)."`
	Sort      string `json:"sort,omitempty" jsonschema:"A promoted attribute to sort by."`
}

type fleetIDInput struct {
	ID string `json:"id" jsonschema:"The fleet ID: 32 lowercase hex characters."`
}

type fleetPageIDInput struct {
	ID     string `json:"id" jsonschema:"The fleet ID: 32 lowercase hex characters."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum entries; the platform bounds the page."`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque cursor from the preceding page."`
}

type fleetDeleteInput struct {
	ID       string `json:"id" jsonschema:"The fleet ID: 32 lowercase hex characters."`
	Revision int64  `json:"revision" jsonschema:"The current revision; a stale one is a conflict."`
}

type fleetConfigUpdateInput struct {
	ID      string                     `json:"id" jsonschema:"The config ID."`
	Request platform.FleetConfigUpdate `json:"request"`
}

type fleetVersionInput struct {
	ID      string `json:"id" jsonschema:"The config ID."`
	Version int    `json:"version" jsonschema:"The version number."`
}

type fleetVersionCreateInput struct {
	ID      string `json:"id" jsonschema:"The config ID."`
	Body    string `json:"body" jsonschema:"Collector YAML, at most 128 KiB; secrets only as ${env:...} or ${file:...} references."`
	Message string `json:"message,omitempty" jsonschema:"What the version changes."`
}

type fleetTargetsInput struct {
	ID      string `json:"id" jsonschema:"The rollout ID."`
	Outcome string `json:"outcome,omitempty" jsonschema:"pending, released, baking, offline, succeeded, failed, unhealthy, timed_out, unmatched or incompatible."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum entries; the platform bounds the page."`
	Cursor  string `json:"cursor,omitempty" jsonschema:"Opaque cursor from the preceding page."`
}

type fleetRolloutActionInput struct {
	ID     string `json:"id" jsonschema:"The rollout ID."`
	Action string `json:"action" jsonschema:"pause, resume, rollback or cancel."`
}

type fleetDeleted struct {
	Deleted bool `json:"deleted"`
}

func page(limit int, cursor string) platform.CollectorPageRequest {
	return platform.CollectorPageRequest{Limit: limit, Cursor: cursor}
}

func agentConfigText(c *platform.FleetAgentConfig) string {
	if c == nil {
		return "config state unknown"
	}
	return fmt.Sprintf("config in sync %t", c.InSync)
}

func addFleetTools(server *sdk.Server, service *platform.Service) {
	fleetTool(server, "linea_get_fleet_overview", "Read fleet's version, whether config management is on, and agent counts, optionally grouped by a promoted attribute. Without a fleet connection enabled is false.",
		func(ctx context.Context, q fleetOverviewInput) (*platform.FleetOverview, error) {
			return service.GetFleetOverview(ctx, callerOf(ctx), q.GroupBy)
		}, func(v *platform.FleetOverview) string {
			if !v.Enabled {
				return "Fleet: not connected."
			}
			return fmt.Sprintf("Fleet: config management %t; %d agents, %d connected.", v.ConfigManagement, v.Total, v.Connected)
		})
	fleetTool(server, "linea_check_fleet_connection", "Contact fleet now and report each step: reaching fleet with the server's credential, the livecapture plugin, and config management. Without a fleet connection configured is false.",
		func(ctx context.Context, _ struct{}) (*platform.FleetConnection, error) {
			return service.CheckFleetConnection(ctx, callerOf(ctx))
		}, func(v *platform.FleetConnection) string {
			if !v.Configured {
				return "Fleet: not configured; start linea-server with --fleet-url and --fleet-token-file."
			}
			text := "Fleet connection:"
			for _, c := range v.Checks {
				text += fmt.Sprintf(" %s %s;", c.Name, c.Status)
			}
			return text
		})
	fleetTool(server, "linea_list_fleet_agents", "List fleet's agents in bounded pages with the Collectors each links to live capture.",
		func(ctx context.Context, q fleetAgentsInput) (*platform.FleetAgentPage, error) {
			return service.ListFleetAgents(ctx, callerOf(ctx), platform.FleetAgentQuery{Limit: q.Limit, Cursor: q.Cursor, Connected: q.Connected, Healthy: q.Healthy, Selector: q.Selector, Sort: q.Sort})
		}, func(v *platform.FleetAgentPage) string {
			return fmt.Sprintf("Fleet agents: %d; capture linked %t; more %t.", len(v.Items), v.CaptureLinked, more(v.NextCursor))
		})
	fleetTool(server, "linea_get_fleet_agent", "Read one agent with its components, live capture links and remote config state.",
		func(ctx context.Context, q fleetIDInput) (*platform.FleetAgentDetail, error) {
			return service.GetFleetAgent(ctx, callerOf(ctx), q.ID)
		}, func(v *platform.FleetAgentDetail) string {
			return fmt.Sprintf("Fleet agent %s: connected %t; %d linked Collectors; %s.", v.ID, v.Connected, len(v.CollectorIDs), agentConfigText(v.Config))
		})
	fleetTool(server, "linea_retry_fleet_agent_config", "Clear the hold fleet places on a desired config the agent failed to apply, so fleet sends it again.",
		func(ctx context.Context, q fleetIDInput) (*platform.FleetAgentConfig, error) {
			return service.RetryFleetAgentConfig(ctx, callerOf(ctx), q.ID)
		}, func(v *platform.FleetAgentConfig) string { return "Agent config retried: " + agentConfigText(v) + "." })
	fleetTool(server, "linea_list_fleet_events", "List fleet's redacted change events in commit order after the cursor, with acting_for naming the debugger user a delegated change was made for; pass next_cursor to continue.",
		func(ctx context.Context, q collectorPageInput) (*platform.FleetEventPage, error) {
			return service.ListFleetEvents(ctx, callerOf(ctx), page(q.Limit, q.Cursor))
		}, func(v *platform.FleetEventPage) string {
			return fmt.Sprintf("Fleet events: %d after the cursor.", len(v.Items))
		})
	fleetTool(server, "linea_save_collector_group", "Create a fleet group (no id; revision 0) or replace one at its current revision. A group lists agent_ids or has a selector, not both.",
		func(ctx context.Context, q platform.CollectorGroupSave) (*platform.CollectorGroup, error) {
			return service.SaveCollectorGroup(ctx, callerOf(ctx), q)
		}, func(v *platform.CollectorGroup) string { return collectorGroupResult{*v}.summary() })
	fleetTool(server, "linea_delete_collector_group", "Delete a fleet group at its current revision.",
		func(ctx context.Context, q fleetDeleteInput) (*fleetDeleted, error) {
			return &fleetDeleted{true}, service.DeleteCollectorGroup(ctx, callerOf(ctx), q.ID, platform.CollectorGroupDelete{Revision: q.Revision})
		}, func(*fleetDeleted) string { return "Fleet group deleted." })

	fleetTool(server, "linea_list_fleet_configs", "List fleet's configs in bounded pages.",
		func(ctx context.Context, q collectorPageInput) (*platform.FleetConfigPage, error) {
			return service.ListFleetConfigs(ctx, callerOf(ctx), page(q.Limit, q.Cursor))
		}, func(v *platform.FleetConfigPage) string {
			return fmt.Sprintf("Fleet configs: %d; more %t.", len(v.Items), more(v.NextCursor))
		})
	configText := func(v *platform.FleetConfig) string {
		return fmt.Sprintf("Fleet config %s: revision %d; latest version %d.", v.ID, v.Revision, v.LatestVersion)
	}
	fleetTool(server, "linea_get_fleet_config", "Read a fleet config's metadata.",
		func(ctx context.Context, q fleetIDInput) (*platform.FleetConfig, error) {
			return service.GetFleetConfig(ctx, callerOf(ctx), q.ID)
		}, configText)
	fleetTool(server, "linea_create_fleet_config", "Create an empty fleet config named with lowercase letters, digits and hyphens; its versions follow.",
		func(ctx context.Context, q platform.FleetConfigCreate) (*platform.FleetConfig, error) {
			return service.CreateFleetConfig(ctx, callerOf(ctx), q)
		}, configText)
	fleetTool(server, "linea_update_fleet_config", "Rename or redescribe a fleet config at its current revision.",
		func(ctx context.Context, q fleetConfigUpdateInput) (*platform.FleetConfig, error) {
			return service.UpdateFleetConfig(ctx, callerOf(ctx), q.ID, q.Request)
		}, configText)
	fleetTool(server, "linea_delete_fleet_config", "Delete a fleet config and its versions at its current revision; fleet refuses while an assignment uses it.",
		func(ctx context.Context, q fleetDeleteInput) (*fleetDeleted, error) {
			return &fleetDeleted{true}, service.DeleteFleetConfig(ctx, callerOf(ctx), q.ID, platform.FleetConfigDelete{Revision: q.Revision})
		}, func(*fleetDeleted) string { return "Fleet config deleted." })
	fleetTool(server, "linea_list_fleet_config_versions", "List a fleet config's versions newest first, without bodies.",
		func(ctx context.Context, q fleetPageIDInput) (*platform.FleetConfigVersionPage, error) {
			return service.ListFleetConfigVersions(ctx, callerOf(ctx), q.ID, page(q.Limit, q.Cursor))
		}, func(v *platform.FleetConfigVersionPage) string {
			return fmt.Sprintf("Config versions: %d; more %t.", len(v.Items), more(v.NextCursor))
		})
	versionText := func(v *platform.FleetConfigVersion) string {
		return fmt.Sprintf("Config version %d: %d bytes.", v.Version, v.Size)
	}
	fleetTool(server, "linea_get_fleet_config_version", "Read one fleet config version with its Collector YAML.",
		func(ctx context.Context, q fleetVersionInput) (*platform.FleetConfigVersion, error) {
			return service.GetFleetConfigVersion(ctx, callerOf(ctx), q.ID, q.Version)
		}, versionText)
	fleetTool(server, "linea_create_fleet_config_version", "Store a fleet config's next immutable version. Fleet refuses inline secrets.",
		func(ctx context.Context, q fleetVersionCreateInput) (*platform.FleetConfigVersion, error) {
			return service.CreateFleetConfigVersion(ctx, callerOf(ctx), q.ID, platform.FleetConfigVersionCreate{Body: q.Body, Message: q.Message})
		}, versionText)

	fleetTool(server, "linea_list_fleet_assignments", "List fleet's config assignments in bounded pages.",
		func(ctx context.Context, q collectorPageInput) (*platform.FleetAssignmentPage, error) {
			return service.ListFleetAssignments(ctx, callerOf(ctx), page(q.Limit, q.Cursor))
		}, func(v *platform.FleetAssignmentPage) string {
			return fmt.Sprintf("Config assignments: %d; more %t.", len(v.Items), more(v.NextCursor))
		})
	assignmentText := func(v *platform.FleetAssignment) string {
		rollout := "none"
		if v.RolloutID != "" {
			rollout = v.RolloutID
		}
		return fmt.Sprintf("Config assignment %s: version %d at layer %d; rollout %s.", v.ID, v.Version, v.Layer, rollout)
	}
	fleetTool(server, "linea_get_fleet_assignment", "Read a config assignment.",
		func(ctx context.Context, q fleetIDInput) (*platform.FleetAssignment, error) {
			return service.GetFleetAssignment(ctx, callerOf(ctx), q.ID)
		}, assignmentText)
	fleetTool(server, "linea_create_fleet_assignment", "Assign a config version to a group or a selector at a layer and start the rollout that introduces it.",
		func(ctx context.Context, q platform.FleetAssignmentCreate) (*platform.FleetAssignment, error) {
			return service.CreateFleetAssignment(ctx, callerOf(ctx), q)
		}, assignmentText)
	fleetTool(server, "linea_preview_fleet_assignment", "List the agents an assignment targets now and the layers each would receive.",
		func(ctx context.Context, q fleetPageIDInput) (*platform.FleetAssignmentPreviewPage, error) {
			return service.PreviewFleetAssignment(ctx, callerOf(ctx), q.ID, page(q.Limit, q.Cursor))
		}, func(v *platform.FleetAssignmentPreviewPage) string {
			return fmt.Sprintf("Assignment preview: %d agents; more %t.", len(v.Items), more(v.NextCursor))
		})

	fleetTool(server, "linea_list_fleet_rollouts", "List the active and recently finished rollouts.",
		func(ctx context.Context, q collectorPageInput) (*platform.FleetRolloutPage, error) {
			return service.ListFleetRollouts(ctx, callerOf(ctx), page(q.Limit, q.Cursor))
		}, func(v *platform.FleetRolloutPage) string {
			return fmt.Sprintf("Rollouts: %d; more %t.", len(v.Items), more(v.NextCursor))
		})
	rolloutText := func(v *platform.FleetRollout) string {
		// CurrentWave counts from 0 and is -1 before the first release.
		return fmt.Sprintf("Rollout %s: state %s; %d of %d waves released; %d targets.", v.ID, v.State, max(v.CurrentWave+1, 0), v.WaveCount, v.Targets)
	}
	fleetTool(server, "linea_get_fleet_rollout", "Read a rollout with its waves' progress.",
		func(ctx context.Context, q fleetIDInput) (*platform.FleetRollout, error) {
			return service.GetFleetRollout(ctx, callerOf(ctx), q.ID)
		}, rolloutText)
	fleetTool(server, "linea_start_fleet_rollout", "Move an assignment to another version (to_version), or remove it (delete), in waves.",
		func(ctx context.Context, q platform.FleetRolloutStart) (*platform.FleetRollout, error) {
			return service.StartFleetRollout(ctx, callerOf(ctx), q)
		}, rolloutText)
	fleetTool(server, "linea_control_fleet_rollout", "Pause, resume, roll back or cancel a rollout; resuming accepts the failures that paused it. Roll back and cancel both return released agents to the version they had before: roll back ends when they report it, cancel ends at once.",
		func(ctx context.Context, q fleetRolloutActionInput) (*platform.FleetRollout, error) {
			return service.ControlFleetRollout(ctx, callerOf(ctx), q.ID, platform.FleetRolloutAction{Action: q.Action})
		}, rolloutText)
	fleetTool(server, "linea_list_fleet_rollout_targets", "List a rollout's targets, optionally of one outcome.",
		func(ctx context.Context, q fleetTargetsInput) (*platform.FleetRolloutTargetPage, error) {
			return service.ListFleetRolloutTargets(ctx, callerOf(ctx), q.ID, platform.FleetRolloutTargetQuery{Limit: q.Limit, Cursor: q.Cursor, Outcome: q.Outcome})
		}, func(v *platform.FleetRolloutTargetPage) string {
			return fmt.Sprintf("Rollout targets: %d; more %t.", len(v.Items), more(v.NextCursor))
		})
}
