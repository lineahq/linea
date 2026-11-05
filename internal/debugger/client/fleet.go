package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// Fleet management through the server's fleet connection.

func fleetPath(parts ...string) string {
	p := "/api/v1/fleet"
	for _, s := range parts {
		p += "/" + url.PathEscape(s)
	}
	return p
}

// withQuery appends q to a page query from collectorQuery.
func withQuery(page string, q url.Values) string {
	if len(q) == 0 {
		return page
	}
	if page == "" {
		return "?" + q.Encode()
	}
	return page + "&" + q.Encode()
}

func revisionParam(rev int64) string { return "?revision=" + strconv.FormatInt(rev, 10) }

// GetFleetOverview returns fleet's version and agent counts, grouped by a
// promoted attribute when groupBy is set.
func (c *Client) GetFleetOverview(ctx context.Context, groupBy string) (*platform.FleetOverview, error) {
	path := fleetPath()
	if groupBy != "" {
		path += "?group_by=" + url.QueryEscape(groupBy)
	}
	return captureCall[platform.FleetOverview](ctx, c, http.MethodGet, path, nil)
}

// ListFleetAgents returns a page of fleet's agents.
func (c *Client) ListFleetAgents(ctx context.Context, q platform.FleetAgentQuery) (*platform.FleetAgentPage, error) {
	extra := url.Values{}
	for name, b := range map[string]*bool{"connected": q.Connected, "healthy": q.Healthy} {
		if b != nil {
			extra.Set(name, strconv.FormatBool(*b))
		}
	}
	for name, v := range map[string]string{"selector": q.Selector, "sort": q.Sort} {
		if v != "" {
			extra.Set(name, v)
		}
	}
	path := fleetPath("agents") + withQuery(collectorQuery(platform.CollectorPageRequest{Limit: q.Limit, Cursor: q.Cursor}), extra)
	return captureCall[platform.FleetAgentPage](ctx, c, http.MethodGet, path, nil)
}

// GetFleetAgent returns one agent with its components and config state.
func (c *Client) GetFleetAgent(ctx context.Context, id string) (*platform.FleetAgentDetail, error) {
	return captureCall[platform.FleetAgentDetail](ctx, c, http.MethodGet, fleetPath("agents", id), nil)
}

// RetryFleetAgentConfig asks fleet to resend a desired config the agent
// failed to apply.
func (c *Client) RetryFleetAgentConfig(ctx context.Context, id string) (*platform.FleetAgentConfig, error) {
	return captureCall[platform.FleetAgentConfig](ctx, c, http.MethodPost, fleetPath("agents", id, "config-retry"), nil)
}

// ListFleetEvents returns fleet's change events after the cursor.
func (c *Client) ListFleetEvents(ctx context.Context, q platform.CollectorPageRequest) (*platform.FleetEventPage, error) {
	return captureCall[platform.FleetEventPage](ctx, c, http.MethodGet, fleetPath("events")+collectorQuery(q), nil)
}

// SaveCollectorGroup creates a group when q has no ID, or replaces it at
// q.Revision.
func (c *Client) SaveCollectorGroup(ctx context.Context, q platform.CollectorGroupSave) (*platform.CollectorGroup, error) {
	if q.ID == "" {
		return captureCall[platform.CollectorGroup](ctx, c, http.MethodPost, "/api/v1/collector-groups", q)
	}
	id := q.ID
	q.ID = ""
	return captureCall[platform.CollectorGroup](ctx, c, http.MethodPut, "/api/v1/collector-groups/"+url.PathEscape(id), q)
}

// DeleteCollectorGroup deletes a group at its current revision.
func (c *Client) DeleteCollectorGroup(ctx context.Context, id string, rev int64) error {
	_, err := captureCall[api.FleetDeleted](ctx, c, http.MethodDelete, "/api/v1/collector-groups/"+url.PathEscape(id)+revisionParam(rev), nil)
	return err
}

// ListFleetConfigs returns a page of fleet's configs.
func (c *Client) ListFleetConfigs(ctx context.Context, q platform.CollectorPageRequest) (*platform.FleetConfigPage, error) {
	return captureCall[platform.FleetConfigPage](ctx, c, http.MethodGet, fleetPath("configs")+collectorQuery(q), nil)
}

// GetFleetConfig returns a config's metadata.
func (c *Client) GetFleetConfig(ctx context.Context, id string) (*platform.FleetConfig, error) {
	return captureCall[platform.FleetConfig](ctx, c, http.MethodGet, fleetPath("configs", id), nil)
}

// CreateFleetConfig creates an empty config.
func (c *Client) CreateFleetConfig(ctx context.Context, q platform.FleetConfigCreate) (*platform.FleetConfig, error) {
	return captureCall[platform.FleetConfig](ctx, c, http.MethodPost, fleetPath("configs"), q)
}

