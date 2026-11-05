package api

import (
	"net/http"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// APIToken is an API token's record; it never holds the
// secret.
type APIToken struct {
	ID            string     `json:"id"`
	Principal     string     `json:"principal"`
	PrincipalKind string     `json:"principal_kind"`
	Label         string     `json:"label"`
	CreatedBy     string     `json:"created_by"`
	Created       time.Time  `json:"created"`
	Expires       time.Time  `json:"expires"`
	Revoked       *time.Time `json:"revoked,omitempty"`
}

// TokenRequest creates a personal token, or with service a service
// principal's token (token.manage).
type TokenRequest struct {
	Label string `json:"label"`
	// Service is the service ID; the token acts as "service:<service>".
	Service string `json:"service,omitempty"`
	// ExpiresInDays is the token's lifetime, 1 to 365 days; 0 is 90.
	ExpiresInDays int `json:"expires_in_days,omitempty"`
}

// CreatedToken is a new token with its secret, shown only in this response.
type CreatedToken struct {
	Token  APIToken `json:"token"`
	Secret string   `json:"secret"`
}

func apiTokenOf(t platform.APIToken) APIToken {
	return APIToken{ID: t.ID, Principal: t.Principal, PrincipalKind: string(t.PrincipalKind), Label: t.Label, CreatedBy: t.CreatedBy,
		Created: t.Created, Expires: t.Expires, Revoked: t.Revoked}
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req TokenRequest
	if !decode(w, r, &req) {
		return
	}
	ttl := time.Duration(req.ExpiresInDays) * 24 * time.Hour
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 365 {
		ttl = -1 // refused by the platform, without overflowing
	}
	created, err := s.runs.CreateAPIToken(r.Context(), callerOf(r), platform.CreateAPITokenRequest{Label: req.Label, Service: req.Service, TTL: ttl})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, CreatedToken{Token: apiTokenOf(created.Token), Secret: created.Secret})
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.runs.ListAPITokens(r.Context(), callerOf(r), platform.APITokenFilter{All: r.URL.Query().Get("all") == "true"})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	out := make([]APIToken, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, apiTokenOf(t))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	t, err := s.runs.RevokeAPIToken(r.Context(), callerOf(r), r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiTokenOf(*t))
}

func tokenPaths() map[string]any {
	return map[string]any{
		"/api/v1/tokens": map[string]any{
			"post": op("createToken",
				"Create an API token: a personal token acting as the caller with the caller's groups (token.own), or with service a service principal's token (token.manage). The secret is in this response only; the server keeps its SHA-256.",
				nil, "TokenRequest", map[string]any{
					"201": reply("The token and its secret.", ref("CreatedToken")),
					"400": operationFailure("Invalid label, lifetime or service ID, or a personal token for a caller that is not a user."),
					"403": operationFailure("The caller lacks token.own, or token.manage for a service token."),
					"503": operationFailure("The server keeps no saved work."),
				}),
			"get": op("listTokens", "API tokens newest first, revoked and expired ones included: the caller's own, or with all=true every principal's (token.manage).",
				[]any{map[string]any{"name": "all", "in": "query", "description": "true lists every principal's tokens.", "schema": map[string]any{"type": "boolean"}}},
				"", map[string]any{
					"200": reply("The tokens.", map[string]any{"type": "array", "items": ref("APIToken")}),
					"403": operationFailure("The caller lacks token.own, or token.manage with all=true."),
				}),
		},
		"/api/v1/tokens/{id}": map[string]any{
			"delete": op("revokeToken", "Revoke an API token: the caller's own (token.own) or anyone's (token.manage). It stops working on its next use; revoking again changes nothing.",
				[]any{idParam}, "", map[string]any{
					"200": reply("The revoked token.", ref("APIToken")),
					"403": operationFailure("The token is another principal's and the caller lacks token.manage."),
					"404": operationFailure("Unknown token."),
				}),
		},
	}
}
