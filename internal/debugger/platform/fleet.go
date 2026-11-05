package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// Fleet management: the debugger is fleet's management UI. These
// operations read fleet's inventory, groups and events and change groups;
// fleet_configs.go covers config management. Every call goes to fleet with
// linea-server's fleet credential, and fleet's role check is the only gate on
// writes. Credentials and plugin lifecycle stay in fleet's own API.

// FleetOverview is fleet's server identity and agent counts. Enabled reports
// whether linea-server is connected to fleet; the rest is empty without it.
type FleetOverview struct {
	Enabled          bool                `json:"enabled"`
	Endpoint         string              `json:"endpoint,omitempty"`
	Version          string              `json:"version,omitempty"`
	Storage          string              `json:"storage,omitempty"`
	ConfigManagement bool                `json:"config_management"`
	Total            int                 `json:"total"`
	Connected        int                 `json:"connected"`
	Disconnected     int                 `json:"disconnected"`
	Groups           []FleetSummaryGroup `json:"groups,omitempty"`
}

// FleetSummaryGroup counts the agents sharing one value of the grouping
// attribute; "" collects agents without it.
type FleetSummaryGroup struct {
	Value     string `json:"value"`
	Total     int    `json:"total"`
	Connected int    `json:"connected"`
}

// FleetAgent is a fleet agent: an OpAMP client, usually a supervised
// Collector. CollectorIDs are the connected Collectors it links to live
// capture; a capturable agent has at least one.
type FleetAgent struct {
	ID               string                 `json:"id"`
	Connected        bool                   `json:"connected"`
	FirstSeenAt      time.Time              `json:"first_seen_at"`
	ConnectedAt      time.Time              `json:"connected_at"`
	DisconnectedAt   *time.Time             `json:"disconnected_at,omitempty"`
	LastSeenAt       *time.Time             `json:"last_seen_at,omitempty"`
	Health           *FleetAgentHealth      `json:"health,omitempty"`
	IdentityConflict bool                   `json:"identity_conflict"`
	CredentialID     string                 `json:"credential_id,omitempty"`
	Attributes       map[string]string      `json:"attributes"`
	Components       []FleetAgentComponent  `json:"components,omitempty"`
	ConfigHash       string                 `json:"config_hash,omitempty"`
	RemoteConfig     *FleetRemoteConfigInfo `json:"remote_config,omitempty"`
	CollectorIDs     []string               `json:"collector_ids"`
}

// FleetAgentHealth is the agent's last reported health.
type FleetAgentHealth struct {
	Healthy             bool      `json:"healthy"`
	ObservedAt          time.Time `json:"observed_at"`
	Status              string    `json:"status,omitempty"`
	Error               string    `json:"error,omitempty"`
	ComponentsUnhealthy int       `json:"components_unhealthy"`
}

