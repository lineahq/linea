package platform

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/authz"
	"github.com/lineahq/linea/internal/debugger/fleetcapture"
)

// Fleet config management, on a fleet started with
// --config-management: configs hold immutable versions of Collector YAML,
// assignments layer a config version onto a group or selector, and rollouts
// move an assignment to another version in waves. Assignments change only
// through rollouts.

// FleetConfig is a named container of immutable, numbered config versions.
type FleetConfig struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Revision      int64     `json:"revision"`
	LatestVersion int       `json:"latest_version"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
}

// FleetConfigPage is a page of configs in ID order.
type FleetConfigPage struct {
	Items      []FleetConfig `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// FleetConfigCreate names a new config.
type FleetConfigCreate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// FleetConfigUpdate renames or redescribes a config at Revision; a nil field
// is unchanged.
type FleetConfigUpdate struct {
	Revision    int64   `json:"revision"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

// FleetConfigDelete deletes a config at Revision.
type FleetConfigDelete struct {
	Revision int64 `json:"revision"`
}

// FleetConfigVersion is one stored version. Body is Collector YAML, returned
// only by a read of the single version.
type FleetConfigVersion struct {
	ConfigID  string    `json:"config_id"`
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256"`
	Size      int       `json:"size"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	Body      string    `json:"body,omitempty"`
}

// FleetConfigVersionPage is a page of versions, newest first.
type FleetConfigVersionPage struct {
	Items      []FleetConfigVersion `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// FleetConfigVersionCreate is the next version's Collector YAML: at most
// 128 KiB, with secrets as ${env:...} or ${file:...} references.
type FleetConfigVersionCreate struct {
	Body    string `json:"body"`
	Message string `json:"message,omitempty"`
}

// FleetAssignment layers a config version onto the agents of a group or a
// selector, narrowed by credential labels. Version is absent while the
// rollout creating the assignment runs; RolloutID names its active rollout.
type FleetAssignment struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	ConfigID         string            `json:"config_id"`
	Version          int               `json:"version,omitempty"`
	Layer            int               `json:"layer"`
	GroupID          string            `json:"group_id,omitempty"`
	Selector         string            `json:"selector,omitempty"`
	CredentialLabels map[string]string `json:"credential_labels,omitempty"`
	Revision         int64             `json:"revision"`
	CreatedAt        time.Time         `json:"created_at"`
	CreatedBy        string            `json:"created_by"`
	RolloutID        string            `json:"rollout_id,omitempty"`
}

