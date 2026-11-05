package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/configmgmt"
	"github.com/lineahq/linea/internal/fleet/fleet"
)

// maxVersionRequest bounds a version upload: a 128 KiB body survives JSON
// escaping well within it; Validate enforces the body bound itself.
const maxVersionRequest = 1024 * 1024

// SetConfigManagement enables the config management API (M6). Without it the
// routes answer 404 config_management_disabled.
func (h *Handler) SetConfigManagement(repo configmgmt.Repository, allowed []string) {
	h.configs, h.allowedComponents = repo, allowed
	h.info.ConfigManagement = repo != nil
}

// configRoute serves config management paths and reports whether the path
// belonged to it.
func (h *Handler) configRoute(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	path := r.URL.Path
	var serve func()
	switch {
	case path == "/api/v1/configs":
		serve = func() { h.configCollection(w, r, p) }
	case strings.HasPrefix(path, "/api/v1/configs/"):
		serve = func() { h.configItem(w, r, p, strings.TrimPrefix(path, "/api/v1/configs/")) }
	case path == "/api/v1/config-assignments":
		serve = func() { h.assignmentCollection(w, r, p) }
	case strings.HasPrefix(path, "/api/v1/config-assignments/"):
		serve = func() { h.assignmentItem(w, r, p, strings.TrimPrefix(path, "/api/v1/config-assignments/")) }
	case path == "/api/v1/rollouts":
		serve = func() { h.rolloutCollection(w, r, p) }
	case strings.HasPrefix(path, "/api/v1/rollouts/"):
		serve = func() { h.rolloutItem(w, r, p, strings.TrimPrefix(path, "/api/v1/rollouts/")) }
	case strings.HasPrefix(path, "/api/v1/agents/") && (strings.HasSuffix(path, "/config") || strings.HasSuffix(path, "/config:retry")):
		serve = func() { h.agentConfig(w, r, p, strings.TrimPrefix(path, "/api/v1/agents/")) }
	default:
		return false
	}
	if h.configs == nil {
		fail(w, 404, "config_management_disabled", "config management is not enabled on this server")
		return true
	}
	serve()
	return true
}

func etag(w http.ResponseWriter, revision int64) {
	w.Header().Set("ETag", fmt.Sprintf("%q", strconv.FormatInt(revision, 10)))
}

func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		failure(w, errNoQuery)
		return false
	}
	return true
}

