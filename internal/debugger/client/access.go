package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/lineahq/linea/internal/debugger/api"
)

// ListRoleBindings returns every role binding with the managed bindings'
// revision and the built-in roles.
func (c *Client) ListRoleBindings(ctx context.Context) (*api.RoleBindings, error) {
	raw, err := c.do(ctx, http.MethodGet, "/api/v1/role-bindings", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return c.roleBindings(raw)
}

// AddRoleBinding grants a role at the revision in req.
func (c *Client) AddRoleBinding(ctx context.Context, req api.RoleBindingRequest) (*api.RoleBindings, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/role-bindings", req, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	return c.roleBindings(raw)
}

// RemoveRoleBinding removes a managed binding at revision.
func (c *Client) RemoveRoleBinding(ctx context.Context, revision uint64, id string) (*api.RoleBindings, error) {
	path := "/api/v1/role-bindings/" + url.PathEscape(id) + "?revision=" + strconv.FormatUint(revision, 10)
	raw, err := c.do(ctx, http.MethodDelete, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return c.roleBindings(raw)
}

func (c *Client) roleBindings(raw []byte) (*api.RoleBindings, error) {
	var out api.RoleBindings
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}
