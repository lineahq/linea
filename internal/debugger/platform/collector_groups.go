package platform

import (
	"context"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// CollectorGroup is a fleet group: listed agents ("static") or an agent
// selector ("selector"). Groups are managed in fleet; the debugger reads them
// to choose capture targets.
type CollectorGroup struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Revision    int64    `json:"revision"`
	MemberCount *int     `json:"member_count,omitempty"`
	AgentIDs    []string `json:"agent_ids,omitempty"`
	Selector    string   `json:"selector,omitempty"`
}

// CollectorGroupPage is a page of fleet groups.
type CollectorGroupPage struct {
	Items      []CollectorGroup `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// CollectorGroupMember is a fleet agent in a group and the Collectors it
// links to the livecapture plugin.
type CollectorGroupMember struct {
	AgentID      string   `json:"agent_id"`
	Connected    bool     `json:"connected"`
	CollectorIDs []string `json:"collector_ids"`
}

// CollectorGroupMembersPage is a page of a group's agents.
// CaptureLinked is false when live capture's peers cannot be read, such as
// on a fleet without the livecapture plugin; members then list no
// Collectors.
type CollectorGroupMembersPage struct {
	GroupID       string                 `json:"group_id"`
	Items         []CollectorGroupMember `json:"items"`
	NextCursor    string                 `json:"next_cursor,omitempty"`
	CaptureLinked bool                   `json:"capture_linked"`
}

func fleetGroup(g fleetcapture.Group) CollectorGroup {
	return CollectorGroup{ID: g.ID, Name: g.Name, Kind: g.Kind, Revision: g.Revision, MemberCount: g.MemberCount, AgentIDs: g.AgentIDs, Selector: g.Selector}
}

// groupPage checks a page request; fleet's cursor passes through opaquely.
func (s *Service) groupPage(ctx context.Context, caller Caller, perm authz.Permission, req CollectorPageRequest) (int, error) {
	if err := s.begin(ctx, caller, perm); err != nil {
		return 0, err
	}
	if err := s.collectors.checkFleet(); err != nil {
		return 0, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = min(25, s.budgets.PageItems)
	}
	if limit < 1 || limit > min(100, s.budgets.PageItems) {
		return 0, captureInvalid("invalid group page limit")
	}
	if !collectorText(req.Cursor, 256, false) {
		return 0, captureInvalid("invalid group cursor")
	}
	return limit, nil
}

// ListCollectorGroups returns a page of fleet groups.
func (s *Service) ListCollectorGroups(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *CollectorGroupPage, err error) {
	defer s.audit(&caller, "ListCollectorGroups", &err)()
	limit, err := s.groupPage(ctx, caller, authz.FleetRead, req)
	if err != nil {
		return nil, err
	}
	got, err := s.collectors.fleet.Groups(ctx, req.Cursor, limit)
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	page := &CollectorGroupPage{Items: []CollectorGroup{}, NextCursor: got.NextAfter}
	for _, g := range got.Items {
		page.Items = append(page.Items, fleetGroup(g))
	}
	return page, nil
}

// GetCollectorGroup returns a fleet group with its listed agents or selector.
func (s *Service) GetCollectorGroup(ctx context.Context, caller Caller, id string) (_ *CollectorGroup, err error) {
	defer s.audit(&caller, "GetCollectorGroup", &err)()
	if err := s.begin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	if err := s.collectors.checkFleet(); err != nil {
		return nil, err
	}
	if !collectorText(id, 128, true) {
		return nil, &Error{Code: CodeNotFound, Message: "group not found"}
	}
	g, err := s.collectors.fleet.Group(ctx, id)
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	v := fleetGroup(*g)
	return &v, nil
}

// ListCollectorGroupMembers returns a page of a fleet group's agents and the
// connected Collectors each one links.
func (s *Service) ListCollectorGroupMembers(ctx context.Context, caller Caller, id string, req CollectorPageRequest) (_ *CollectorGroupMembersPage, err error) {
	defer s.audit(&caller, "ListCollectorGroupMembers", &err)()
	limit, err := s.groupPage(ctx, caller, authz.FleetRead, req)
	if err != nil {
		return nil, err
	}
	if !collectorText(id, 128, true) {
		return nil, &Error{Code: CodeNotFound, Message: "group not found"}
	}
	r := s.collectors
	got, err := r.fleet.GroupAgents(ctx, id, req.Cursor, limit)
	if err != nil {
		return nil, fleetError(ctx, err)
	}
	linked, ok := s.captureLinks(ctx)
	page := &CollectorGroupMembersPage{GroupID: id, Items: []CollectorGroupMember{}, NextCursor: got.NextAfter, CaptureLinked: ok}
	for _, a := range got.Items {
		ids := linked[a.ID]
		if ids == nil {
			ids = []string{}
		}
		page.Items = append(page.Items, CollectorGroupMember{AgentID: a.ID, Connected: a.Connected, CollectorIDs: ids})
	}
	return page, nil
}