// UpdateFleetConfig renames or redescribes a config at q.Revision.
func (c *Client) UpdateFleetConfig(ctx context.Context, id string, q platform.FleetConfigUpdate) (*platform.FleetConfig, error) {
	return captureCall[platform.FleetConfig](ctx, c, http.MethodPatch, fleetPath("configs", id), q)
}

// DeleteFleetConfig deletes a config at its current revision.
func (c *Client) DeleteFleetConfig(ctx context.Context, id string, rev int64) error {
	_, err := captureCall[api.FleetDeleted](ctx, c, http.MethodDelete, fleetPath("configs", id)+revisionParam(rev), nil)
	return err
}

// ListFleetConfigVersions returns a page of a config's versions without
// bodies.
func (c *Client) ListFleetConfigVersions(ctx context.Context, id string, q platform.CollectorPageRequest) (*platform.FleetConfigVersionPage, error) {
	return captureCall[platform.FleetConfigVersionPage](ctx, c, http.MethodGet, fleetPath("configs", id, "versions")+collectorQuery(q), nil)
}

// GetFleetConfigVersion returns one version with its body.
func (c *Client) GetFleetConfigVersion(ctx context.Context, id string, version int) (*platform.FleetConfigVersion, error) {
	return captureCall[platform.FleetConfigVersion](ctx, c, http.MethodGet, fleetPath("configs", id, "versions", strconv.Itoa(version)), nil)
}

// CreateFleetConfigVersion stores a config's next version.
func (c *Client) CreateFleetConfigVersion(ctx context.Context, id string, q platform.FleetConfigVersionCreate) (*platform.FleetConfigVersion, error) {
	return captureCall[platform.FleetConfigVersion](ctx, c, http.MethodPost, fleetPath("configs", id, "versions"), q)
}

// ListFleetAssignments returns a page of config assignments.
func (c *Client) ListFleetAssignments(ctx context.Context, q platform.CollectorPageRequest) (*platform.FleetAssignmentPage, error) {
	return captureCall[platform.FleetAssignmentPage](ctx, c, http.MethodGet, fleetPath("assignments")+collectorQuery(q), nil)
}

// GetFleetAssignment returns a config assignment.
func (c *Client) GetFleetAssignment(ctx context.Context, id string) (*platform.FleetAssignment, error) {
	return captureCall[platform.FleetAssignment](ctx, c, http.MethodGet, fleetPath("assignments", id), nil)
}

// CreateFleetAssignment assigns a config version and starts its rollout.
func (c *Client) CreateFleetAssignment(ctx context.Context, q platform.FleetAssignmentCreate) (*platform.FleetAssignment, error) {
	return captureCall[platform.FleetAssignment](ctx, c, http.MethodPost, fleetPath("assignments"), q)
}

// PreviewFleetAssignment returns a page of the agents an assignment targets.
func (c *Client) PreviewFleetAssignment(ctx context.Context, id string, q platform.CollectorPageRequest) (*platform.FleetAssignmentPreviewPage, error) {
	return captureCall[platform.FleetAssignmentPreviewPage](ctx, c, http.MethodGet, fleetPath("assignments", id, "preview")+collectorQuery(q), nil)
}

// ListFleetRollouts returns a page of rollouts.
func (c *Client) ListFleetRollouts(ctx context.Context, q platform.CollectorPageRequest) (*platform.FleetRolloutPage, error) {
	return captureCall[platform.FleetRolloutPage](ctx, c, http.MethodGet, fleetPath("rollouts")+collectorQuery(q), nil)
}

// GetFleetRollout returns a rollout with its waves.
func (c *Client) GetFleetRollout(ctx context.Context, id string) (*platform.FleetRollout, error) {
	return captureCall[platform.FleetRollout](ctx, c, http.MethodGet, fleetPath("rollouts", id), nil)
}

// ListFleetRolloutTargets returns a page of a rollout's targets.
func (c *Client) ListFleetRolloutTargets(ctx context.Context, id string, q platform.FleetRolloutTargetQuery) (*platform.FleetRolloutTargetPage, error) {
	extra := url.Values{}
	if q.Outcome != "" {
		extra.Set("outcome", q.Outcome)
	}
	path := fleetPath("rollouts", id, "targets") + withQuery(collectorQuery(platform.CollectorPageRequest{Limit: q.Limit, Cursor: q.Cursor}), extra)
	return captureCall[platform.FleetRolloutTargetPage](ctx, c, http.MethodGet, path, nil)
}

// StartFleetRollout moves an assignment to another version, or removes it.
func (c *Client) StartFleetRollout(ctx context.Context, q platform.FleetRolloutStart) (*platform.FleetRollout, error) {
	return captureCall[platform.FleetRollout](ctx, c, http.MethodPost, fleetPath("rollouts"), q)
}

// ControlFleetRollout pauses, resumes, rolls back or cancels a rollout.
func (c *Client) ControlFleetRollout(ctx context.Context, id string, q platform.FleetRolloutAction) (*platform.FleetRollout, error) {
	return captureCall[platform.FleetRollout](ctx, c, http.MethodPost, fleetPath("rollouts", id, "actions"), q)
}
