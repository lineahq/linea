package pluginsdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"
)

// Host services beyond the session. Each is gated by a registration
// permission: agents.read covers the directory and both resolvers, and
// credentials.introspect covers introspection.
const (
	CredentialsIntrospect = "credentials.introspect"
	AgentsResolvePath     = "/fleet-host/v3/agents/resolve"
	GroupsResolvePath     = "/fleet-host/v3/groups/resolve"
	IntrospectPath        = "/fleet-host/v3/credentials/introspect"
)

// Host bounds. A group that resolves to more than the requested limit is
// refused with 422 rather than truncated.
const (
	MaxRequestBytes   = 4096
	MaxAgentPageBytes = 1 << 20
	MaxResolveTerms   = 8
	MaxResolveAgents  = 100
	MaxGroupAgents    = 1000
)

// ResolveRequest finds connected agents whose attributes equal every term,
// optionally restricted to the credential they connected with. Plugins use it
// to link a Collector that presented an agent credential to its fleet agent.
type ResolveRequest struct {
	CredentialID string            `json:"credential_id,omitempty"`
	Attributes   map[string]string `json:"attributes"`
	Limit        int               `json:"limit,omitempty"`
}

type ResolveResponse struct {
	AgentIDs []string `json:"agent_ids"`
	More     bool     `json:"more"`
}

// GroupResolveRequest names a fleet group or a selector expression.
type GroupResolveRequest struct {
	GroupID   string `json:"group_id,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Connected *bool  `json:"connected,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type GroupResolveResponse struct {
	AgentIDs []string `json:"agent_ids"`
	Revision int64    `json:"revision,omitempty"`
}

// IntrospectRequest carries the SHA-256 of a bearer a Collector presented to
// the plugin; the token itself never leaves the plugin.
type IntrospectRequest struct {
	TokenSHA256 string `json:"token_sha256"`
}

// Introspection reports only active agent credentials; anything else is
// {"active":false}.
type Introspection struct {
	Active       bool              `json:"active"`
	CredentialID string            `json:"credential_id,omitempty"`
	Role         string            `json:"role,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
}

// AgentQuery filters the host agent directory.
type AgentQuery struct {
	After     string
	Limit     int
	Selector  string
	Connected *bool
}

func (c HostClient) post(ctx context.Context, session Session, path string, in any, max int64, out any) error {
	body, err := json.Marshal(in)
	if err != nil || len(body) > MaxRequestBytes {
		return ErrProtocol
	}
	return RequestJSON(ctx, c.HTTP, "POST", c.Endpoint+path, c.Token, c.Headers(session), body, max, out)
}

// Agents reads one directory page (at most 100 agents and 1 MiB).
func (c HostClient) Agents(ctx context.Context, session Session, q AgentQuery) (AgentPage, error) {
	values := url.Values{}
	if q.After != "" {
		values.Set("after", q.After)
	}
	if q.Limit > 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Selector != "" {
		values.Set("selector", q.Selector)
	}
	if q.Connected != nil {
		values.Set("connected", strconv.FormatBool(*q.Connected))
	}
	endpoint := c.Endpoint + AgentsPath
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	var page AgentPage
	err := GetJSON(ctx, c.HTTP, endpoint, c.Token, c.Headers(session), MaxAgentPageBytes, &page)
	return page, err
}

// ResolveAgents finds connected agents matching every attribute (1-8 terms).
func (c HostClient) ResolveAgents(ctx context.Context, session Session, req ResolveRequest) (ResolveResponse, error) {
	var out ResolveResponse
	err := c.post(ctx, session, AgentsResolvePath, req, 16<<10, &out)
	return out, err
}

// ResolveGroup lists the members of a group or selector, or refuses with 422
// when they exceed the limit.
func (c HostClient) ResolveGroup(ctx context.Context, session Session, req GroupResolveRequest) (GroupResolveResponse, error) {
	var out GroupResolveResponse
	err := c.post(ctx, session, GroupsResolvePath, req, 64<<10, &out)
	return out, err
}

// Introspect hashes a presented bearer token and asks the host about it.
func (c HostClient) Introspect(ctx context.Context, session Session, token string) (Introspection, error) {
	return c.IntrospectHash(ctx, session, sha256.Sum256([]byte(token)))
}

// IntrospectHash asks about a token by its SHA-256.
func (c HostClient) IntrospectHash(ctx context.Context, session Session, sum [32]byte) (Introspection, error) {
	var out Introspection
	err := c.post(ctx, session, IntrospectPath, IntrospectRequest{TokenSHA256: hex.EncodeToString(sum[:])}, 4096, &out)
	return out, err
}

// StatusOf returns the HTTP status of a host refusal, or 0 for transport and
// decoding failures. 404 means the group is unknown and 422 that it exceeds
// the requested limit.
func StatusOf(err error) int {
	var failure *HTTPError
	if errors.As(err, &failure) {
		return failure.Status
	}
	return 0
}
