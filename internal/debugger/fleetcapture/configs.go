package fleetcapture

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Fleet's config management reads (optional in fleet, behind
// --config-management): an agent's desired layers, stored config versions
// and rollouts. The debugger only reads them, to rebuild the configuration an
// agent received; it never creates versions, assignments or rollouts.

// AgentConfig is the part of fleet's view of an agent's config management
// the debugger reads.
type AgentConfig struct {
	AgentID string `json:"agent_id"`
	// Desired is nil when no assignment matches the agent and it was never
	// managed.
	Desired *DesiredConfig `json:"desired"`
	// Error is "too_many_layers" when more assignments match than fleet sends.
	Error string `json:"error,omitempty"`
}

// DesiredConfig is the layers fleet sends an agent, in entry name order.
type DesiredConfig struct {
	Layers []DesiredLayer `json:"layers"`
	Hash   string         `json:"hash,omitempty"`
	// Unmanaged is the single empty entry that releases a formerly managed
	// agent to its local configuration.
	Unmanaged bool `json:"unmanaged,omitempty"`
}

// DesiredLayer is one config entry, named NN-<assignment name>. The
// supervisor merges entries in name order over its local files.
type DesiredLayer struct {
	Name         string `json:"name"`
	AssignmentID string `json:"assignment_id,omitempty"`
	ConfigID     string `json:"config_id,omitempty"`
	Version      int    `json:"version,omitempty"`
	SHA256       string `json:"sha256"`
}

// StoredConfig is a fleet config: a named container of immutable, numbered versions.
type StoredConfig struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Revision      int64     `json:"revision"`
	LatestVersion int       `json:"latest_version"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by,omitempty"`
}

// ConfigVersion is one stored version. Body is Collector YAML, returned only
// by an admin read of a single version.
type ConfigVersion struct {
	ConfigID  string    `json:"config_id"`
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256"`
	Size      int       `json:"size"`
	Message   string    `json:"message,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	Body      string    `json:"body,omitempty"`
}

// ConfigVersionPage is a page of versions, newest first; NextAfter is the
// version to continue below.
type ConfigVersionPage struct {
	Items     []ConfigVersion `json:"items"`
	NextAfter string          `json:"next_after,omitempty"`
}

// Rollout is the part of a fleet rollout the debugger reads. FromVersion is
// zero for a rollout creating its assignment, ToVersion for one deleting it.
type Rollout struct {
	ID           string    `json:"id"`
	AssignmentID string    `json:"assignment_id"`
	ConfigID     string    `json:"config_id"`
	FromVersion  int       `json:"from_version,omitempty"`
	ToVersion    int       `json:"to_version,omitempty"`
	State        string    `json:"state"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RolloutPage is a page of rollouts in ID order.
type RolloutPage struct {
	Items     []Rollout `json:"items"`
	NextAfter string    `json:"next_after,omitempty"`
}

// AgentConfig reads an agent's desired layers.
func (c *Client) AgentConfig(ctx context.Context, agentID string) (*AgentConfig, error) {
	var out AgentConfig
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/agents/"+url.PathEscape(agentID)+"/config", nil), nil, nil, &out)
	return &out, err
}

// StoredConfig reads a config's metadata.
func (c *Client) StoredConfig(ctx context.Context, id string) (*StoredConfig, error) {
	var out StoredConfig
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/configs/"+url.PathEscape(id), nil), nil, nil, &out)
	return &out, err
}

// ConfigVersions reads one page of a config's versions, newest first, below
// the version after (0: from the newest).
func (c *Client) ConfigVersions(ctx context.Context, id string, after, limit int) (*ConfigVersionPage, error) {
	q := pageQuery("", limit)
	if after > 0 {
		q.Set("after", strconv.Itoa(after))
	}
	var out ConfigVersionPage
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/configs/"+url.PathEscape(id)+"/versions", q), nil, nil, &out)
	return &out, err
}

// ConfigVersion reads one version with its body, which needs an admin
// credential.
func (c *Client) ConfigVersion(ctx context.Context, id string, version int) (*ConfigVersion, error) {
	var out ConfigVersion
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/configs/"+url.PathEscape(id)+"/versions/"+strconv.Itoa(version), nil), nil, nil, &out)
	return &out, err
}

// Rollouts reads one page of rollouts in ID order.
func (c *Client) Rollouts(ctx context.Context, after string, limit int) (*RolloutPage, error) {
	var out RolloutPage
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/rollouts", pageQuery(after, limit)), nil, nil, &out)
	return &out, err
}
