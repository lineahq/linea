package plugins

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/pluginsdk"
)

// hostRoute binds one host path to its method and registration permission.
type hostRoute struct {
	method, permission string
	serve              func(*Registry, http.ResponseWriter, *http.Request)
}

var hostRoutes = map[string]hostRoute{
	pluginsdk.AgentsPath:        {"GET", pluginsdk.AgentsRead, (*Registry).serveAgents},
	pluginsdk.AgentsResolvePath: {"POST", pluginsdk.AgentsRead, (*Registry).serveResolve},
	pluginsdk.GroupsResolvePath: {"POST", pluginsdk.AgentsRead, (*Registry).serveGroup},
	pluginsdk.IntrospectPath:    {"POST", pluginsdk.CredentialsIntrospect, (*Registry).serveIntrospect},
}

// SetHost provides the optional group and credential services. Without a
// control repository group IDs are unknown; without an introspector every
// token is inactive. Call it before the host listener serves.
func (r *Registry) SetHost(control fleet.ControlRepository, introspector auth.Introspector) {
	r.control, r.introspector = control, introspector
}

func hostJSONBounded(w http.ResponseWriter, value any, max int) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > max {
		hostError(w, 503)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// decodeHost reads one strict JSON object of at most MaxRequestBytes.
func decodeHost(req *http.Request, out any) bool {
	if req.URL.RawQuery != "" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, pluginsdk.MaxRequestBytes+1))
	if err != nil || len(body) > pluginsdk.MaxRequestBytes {
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}

func (r *Registry) serveAgents(w http.ResponseWriter, req *http.Request) {
	q, err := agentsQuery(req)
	if err != nil {
		hostError(w, 400)
		return
	}
	page, err := r.store.ListAgents(req.Context(), q)
	if err != nil {
		hostError(w, 503)
		return
	}
	hostJSONBounded(w, agentPage(page), pluginsdk.MaxAgentPageBytes)
}

// agentPage maps core inventory onto the versioned plugin contract, so new
// core fields never reach plugins that decode strictly. A page that would
// exceed the response bound is shortened and continues after its last item.
func agentPage(page fleet.Page) pluginsdk.AgentPage {
	out := pluginsdk.AgentPage{Items: make([]pluginsdk.Agent, 0, len(page.Items)), NextAfter: page.NextAfter}
	size := 64
	for _, a := range page.Items {
		item := pluginsdk.Agent{ID: a.ID, Generation: a.Generation, Connected: a.Connected, Presence: a.Presence,
			ConnectedAt: a.ConnectedAt, DisconnectedAt: a.DisconnectedAt, LastReportAt: a.LastSeenAt, Capabilities: a.Capabilities}
		if a.Health != nil {
			item.Health = &pluginsdk.Health{Healthy: a.Health.Healthy, ObservedAt: a.Health.ObservedAt}
		}
		if len(a.Attributes) > 0 {
			item.Attributes = a.Attributes
		}
		data, _ := json.Marshal(item)
		if size+len(data)+1 > pluginsdk.MaxAgentPageBytes && len(out.Items) > 0 {
			out.NextAfter = out.Items[len(out.Items)-1].ID
			break
		}
		size += len(data) + 1
		out.Items = append(out.Items, item)
	}
	return out
}