// FleetAssignmentPage is a page of assignments in ID order.
type FleetAssignmentPage struct {
	Items      []FleetAssignment `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

// FleetAssignmentCreate stores an assignment and starts the rollout that
// introduces it. Exactly one of GroupID or Selector is set.
type FleetAssignmentCreate struct {
	Name             string              `json:"name"`
	ConfigID         string              `json:"config_id"`
	Version          int                 `json:"version"`
	Layer            int                 `json:"layer"`
	GroupID          string              `json:"group_id,omitempty"`
	Selector         string              `json:"selector,omitempty"`
	CredentialLabels map[string]string   `json:"credential_labels,omitempty"`
	Rollout          *FleetRolloutPolicy `json:"rollout,omitempty"`
}

// FleetAssignmentPreview is an agent the assignment targets now, with the
// layers it would receive from every assignment it matches.
type FleetAssignmentPreview struct {
	AgentID string              `json:"agent_id"`
	Desired *FleetDesiredConfig `json:"desired"`
	Error   string              `json:"error,omitempty"`
}

// FleetAssignmentPreviewPage is a page of previewed agents; agents excluded
// by credential labels are skipped, so a page may be short.
type FleetAssignmentPreviewPage struct {
	Items      []FleetAssignmentPreview `json:"items"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// FleetRolloutWave is a cumulative wave boundary: Count targets or Percent of
// them released by its end.
type FleetRolloutWave struct {
	Count   *int `json:"count,omitempty"`
	Percent *int `json:"percent,omitempty"`
}

// FleetRolloutPolicy is how a rollout releases targets and when it stops.
// Omitted fields take fleet's defaults: waves 1, 5%, 25%, 100%; bake 120 s;
// wave timeout 900 s; 5% failures; pause on failure.
type FleetRolloutPolicy struct {
	Waves              []FleetRolloutWave `json:"waves,omitempty"`
	CanaryGroupID      string             `json:"canary_group_id,omitempty"`
	BakeSeconds        *int               `json:"bake_seconds,omitempty"`
	WaveTimeoutSeconds *int               `json:"wave_timeout_seconds,omitempty"`
	MaxFailures        *int               `json:"max_failures,omitempty"`
	MaxFailurePercent  *int               `json:"max_failure_percent,omitempty"`
	OnFailure          string             `json:"on_failure,omitempty"`
}

// FleetRolloutStart moves an assignment to ToVersion, or removes it with
// Delete, in waves.
type FleetRolloutStart struct {
	AssignmentID string             `json:"assignment_id"`
	ToVersion    int                `json:"to_version,omitempty"`
	Delete       bool               `json:"delete,omitempty"`
	Policy       FleetRolloutPolicy `json:"policy"`
}

// FleetRollout is a rollout and, on a single read, its waves' progress.
// CurrentWave is -1 before the first wave is released.
type FleetRollout struct {
	ID           string                     `json:"id"`
	AssignmentID string                     `json:"assignment_id"`
	ConfigID     string                     `json:"config_id"`
	FromVersion  int                        `json:"from_version,omitempty"`
	ToVersion    int                        `json:"to_version,omitempty"`
	State        string                     `json:"state"`
	CurrentWave  int                        `json:"current_wave"`
	WaveCount    int                        `json:"wave_count"`
	Reason       string                     `json:"reason,omitempty"`
	Policy       FleetRolloutPolicy         `json:"policy"`
	Targets      int                        `json:"targets"`
	Waves        []FleetRolloutWaveProgress `json:"waves,omitempty"`
	CreatedAt    time.Time                  `json:"created_at"`
	CreatedBy    string                     `json:"created_by"`
	UpdatedAt    time.Time                  `json:"updated_at"`
}

// FleetRolloutWaveProgress counts one wave's targets by outcome.
type FleetRolloutWaveProgress struct {
	Wave       int            `json:"wave"`
	Targets    int            `json:"targets"`
	ReleasedAt *time.Time     `json:"released_at,omitempty"`
	Outcomes   map[string]int `json:"outcomes"`
}

// FleetRolloutPage is a page of rollouts: the active ones and the newest
// finished ones, in ID order.
type FleetRolloutPage struct {
	Items      []FleetRollout `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// FleetRolloutTarget is one agent of a rollout and its outcome.
type FleetRolloutTarget struct {
	AgentID    string     `json:"agent_id"`
	Wave       int        `json:"wave"`
	Outcome    string     `json:"outcome"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	Accepted   bool       `json:"accepted"`
}

// FleetRolloutTargetQuery pages a rollout's targets, optionally of one outcome.
type FleetRolloutTargetQuery struct {
	Limit   int    `json:"limit,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// FleetRolloutTargetPage is a page of a rollout's targets.
type FleetRolloutTargetPage struct {
	Items      []FleetRolloutTarget `json:"items"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// FleetRolloutAction is pause, resume, rollback or cancel.
type FleetRolloutAction struct {
	Action string `json:"action"`
}

// rolloutOutcomes are fleet's target outcomes.
var rolloutOutcomes = []string{"pending", "released", "baking", "offline", "succeeded", "failed", "unhealthy", "timed_out", "unmatched", "incompatible"}

var fleetName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// fleetPage is fleet's page envelope; next_after becomes the cursor.
type fleetPage[T any] struct {
	Items     []T    `json:"items"`
	NextAfter string `json:"next_after"`
}

// listFleet reads one page of a fleet collection.
func listFleet[T any](s *Service, ctx context.Context, caller Caller, perm authz.Permission, path, what string, req CollectorPageRequest, extra url.Values) ([]T, string, error) {
	if err := s.fleetBegin(ctx, caller, perm); err != nil {
		return nil, "", err
	}
	q, err := s.fleetLimit(req, what)
	if err != nil {
		return nil, "", err
	}
	for k, v := range extra {
		q[k] = v
	}
	var got fleetPage[T]
	if err := s.manage(ctx, caller, http.MethodGet, path, q, fleetcapture.NoRevision, nil, &got); err != nil {
		return nil, "", err
	}
	if got.Items == nil {
		got.Items = []T{}
	}
	return got.Items, got.NextAfter, nil
}

// readFleet reads one fleet resource by its ID.
func readFleet[T any](s *Service, ctx context.Context, caller Caller, perm authz.Permission, path, id, what string) (*T, error) {
	if err := s.fleetBegin(ctx, caller, perm); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound(what)
	}
	var v T
	if err := s.manage(ctx, caller, http.MethodGet, path+id, nil, fleetcapture.NoRevision, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ListFleetConfigs returns a page of fleet's configs.
func (s *Service) ListFleetConfigs(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *FleetConfigPage, err error) {
	defer s.audit(&caller, "ListFleetConfigs", &err)()
	items, next, err := listFleet[FleetConfig](s, ctx, caller, authz.FleetRead, "/api/v1/configs", "config", req, nil)
	if err != nil {
		return nil, err
	}
	return &FleetConfigPage{Items: items, NextCursor: next}, nil
}

// GetFleetConfig returns a config's metadata.
func (s *Service) GetFleetConfig(ctx context.Context, caller Caller, id string) (_ *FleetConfig, err error) {
	defer s.audit(&caller, "GetFleetConfig", &err)()
	return readFleet[FleetConfig](s, ctx, caller, authz.FleetRead, "/api/v1/configs/", id, "config")
}

// CreateFleetConfig creates an empty config; its first version follows.
func (s *Service) CreateFleetConfig(ctx context.Context, caller Caller, req FleetConfigCreate) (_ *FleetConfig, err error) {
	defer s.audit(&caller, "CreateFleetConfig", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return nil, err
	}
	if !fleetName.MatchString(req.Name) || !collectorText(req.Description, 1024, false) {
		return nil, captureInvalid("a config name is lowercase letters, digits and dashes, at most 63")
	}
	body := struct {
		ID string `json:"id"`
		FleetConfigCreate
	}{newFleetID(), req}
	var v FleetConfig
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/configs", nil, fleetcapture.NoRevision, body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// UpdateFleetConfig renames or redescribes a config at its current revision.
func (s *Service) UpdateFleetConfig(ctx context.Context, caller Caller, id string, req FleetConfigUpdate) (_ *FleetConfig, err error) {
	defer s.audit(&caller, "UpdateFleetConfig", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("config")
	}
	if req.Revision < 1 || req.Name == nil && req.Description == nil {
		return nil, captureInvalid("a config update needs its revision and a change")
	}
	if req.Name != nil && !fleetName.MatchString(*req.Name) || req.Description != nil && !collectorText(*req.Description, 1024, false) {
		return nil, captureInvalid("a config name is lowercase letters, digits and dashes, at most 63")
	}
	body := map[string]string{}
	if req.Name != nil {
		body["name"] = *req.Name
	}
	if req.Description != nil {
		body["description"] = *req.Description
	}
	var v FleetConfig
	if err := s.manage(ctx, caller, http.MethodPatch, "/api/v1/configs/"+id, nil, req.Revision, body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// DeleteFleetConfig deletes a config and its versions at its current
// revision; fleet refuses while an assignment uses it.
func (s *Service) DeleteFleetConfig(ctx context.Context, caller Caller, id string, req FleetConfigDelete) (err error) {
	defer s.audit(&caller, "DeleteFleetConfig", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return err
	}
	if !validFleetID(id) {
		return fleetNotFound("config")
	}
	if req.Revision < 1 {
		return captureInvalid("deleting a config needs its revision")
	}
	return s.manage(ctx, caller, http.MethodDelete, "/api/v1/configs/"+id, nil, req.Revision, nil, nil)
}

// ListFleetConfigVersions returns a page of a config's versions, newest
// first, without bodies.
func (s *Service) ListFleetConfigVersions(ctx context.Context, caller Caller, id string, req CollectorPageRequest) (_ *FleetConfigVersionPage, err error) {
	defer s.audit(&caller, "ListFleetConfigVersions", &err)()
	if !validFleetID(id) {
		if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
			return nil, err
		}
		return nil, fleetNotFound("config")
	}
	if req.Cursor != "" {
		if n, err := strconv.Atoi(req.Cursor); err != nil || n < 1 || strconv.Itoa(n) != req.Cursor {
			return nil, captureInvalid("invalid version cursor")
		}
	}
	items, next, err := listFleet[FleetConfigVersion](s, ctx, caller, authz.FleetRead, "/api/v1/configs/"+id+"/versions", "version", req, nil)
	if err != nil {
		return nil, err
	}
	return &FleetConfigVersionPage{Items: items, NextCursor: next}, nil
}

// GetFleetConfigVersion returns one version with its body, which fleet
// returns to an admin credential only.
func (s *Service) GetFleetConfigVersion(ctx context.Context, caller Caller, id string, version int) (_ *FleetConfigVersion, err error) {
	defer s.audit(&caller, "GetFleetConfigVersion", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	if !validFleetID(id) || version < 1 {
		return nil, fleetNotFound("config version")
	}
	var v FleetConfigVersion
	if err := s.manage(ctx, caller, http.MethodGet, "/api/v1/configs/"+id+"/versions/"+strconv.Itoa(version), nil, fleetcapture.NoRevision, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// CreateFleetConfigVersion stores the config's next immutable version. Fleet
// checks the YAML's shape and refuses inline secrets; the supervisor
// validates it against the real Collector when it is applied.
func (s *Service) CreateFleetConfigVersion(ctx context.Context, caller Caller, id string, req FleetConfigVersionCreate) (_ *FleetConfigVersion, err error) {
	defer s.audit(&caller, "CreateFleetConfigVersion", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("config")
	}
	if req.Body == "" || len(req.Body) > 128<<10 || !collectorText(req.Message, 1024, false) {
		return nil, captureInvalid("a config version is 1 byte to 128 KiB of YAML with a message of at most 1024")
	}
	var v FleetConfigVersion
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/configs/"+id+"/versions", nil, fleetcapture.NoRevision, req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ListFleetAssignments returns a page of config assignments.
func (s *Service) ListFleetAssignments(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *FleetAssignmentPage, err error) {
	defer s.audit(&caller, "ListFleetAssignments", &err)()
	items, next, err := listFleet[FleetAssignment](s, ctx, caller, authz.FleetRead, "/api/v1/config-assignments", "assignment", req, nil)
	if err != nil {
		return nil, err
	}
	return &FleetAssignmentPage{Items: items, NextCursor: next}, nil
}

// GetFleetAssignment returns one config assignment.
func (s *Service) GetFleetAssignment(ctx context.Context, caller Caller, id string) (_ *FleetAssignment, err error) {
	defer s.audit(&caller, "GetFleetAssignment", &err)()
	return readFleet[FleetAssignment](s, ctx, caller, authz.FleetRead, "/api/v1/config-assignments/", id, "assignment")
}

// PreviewFleetAssignment returns a page of the agents an assignment targets
// now and the layers each would receive.
func (s *Service) PreviewFleetAssignment(ctx context.Context, caller Caller, id string, req CollectorPageRequest) (_ *FleetAssignmentPreviewPage, err error) {
	defer s.audit(&caller, "PreviewFleetAssignment", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("assignment")
	}
	q, err := s.fleetLimit(req, "preview")
	if err != nil {
		return nil, err
	}
	var got fleetPage[FleetAssignmentPreview]
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/config-assignments/"+id+":preview", q, fleetcapture.NoRevision, nil, &got); err != nil {
		return nil, err
	}
	if got.Items == nil {
		got.Items = []FleetAssignmentPreview{}
	}
	return &FleetAssignmentPreviewPage{Items: got.Items, NextCursor: got.NextAfter}, nil
}

// CreateFleetAssignment stores an assignment and starts the rollout that
// introduces it; only released targets receive the layer until it completes.
func (s *Service) CreateFleetAssignment(ctx context.Context, caller Caller, req FleetAssignmentCreate) (_ *FleetAssignment, err error) {
	defer s.audit(&caller, "CreateFleetAssignment", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetConfigWrite); err != nil {
		return nil, err
	}
	switch {
	case !fleetName.MatchString(req.Name):
		return nil, captureInvalid("an assignment name is lowercase letters, digits and dashes, at most 63")
	case !validFleetID(req.ConfigID) || req.Version < 1:
		return nil, captureInvalid("an assignment names a config and one of its versions")
	case req.Layer < 0 || req.Layer > 99:
		return nil, captureInvalid("an assignment's layer is 0 to 99")
	case (req.GroupID == "") == (req.Selector == ""):
		return nil, captureInvalid("an assignment targets a group or a selector, not both")
	case req.GroupID != "" && !validFleetID(req.GroupID), !collectorText(req.Selector, 4096, false):
		return nil, captureInvalid("invalid assignment group or selector")
	case len(req.CredentialLabels) > 16:
		return nil, captureInvalid("an assignment requires at most 16 credential labels")
	}
	if err := checkRolloutPolicy(req.Rollout); err != nil {
		return nil, err
	}
	body := struct {
		ID string `json:"id"`
		FleetAssignmentCreate
	}{newFleetID(), req}
	var v FleetAssignment
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/config-assignments", nil, fleetcapture.NoRevision, body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func checkRolloutPolicy(p *FleetRolloutPolicy) error {
	if p == nil {
		return nil
	}
	if len(p.Waves) > 10 || p.CanaryGroupID != "" && !validFleetID(p.CanaryGroupID) {
		return captureInvalid("a rollout has at most 10 waves and a fleet group as canary")
	}
	for _, w := range p.Waves {
		if (w.Count == nil) == (w.Percent == nil) {
			return captureInvalid("each rollout wave sets a count or a percent")
		}
	}
	if p.OnFailure != "" && p.OnFailure != "pause" && p.OnFailure != "rollback" {
		return captureInvalid("on_failure is pause or rollback")
	}
	return nil
}

// ListFleetRollouts returns a page of rollouts without wave progress.
func (s *Service) ListFleetRollouts(ctx context.Context, caller Caller, req CollectorPageRequest) (_ *FleetRolloutPage, err error) {
	defer s.audit(&caller, "ListFleetRollouts", &err)()
	items, next, err := listFleet[FleetRollout](s, ctx, caller, authz.FleetRead, "/api/v1/rollouts", "rollout", req, nil)
	if err != nil {
		return nil, err
	}
	return &FleetRolloutPage{Items: items, NextCursor: next}, nil
}

// GetFleetRollout returns a rollout with its waves' progress.
func (s *Service) GetFleetRollout(ctx context.Context, caller Caller, id string) (_ *FleetRollout, err error) {
	defer s.audit(&caller, "GetFleetRollout", &err)()
	return readFleet[FleetRollout](s, ctx, caller, authz.FleetRead, "/api/v1/rollouts/", id, "rollout")
}

// ListFleetRolloutTargets returns a page of a rollout's targets.
func (s *Service) ListFleetRolloutTargets(ctx context.Context, caller Caller, id string, req FleetRolloutTargetQuery) (_ *FleetRolloutTargetPage, err error) {
	defer s.audit(&caller, "ListFleetRolloutTargets", &err)()
	if !validFleetID(id) {
		if err := s.fleetBegin(ctx, caller, authz.FleetRead); err != nil {
			return nil, err
		}
		return nil, fleetNotFound("rollout")
	}
	var extra url.Values
	if req.Outcome != "" {
		if !slices.Contains(rolloutOutcomes, req.Outcome) {
			return nil, captureInvalid("invalid rollout outcome")
		}
		extra = url.Values{"outcome": {req.Outcome}}
	}
	items, next, err := listFleet[FleetRolloutTarget](s, ctx, caller, authz.FleetRead, "/api/v1/rollouts/"+id+"/targets", "target", CollectorPageRequest{Limit: req.Limit, Cursor: req.Cursor}, extra)
	if err != nil {
		return nil, err
	}
	return &FleetRolloutTargetPage{Items: items, NextCursor: next}, nil
}

// StartFleetRollout moves an assignment to another version, or removes it,
// in waves. Fleet refuses while the assignment or a target is in another
// active rollout.
func (s *Service) StartFleetRollout(ctx context.Context, caller Caller, req FleetRolloutStart) (_ *FleetRollout, err error) {
	defer s.audit(&caller, "StartFleetRollout", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRollout); err != nil {
		return nil, err
	}
	if !validFleetID(req.AssignmentID) {
		return nil, captureInvalid("a rollout names an assignment")
	}
	if req.Delete == (req.ToVersion > 0) || req.ToVersion < 0 {
		return nil, captureInvalid("a rollout moves to a version or deletes the assignment")
	}
	if err := checkRolloutPolicy(&req.Policy); err != nil {
		return nil, err
	}
	body := struct {
		AssignmentID string `json:"assignment_id"`
		ToVersion    int    `json:"to_version,omitempty"`
		Delete       bool   `json:"delete,omitempty"`
		FleetRolloutPolicy
	}{req.AssignmentID, req.ToVersion, req.Delete, req.Policy}
	var v FleetRollout
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/rollouts", nil, fleetcapture.NoRevision, body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ControlFleetRollout pauses, resumes, rolls back or cancels a rollout.
// Resuming accepts the failures that paused it.
func (s *Service) ControlFleetRollout(ctx context.Context, caller Caller, id string, req FleetRolloutAction) (_ *FleetRollout, err error) {
	defer s.audit(&caller, "ControlFleetRollout", &err)()
	if err := s.fleetBegin(ctx, caller, authz.FleetRollout); err != nil {
		return nil, err
	}
	if !validFleetID(id) {
		return nil, fleetNotFound("rollout")
	}
	switch req.Action {
	case "pause", "resume", "rollback", "cancel":
	default:
		return nil, captureInvalid("a rollout action is pause, resume, rollback or cancel")
	}
	var v FleetRollout
	if err := s.manage(ctx, caller, http.MethodPost, "/api/v1/rollouts/"+id+":"+req.Action, nil, fleetcapture.NoRevision, nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}