// FleetAgentComponent is a component of the agent's effective configuration
// with its reported health.
type FleetAgentComponent struct {
	ID        string   `json:"id"`
	Pipelines []string `json:"pipelines,omitempty"`
	Healthy   *bool    `json:"healthy,omitempty"`
	Status    string   `json:"status,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// FleetRemoteConfigInfo is the remote config status the agent reported last.
// HeldHash is a desired config fleet withholds after it failed, until a retry.
type FleetRemoteConfigInfo struct {
	Hash       string     `json:"hash,omitempty"`
	Status     string     `json:"status,omitempty"`
	Error      string     `json:"error,omitempty"`
	HeldHash   string     `json:"held_hash,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// FleetAgentQuery pages fleet's agents. Selector is fleet's selector syntax;
// Sort is a promoted attribute such as service.name.
type FleetAgentQuery struct {
	Limit     int    `json:"limit,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Connected *bool  `json:"connected,omitempty"`
	Healthy   *bool  `json:"healthy,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Sort      string `json:"sort,omitempty"`
}

// FleetAgentPage is a page of agents. CaptureLinked reports whether the
// agents' live capture links were resolved; it is false when fleet runs
// without the livecapture plugin.
type FleetAgentPage struct {
	Items         []FleetAgent `json:"items"`
	NextCursor    string       `json:"next_cursor,omitempty"`
	CaptureLinked bool         `json:"capture_linked"`
}

// FleetAgentDetail is one agent with its config management state, which is
// absent when fleet runs without config management.
type FleetAgentDetail struct {
	FleetAgent
	CaptureLinked bool              `json:"capture_linked"`
	Config        *FleetAgentConfig `json:"config,omitempty"`
}

// FleetAgentConfig is fleet's view of an agent's remote config: the layers it
// sends, what the agent reported and whether the two agree. Remote config
// needs both the credential's permission and the agent's capability.
type FleetAgentConfig struct {
	CredentialRemoteConfig bool                   `json:"credential_remote_config"`
	AcceptsRemoteConfig    bool                   `json:"accepts_remote_config"`
	Desired                *FleetDesiredConfig    `json:"desired"`
	Error                  string                 `json:"error,omitempty"`
	Reported               *FleetRemoteConfigInfo `json:"reported"`
	InSync                 bool                   `json:"in_sync"`
	MissingComponents      []string               `json:"missing_components,omitempty"`
}

// FleetDesiredConfig is the layers fleet sends an agent, in entry name order.
type FleetDesiredConfig struct {
	Layers    []FleetDesiredLayer `json:"layers"`
	Hash      string              `json:"hash"`
	Unmanaged bool                `json:"unmanaged,omitempty"`
}

// FleetDesiredLayer is one config entry, named NN-<assignment name>.
type FleetDesiredLayer struct {
	Name         string `json:"name"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ConfigID     string `json:"config_id,omitempty"`
	Version      int    `json:"version,omitempty"`
	SHA256       string `json:"sha256"`
}

// FleetEvent is one of fleet's redacted change events. Detail never holds
// configuration, attribute values or telemetry. ActingFor names the debugger
// user a delegated call was made for; ActorID is still the credential.
type FleetEvent struct {
	ID         string               `json:"id"`
	Kind       string               `json:"kind"`
	SubjectID  string               `json:"subject_id"`
	ActorID    string               `json:"actor_id"`
	ActingFor  *fleetcapture.Acting `json:"acting_for,omitempty"`
	OccurredAt time.Time            `json:"occurred_at"`
	Detail     json.RawMessage      `json:"detail,omitempty"`
}

// FleetEventPage is a page of events in commit order. NextCursor continues
// after the page and is returned unchanged by an empty page.
type FleetEventPage struct {
	Items      []FleetEvent `json:"items"`
	NextCursor string       `json:"next_cursor"`
}

// CollectorGroupSave creates a fleet group (empty ID) or replaces one at
// Revision. Exactly one of AgentIDs or Selector is set.
type CollectorGroupSave struct {
	ID       string   `json:"id,omitempty"`
	Revision int64    `json:"revision"`
	Name     string   `json:"name"`
	AgentIDs []string `json:"agent_ids,omitempty"`
	Selector string   `json:"selector,omitempty"`
}

// CollectorGroupDelete deletes a fleet group at Revision.
type CollectorGroupDelete struct {
	Revision int64 `json:"revision"`
}

// fleetBegin starts an operation that needs the fleet connection.
func (s *Service) fleetBegin(ctx context.Context, caller Caller, perm authz.Permission) error {
	if err := s.begin(ctx, caller, perm); err != nil {
		return err
	}
	return s.collectors.checkFleet()
}

// fleetLimit checks a page request against fleet's and the platform's bounds.
func (s *Service) fleetLimit(req CollectorPageRequest, what string) (url.Values, error) {
	limit := req.Limit
	if limit == 0 {
		limit = min(50, s.budgets.PageItems)
	}
	if limit < 1 || limit > min(500, s.budgets.PageItems) {
		return nil, captureInvalid("invalid " + what + " page limit")
	}
	if !collectorText(req.Cursor, 1024, false) {
		return nil, captureInvalid("invalid " + what + " cursor")
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if req.Cursor != "" {
		q.Set("after", req.Cursor)
	}
	return q, nil
}

func newFleetID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fleetNotFound(what string) error {
	return &Error{Code: CodeNotFound, Message: what + " not found"}
}

// fleetManager is the part of fleetcapture.Client fleet management uses.
type fleetManager interface {
	Endpoint() string
	Manage(ctx context.Context, method, path string, query url.Values, revision int64, body, out any) error
}

// manage sends one management request and maps fleet's refusal.
func (s *Service) manage(ctx context.Context, caller Caller, method, path string, q url.Values, revision int64, body, out any) error {
	return fleetError(ctx, s.rawManage(ctx, caller, method, path, q, revision, body, out))
}

// rawManage is manage without the error mapping.
func (s *Service) rawManage(ctx context.Context, caller Caller, method, path string, q url.Values, revision int64, body, out any) error {
	m, ok := s.collectors.fleet.(fleetManager)
	if !ok {
		return &Error{Code: CodeUnavailable, Message: "this fleet connection cannot manage fleet"}
	}
	return m.Manage(actingFor(ctx, caller), method, path, q, revision, body, out)
}

// actingFor makes fleet record the caller as the user a call acts for. Fleet
// keeps it on its events and the livecapture plugin on captures; neither
// decides anything on it. The local owner of an in-process platform is no one
// fleet knows, so it sends nothing.
func actingFor(ctx context.Context, caller Caller) context.Context {
	if caller.Principal.Kind == PrincipalLocal {
		return ctx
	}
	return fleetcapture.WithActing(ctx, fleetcapture.Acting{ID: caller.Principal.ID, Name: caller.Principal.Name, RequestID: caller.RequestID})
}

// captureLinks maps agent IDs to the connected Collectors each links to live
// capture. ok is false when the peers cannot be read, such as on a fleet
// without the livecapture plugin.
func (s *Service) captureLinks(ctx context.Context) (map[string][]string, bool) {
	snap, err := s.collectors.snapshot(ctx, false)
	if err != nil {
		return nil, false
	}
	linked := map[string][]string{}
	for _, peer := range snap.ids {
		if agent := snap.collectors[peer].AgentID; agent != "" {
			linked[agent] = append(linked[agent], peer)
		}
	}
	return linked, true
}

func linkAgent(a *FleetAgent, linked map[string][]string) {
	a.CollectorIDs = linked[a.ID]
	if a.CollectorIDs == nil {
		a.CollectorIDs = []string{}
	}
	if a.Attributes == nil {
		a.Attributes = map[string]string{}
	}
}

// GetFleetOverview returns fleet's version, whether config management is on
// and its agent counts, grouped by a promoted attribute when groupBy is set.
// Without a fleet connection it reports Enabled false rather than failing.
func (s *Service) GetFleetOverview(ctx context.Context, caller Caller, groupBy string) (_ *FleetOverview, err error) {
	defer s.audit(&caller, "GetFleetOverview", &err)()
	if err := s.begin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	if s.collectors.checkFleet() != nil {
		return &FleetOverview{}, nil
	}
	if !collectorText(groupBy, 64, false) {
		return nil, captureInvalid("invalid group_by attribute")
	}
	var server struct {
		Version          string `json:"version"`
		Storage          string `json:"storage"`
		ConfigManagement bool   `json:"config_management"`
	}
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/server", nil, fleetcapture.NoRevision, nil, &server); err != nil {
		return nil, err
	}
	var q url.Values
	if groupBy != "" {
		q = url.Values{"group_by": {groupBy}}
	}
	v := FleetOverview{Enabled: true, Version: server.Version, Storage: server.Storage, ConfigManagement: server.ConfigManagement}
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/summary", q, fleetcapture.NoRevision, nil, &v); err != nil {
		return nil, err
	}
	if m, ok := s.collectors.fleet.(fleetManager); ok {
		v.Endpoint = m.Endpoint()
	}
	v.Enabled, v.Version, v.Storage, v.ConfigManagement = true, server.Version, server.Storage, server.ConfigManagement
	return &v, nil
}

// ListFleetAgents returns a page of fleet's agents, each with the Collectors
// it links to live capture.
func (s *Service) ListFleetAgents(ctx context.Context, caller Caller, req FleetAgentQuery) (_ *FleetAgentPage, err error) {
	defer s.audit(&caller, "ListFleetAgents", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	q, err := s.fleetLimit(CollectorPageRequest{Limit: req.Limit, Cursor: req.Cursor}, "agent")
	if err != nil {
		return nil, err
	}
	if !collectorText(req.Selector, 4096, false) || !collectorText(req.Sort, 64, false) {
		return nil, captureInvalid("invalid agent selector or sort")
	}
	if req.Connected != nil {
		q.Set("connected", strconv.FormatBool(*req.Connected))
	}
	if req.Healthy != nil {
		q.Set("healthy", strconv.FormatBool(*req.Healthy))
	}
	if req.Selector != "" {
		q.Set("selector", req.Selector)
	}
	if req.Sort != "" {
		q.Set("sort", req.Sort)
	}
	var got struct {
		Items     []FleetAgent `json:"items"`
		NextAfter string       `json:"next_after"`
	}
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/agents", q, fleetcapture.NoRevision, nil, &got); err != nil {
		return nil, err
	}
	linked, ok := s.captureLinks(ctx)
	page := &FleetAgentPage{Items: []FleetAgent{}, NextCursor: got.NextAfter, CaptureLinked: ok}
	for _, a := range got.Items {
		linkAgent(&a, linked)
		page.Items = append(page.Items, a)
	}
	return page, nil
}

// GetFleetAgent returns one agent with its components, its live capture
// links and, when fleet manages configs, its remote config state.
func (s *Service) GetFleetAgent(ctx context.Context, caller Caller, id string) (_ *FleetAgentDetail, err error) {
	defer s.audit(&caller, "GetFleetAgent", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("agent")
	}
	var v FleetAgentDetail
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/agents/"+id, nil, fleetcapture.NoRevision, nil, &v.FleetAgent); err != nil {
		return nil, err
	}
	linked, ok := s.captureLinks(ctx)
	linkAgent(&v.FleetAgent, linked)
	v.CaptureLinked = ok
	var cfg FleetAgentConfig
	err = s.rawManage(ctx, caller, http.MethodGet, "/api/v1/agents/"+id+"/config", nil, fleetcapture.NoRevision, nil, &cfg)
	switch {
	case err == nil:
		v.Config = &cfg
	case !configManagementOff(err):
		return nil, fleetError(ctx, err)
	}
	return &v, nil
}

// configManagementOff reports fleet's refusal when it runs without
// --config-management.
func configManagementOff(err error) bool {
	var e *fleetcapture.Error
	return errors.As(err, &e) && e.Code == "config_management_disabled"
}

// RetryFleetAgentConfig clears the hold fleet places on a desired config the
// agent failed to apply, so fleet sends it again, and returns the agent's
// config state.
func (s *Service) RetryFleetAgentConfig(ctx context.Context, caller Caller, id string) (_ *FleetAgentConfig, err error) {
	defer s.audit(&caller, "RetryFleetAgentConfig", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRollout); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("agent")
	}
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/agents/"+id+"/config:retry", nil, fleetcapture.NoRevision, nil, nil); err != nil {
		return nil, err
	}
	var cfg FleetAgentConfig
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/agents/"+id+"/config", nil, fleetcapture.NoRevision, nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ListFleetEvents returns a page of fleet's change events after the cursor,
// oldest first; without a cursor it starts at the oldest event fleet keeps.
// An expired cursor is a conflict: restart without one.
func (s *Service) ListFleetEvents(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *FleetEventPage, err error) {
	defer s.audit(&caller, "ListFleetEvents", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	q, err := s.fleetLimit(req, "event")
	if err != nil {
		return nil, err
	}
	if q.Has("after") {
		q.Set("cursor", q.Get("after"))
		q.Del("after")
	}
	page := &FleetEventPage{}
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/events", q, fleetcapture.NoRevision, nil, page); err != nil {
		return nil, err
	}
	if page.Items == nil {
		page.Items = []FleetEvent{}
	}
	return page, nil
}

// SaveCollectorGroup creates a fleet group, when ID is empty, or replaces
// one at its current revision.
func (s *Service) SaveCollectorGroup(ctx context.Context, caller Caller, req CollectorGroupSave) (_ *CollectorGroup, err error) {
	defer s.audit(&caller, "SaveCollectorGroup", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return nil, err
	}
	if req.ID == "" {
		req.ID, req.Revision = newFleetID(), 0
	} else if !validFleetID(req.ID) {
		return nil, fleetNotFound("group")
	} else if req.Revision < 1 {
		return nil, captureInvalid("replacing a group needs its revision")
	}
	if !collectorText(req.Name, 128, true) || !collectorText(req.Selector, 4096, false) {
		return nil, captureInvalid("a group needs a name and a valid selector")
	}
	if (req.Selector == "") == (req.AgentIDs == nil) {
		return nil, captureInvalid("a group lists agents or has a selector, not both")
	}
	if len(req.AgentIDs) > 1000 {
		return nil, captureInvalid("a group lists at most 1000 agents")
	}
	for _, id := range req.AgentIDs {
		if !validFleetID(id) {
			return nil, captureInvalid("group agents are fleet agent IDs")
		}
	}
	body := map[string]any{"name": req.Name}
	if req.Selector != "" {
		body["selector"] = req.Selector
	} else {
		body["agent_ids"] = req.AgentIDs
	}
	var g fleetcapture.Group
	if err := s.manage(ctx, caller, http.MethodPut, "/api/v1/groups/"+req.ID, nil, req.Revision, body, &g); err != nil {
		return nil, err
	}
	v := fleetGroup(g)
	return &v, nil
}

// DeleteCollectorGroup deletes a fleet group at its current revision.
func (s *Service) DeleteCollectorGroup(ctx context.Context, caller Caller, id string, req CollectorGroupDelete) (err error) {
	defer s.audit(&caller, "DeleteCollectorGroup", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return err
	}
	if !validFleetID(id) {
		return fleetNotFound("group")
	}
	if req.Revision < 1 {
		return captureInvalid("deleting a group needs its revision")
	}
	return s.manage(ctx, caller, http.MethodDelete, "/api/v1/groups/"+id, nil, req.Revision, nil, nil)
}
