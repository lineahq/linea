package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/lineahq/linea/internal/debugger/api"
)

// CreateToken creates an API token; the secret is in the answer only.
func (c *Client) CreateToken(ctx context.Context, req api.TokenRequest) (*api.CreatedToken, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/tokens", req, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	var out api.CreatedToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// ListTokens lists the caller's API tokens, or with all every principal's.
func (c *Client) ListTokens(ctx context.Context, all bool) ([]api.APIToken, error) {
	path := "/api/v1/tokens"
	if all {
		path += "?all=true"
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []api.APIToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return out, nil
}

// RevokeToken revokes an API token.
func (c *Client) RevokeToken(ctx context.Context, id string) (*api.APIToken, error) {
	raw, err := c.do(ctx, http.MethodDelete, "/api/v1/tokens/"+url.PathEscape(id), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.APIToken
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}