func (h *Handler) configCollection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !method(w, r, "GET", "POST") {
		return
	}
	ctx := r.Context()
	if r.Method == "POST" {
		if !admin(w, p) || !noQuery(w, r) {
			return
		}
		var body struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if !decode(w, r, &body) {
			return
		}
		c, err := h.configs.CreateConfig(ctx, p.ID, configmgmt.Config{ID: body.ID, Name: body.Name, Description: body.Description})
		if err != nil {
			failure(w, err)
			return
		}
		w.Header().Set("Location", "/api/v1/configs/"+c.ID)
		etag(w, c.Revision)
		writeJSON(w, 201, c)
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after, ok := afterID(w, q)
	if !ok {
		return
	}
	page, err := h.configs.ListConfigs(ctx, after, limit)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (h *Handler) configItem(w http.ResponseWriter, r *http.Request, p auth.Principal, rest string) {
	parts := strings.Split(rest, "/")
	if !fleet.ValidID(parts[0]) || len(parts) > 3 || len(parts) > 1 && parts[1] != "versions" {
		failure(w, fleet.ErrNotFound)
		return
	}
	id := parts[0]
	switch len(parts) {
	case 2:
		h.versionCollection(w, r, p, id)
		return
	case 3:
		h.version(w, r, p, id, parts[2])
		return
	}
	if !method(w, r, "GET", "PATCH", "DELETE") || !noQuery(w, r) {
		return
	}
	ctx := r.Context()
	if r.Method == "GET" {
		c, err := h.configs.GetConfig(ctx, id)
		if err != nil {
			failure(w, err)
			return
		}
		etag(w, c.Revision)
		writeJSON(w, 200, c)
		return
	}
	if !admin(w, p) {
		return
	}
	revision, ok := precondition(w, r)
	if !ok {
		return
	}
	if r.Method == "DELETE" {
		if err := h.configs.DeleteConfig(ctx, p.ID, id, revision); err != nil {
			failure(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if !decode(w, r, &body) {
		return
	}
	c, err := h.configs.GetConfig(ctx, id)
	if err != nil {
		failure(w, err)
		return
	}
	if body.Name != nil {
		c.Name = *body.Name
	}
	if body.Description != nil {
		c.Description = *body.Description
	}
	if c, err = h.configs.UpdateConfig(ctx, p.ID, c, revision); err != nil {
		failure(w, err)
		return
	}
	etag(w, c.Revision)
	writeJSON(w, 200, c)
}

func (h *Handler) versionCollection(w http.ResponseWriter, r *http.Request, p auth.Principal, id string) {
	if !method(w, r, "GET", "POST") {
		return
	}
	ctx := r.Context()
	if r.Method == "POST" {
		if !admin(w, p) || !noQuery(w, r) {
			return
		}
		var body struct {
			Body    string `json:"body"`
			Message string `json:"message"`
		}
		if !decodeLimit(w, r, &body, maxVersionRequest) {
			return
		}
		if err := configmgmt.Validate([]byte(body.Body), h.allowedComponents); err != nil {
			invalidConfig(w, err)
			return
		}
		v, err := h.configs.CreateVersion(ctx, p.ID, id, []byte(body.Body), body.Message)
		if err != nil {
			failure(w, err)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("/api/v1/configs/%s/versions/%d", id, v.Version))
		writeJSON(w, 201, v)
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after := 0
	if q.Has("after") {
		var err error
		if after, err = strconv.Atoi(q.Get("after")); err != nil || after < 1 {
			failure(w, fleet.Invalid("after must be a version number of at least 1"))
			return
		}
	}
	page, err := h.configs.ListVersions(ctx, id, after, limit)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// version returns a version; its body only to admins.
func (h *Handler) version(w http.ResponseWriter, r *http.Request, p auth.Principal, id, number string) {
	if !method(w, r, "GET") || !noQuery(w, r) {
		return
	}
	v, err := strconv.Atoi(number)
	if err != nil || v < 1 || strconv.Itoa(v) != number {
		failure(w, fleet.Invalid("the version must be a number of at least 1"))
		return
	}
	version, err := h.configs.GetVersion(r.Context(), id, v, p.Role == "admin")
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, version)
}

// invalidConfig reports why a body was rejected. Validation errors name a
// path and a reason, never a value.
func invalidConfig(w http.ResponseWriter, err error) {
	var invalid *configmgmt.ValidationError
	if !errors.As(err, &invalid) {
		if !errors.Is(err, fleet.ErrInvalid) {
			err = fleet.ErrInvalid
		}
		failure(w, err)
		return
	}
	message := invalid.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	code := "invalid_config"
	if errors.Is(err, configmgmt.ErrInlineSecret) {
		code = "inline_secret"
	}
	fail(w, 422, code, message)
}

type assignmentBody struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	ConfigID         string            `json:"config_id"`
	Version          int               `json:"version"`
	Layer            *int              `json:"layer"`
	GroupID          string            `json:"group_id"`
	Selector         string            `json:"selector"`
	CredentialLabels map[string]string `json:"credential_labels"`
	// Rollout optionally sets the policy of the rollout that introduces the
	// assignment; its ID is generated when omitted.
	Rollout *rolloutPolicy `json:"rollout"`
}

// rolloutPolicy is a rollout's ID and policy, prefilled with the defaults.
type rolloutPolicy struct {
	ID string `json:"id"`
	configmgmt.Policy
}

// defaultRollout prefills every field but the waves: decoding into a
// prefilled slice would merge the given waves into the default ones.
func defaultRollout() *rolloutPolicy {
	p := configmgmt.DefaultPolicy()
	p.Waves = nil
	return &rolloutPolicy{Policy: p}
}

// complete sets the defaults decoding cannot prefill.
func (p *rolloutPolicy) complete() {
	if p.ID == "" {
		p.ID = newID()
	}
	if p.Waves == nil {
		p.Waves = configmgmt.DefaultPolicy().Waves
	}
}

func (h *Handler) assignmentCollection(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if !method(w, r, "GET", "POST") {
		return
	}
	ctx := r.Context()
	if r.Method == "POST" {
		if !admin(w, p) || !noQuery(w, r) {
			return
		}
		body := assignmentBody{Rollout: defaultRollout()}
		if !decode(w, r, &body) {
			return
		}
		if body.Layer == nil || body.Rollout == nil {
			failure(w, fleet.Invalid("layer and rollout are required"))
			return
		}
		body.Rollout.complete()
		a, _, err := h.configs.CreateAssignment(ctx, p.ID, configmgmt.Assignment{ID: body.ID, Name: body.Name, ConfigID: body.ConfigID, Version: body.Version,
			Layer: *body.Layer, GroupID: body.GroupID, Selector: body.Selector, CredentialLabels: body.CredentialLabels},
			configmgmt.RolloutRequest{ID: body.Rollout.ID, Policy: body.Rollout.Policy})
		if err != nil {
			rolloutFailure(w, err)
			return
		}
		w.Header().Set("Location", "/api/v1/config-assignments/"+a.ID)
		etag(w, a.Revision)
		writeJSON(w, 201, a)
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after, ok := afterID(w, q)
	if !ok {
		return
	}
	page, err := h.configs.ListAssignments(ctx, after, limit)
	if err != nil {
		failure(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (h *Handler) assignmentItem(w http.ResponseWriter, r *http.Request, p auth.Principal, id string) {
	if id, ok := strings.CutSuffix(id, ":preview"); ok {
		h.preview(w, r, id)
		return
	}
	if !fleet.ValidID(id) {
		failure(w, fleet.ErrNotFound)
		return
	}
	if !method(w, r, "GET") || !noQuery(w, r) {
		return
	}
	a, err := h.configs.GetAssignment(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	etag(w, a.Revision)
	writeJSON(w, 200, a)
}

// previewItem is one agent an assignment matches, with the config it would
// receive from every assignment it matches.
type previewItem struct {
	AgentID string                    `json:"agent_id"`
	Desired *configmgmt.DesiredConfig `json:"desired"`
	Error   string                    `json:"error,omitempty"`
}

// preview pages the agents an assignment targets. Agents excluded by its
// credential labels are skipped, so a page may hold fewer than limit items
// while next_after continues it.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request, id string) {
	if !fleet.ValidID(id) {
		failure(w, fleet.ErrNotFound)
		return
	}
	if !method(w, r, "POST") {
		return
	}
	q, limit, ok := query(w, r, "after")
	if !ok {
		return
	}
	after, ok := afterID(w, q)
	if !ok {
		return
	}
	ctx := r.Context()
	targets, err := h.configs.Targets(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	var target *configmgmt.Target
	for i := range targets {
		if targets[i].ID == id {
			target = &targets[i]
		}
	}
	if target == nil {
		failure(w, fleet.ErrNotFound)
		return
	}
	aq := fleet.AgentQuery{After: after, Limit: limit}
	selector := target.Selector
	if target.GroupID != "" {
		if h.control == nil {
			failure(w, fleet.ErrUnavailable)
			return
		}
		g, err := h.control.GetGroup(ctx, target.GroupID)
		if err != nil {
			failure(w, err)
			return
		}
		if selector = g.Selector; g.Kind == fleet.GroupStatic {
			aq.Group = g.ID
		}
	}
	if selector != "" {
		if aq.Selector, err = fleet.ParseSelector(selector); err != nil {
			failure(w, fleet.ErrUnavailable)
			return
		}
	}
	page, err := h.store.ListAgents(ctx, aq)
	if err != nil {
		failure(w, err)
		return
	}
	labels := h.labelCache(ctx)
	out := struct {
		Items     []previewItem `json:"items"`
		NextAfter string        `json:"next_after,omitempty"`
	}{Items: []previewItem{}, NextAfter: page.NextAfter}
	for _, a := range page.Items {
		l, err := labels(a.CredentialID)
		if err != nil {
			failure(w, err)
			return
		}
		if !target.Matches(a.ID, a.Attributes, l) {
			continue
		}
		item := previewItem{AgentID: a.ID}
		item.Desired, item.Error = describe(targets, a, l)
		out.Items = append(out.Items, item)
	}
	writeJSON(w, 200, out)
}

// labelCache resolves credential labels once per request.
func (h *Handler) labelCache(ctx context.Context) func(string) (map[string]string, error) {
	cache := map[string]map[string]string{}
	return func(id string) (map[string]string, error) {
		if id == "" || h.credentials == nil {
			return nil, nil
		}
		if l, ok := cache[id]; ok {
			return l, nil
		}
		c, err := h.credentials.Repository.Credential(ctx, id)
		if errors.Is(err, fleet.ErrNotFound) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		cache[id] = c.Labels
		return c.Labels, nil
	}
}

func describe(targets []configmgmt.Target, a fleet.Agent, labels map[string]string) (*configmgmt.DesiredConfig, string) {
	var reported string
	if a.RemoteConfig != nil {
		reported = a.RemoteConfig.Hash
	}
	d, err := configmgmt.Describe(configmgmt.Match(targets, a.ID, a.Attributes, labels), reported)
	if errors.Is(err, configmgmt.ErrTooManyLayers) {
		return nil, "too_many_layers"
	}
	return d, ""
}

// agentConfigView is an agent's desired config next to what it reported.
type agentConfigView struct {
	AgentID                string                    `json:"agent_id"`
	CredentialRemoteConfig bool                      `json:"credential_remote_config"`
	AcceptsRemoteConfig    bool                      `json:"accepts_remote_config"`
	Desired                *configmgmt.DesiredConfig `json:"desired"`
	Error                  string                    `json:"error,omitempty"`
	Reported               *fleet.RemoteConfigState  `json:"reported"`
	InSync                 bool                      `json:"in_sync"`
	// MissingComponents are types the desired layers use that the agent's
	// build lacks; the config is not sent while any are missing.
	MissingComponents []string `json:"missing_components,omitempty"`
}

func (h *Handler) agentConfig(w http.ResponseWriter, r *http.Request, p auth.Principal, rest string) {
	id, retry := strings.CutSuffix(rest, "/config:retry")
	if !retry {
		id = strings.TrimSuffix(rest, "/config")
	}
	if !fleet.ValidID(id) {
		failure(w, fleet.Invalid("agent id "+fleet.IDRule))
		return
	}
	ctx := r.Context()
	if retry {
		if !method(w, r, "POST") || !admin(w, p) || !noQuery(w, r) {
			return
		}
		if err := h.configs.ClearHold(ctx, p.ID, id); err != nil {
			failure(w, err)
			return
		}
		w.WriteHeader(204)
		return
	}
	if !method(w, r, "GET") || !noQuery(w, r) {
		return
	}
	a, err := h.store.GetAgent(ctx, id)
	if err != nil {
		failure(w, err)
		return
	}
	targets, err := h.configs.Targets(ctx)
	if err != nil {
		failure(w, err)
		return
	}
	view := agentConfigView{AgentID: a.ID, AcceptsRemoteConfig: a.Capabilities&configmgmt.AcceptsRemoteConfig != 0, Reported: a.RemoteConfig}
	var labels map[string]string
	if a.CredentialID != "" && h.credentials != nil {
		c, err := h.credentials.Repository.Credential(ctx, a.CredentialID)
		if err != nil && !errors.Is(err, fleet.ErrNotFound) {
			failure(w, err)
			return
		}
		labels, view.CredentialRemoteConfig = c.Labels, c.RemoteConfig && c.RevokedAt == nil
	}
	view.Desired, view.Error = describe(targets, a, labels)
	if view.Desired != nil && a.RemoteConfig != nil {
		view.InSync = view.Desired.Hash == a.RemoteConfig.Hash && a.RemoteConfig.Status == string(configmgmt.Applied)
	}
	if view.Desired != nil && !view.InSync && a.AvailableComponentsHash != "" {
		if view.MissingComponents, err = h.missingComponents(ctx, view.Desired, a.AvailableComponentsHash); err != nil {
			failure(w, err)
			return
		}
	}
	writeJSON(w, 200, view)
}

// missingComponents applies the gateway's compatibility check to the desired
// layers. Only component types leave the server, never bodies.
func (h *Handler) missingComponents(ctx context.Context, d *configmgmt.DesiredConfig, hash string) ([]string, error) {
	set, err := h.configs.ComponentSet(ctx, hash)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	uses := configmgmt.Components{}
	for _, l := range d.Layers {
		if l.ConfigID == "" {
			continue
		}
		v, err := h.configs.GetVersion(ctx, l.ConfigID, l.Version, true)
		if err != nil {
			return nil, err
		}
		if v.Body != nil {
			uses = uses.Merge(configmgmt.Uses([]byte(*v.Body)))
		}
	}
	return set.Missing(uses), nil
}