func (r *Registry) serveResolve(w http.ResponseWriter, req *http.Request) {
	var in pluginsdk.ResolveRequest
	if !decodeHost(req, &in) || len(in.Attributes) < 1 || len(in.Attributes) > pluginsdk.MaxResolveTerms ||
		(in.CredentialID != "" && !pluginsdk.ValidID(in.CredentialID)) || in.Limit < 0 || in.Limit > pluginsdk.MaxResolveAgents {
		hostError(w, 400)
		return
	}
	limit := in.Limit
	if limit == 0 {
		limit = 16
	}
	var selector fleet.Selector
	for k, v := range in.Attributes {
		if !fleet.ValidKey(k) || !fleet.ValidValue(v) {
			hostError(w, 400)
			return
		}
		selector.Terms = append(selector.Terms, fleet.Term{Key: k, Op: "=", Values: []string{v}})
	}
	slices.SortFunc(selector.Terms, func(a, b fleet.Term) int { return strings.Compare(a.Key, b.Key) })
	connected := true
	page, err := r.store.ListAgents(req.Context(), fleet.AgentQuery{Limit: limit + 1, Selector: selector, Connected: &connected, Credential: in.CredentialID})
	if err != nil {
		hostError(w, 503)
		return
	}
	out := pluginsdk.ResolveResponse{AgentIDs: []string{}, More: len(page.Items) > limit}
	for _, a := range page.Items[:min(limit, len(page.Items))] {
		out.AgentIDs = append(out.AgentIDs, a.ID)
	}
	hostJSONBounded(w, out, 16<<10)
}

var errTooLarge = errors.New("group exceeds the requested limit")

func (r *Registry) serveGroup(w http.ResponseWriter, req *http.Request) {
	var in pluginsdk.GroupResolveRequest
	if !decodeHost(req, &in) || (in.GroupID == "") == (in.Selector == "") || in.Limit < 0 || in.Limit > pluginsdk.MaxGroupAgents {
		hostError(w, 400)
		return
	}
	limit := in.Limit
	if limit == 0 {
		limit = pluginsdk.MaxGroupAgents
	}
	q := fleet.AgentQuery{Connected: in.Connected}
	var revision int64
	if in.GroupID != "" {
		if !fleet.ValidID(in.GroupID) {
			hostError(w, 400)
			return
		}
		if r.control == nil {
			hostError(w, 404)
			return
		}
		g, err := r.control.GetGroup(req.Context(), in.GroupID)
		if errors.Is(err, fleet.ErrNotFound) {
			hostError(w, 404)
			return
		}
		if err != nil {
			hostError(w, 503)
			return
		}
		revision = g.Revision
		if g.Kind == fleet.GroupSelector {
			if q.Selector, err = fleet.ParseSelector(g.Selector); err != nil {
				hostError(w, 503)
				return
			}
		} else {
			q.Group = g.ID
		}
	} else {
		var err error
		if q.Selector, err = fleet.ParseSelector(in.Selector); err != nil {
			hostError(w, 400)
			return
		}
	}
	ids, err := r.members(req, q, limit)
	switch {
	case errors.Is(err, errTooLarge):
		hostError(w, 422)
	case err != nil:
		hostError(w, 503)
	default:
		hostJSONBounded(w, pluginsdk.GroupResolveResponse{AgentIDs: ids, Revision: revision}, 64<<10)
	}
}

// members pages the whole match set and refuses rather than truncates.
func (r *Registry) members(req *http.Request, q fleet.AgentQuery, limit int) ([]string, error) {
	ids := []string{}
	q.Limit = 100
	for {
		page, err := r.store.ListAgents(req.Context(), q)
		if err != nil {
			return nil, err
		}
		for _, a := range page.Items {
			ids = append(ids, a.ID)
		}
		if len(ids) > limit {
			return nil, errTooLarge
		}
		if page.NextAfter == "" || len(page.Items) == 0 {
			return ids, nil
		}
		q.After = page.NextAfter
	}
}

func (r *Registry) serveIntrospect(w http.ResponseWriter, req *http.Request) {
	var in pluginsdk.IntrospectRequest
	if !decodeHost(req, &in) {
		hostError(w, 400)
		return
	}
	digest, err := hex.DecodeString(in.TokenSHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != in.TokenSHA256 {
		hostError(w, 400)
		return
	}
	out := pluginsdk.Introspection{}
	if r.introspector != nil {
		got, err := r.introspector.Introspect(req.Context(), [32]byte(digest))
		if err != nil {
			hostError(w, 503)
			return
		}
		if got.Active {
			out = pluginsdk.Introspection{Active: true, CredentialID: got.CredentialID, Role: got.Role, Labels: got.Labels, ExpiresAt: got.ExpiresAt}
		}
	}
	hostJSONBounded(w, out, 4096)
}
