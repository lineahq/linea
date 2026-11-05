package api

import (
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Session is who the request's credential signed in,
// with the CSRF token a signed-in web page sends on unsafe requests.
type Session struct {
	Principal SessionPrincipal `json:"principal"`
	// CSRFToken is set for a web session; requests that change something
	// send it as X-CSRF-Token. Other credentials need none.
	CSRFToken string `json:"csrf_token,omitempty"`
	// Permissions are those the principal holds: platform-wide ones
	// and project ones held in some project. A client disables what is
	// missing; the server still checks every request.
	Permissions []string `json:"permissions"`
}

// SessionPrincipal is the signed-in principal.
type SessionPrincipal struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Name   string   `json:"name,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	caller := callerOf(r)
	p := caller.Principal
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, Session{
		Principal:   SessionPrincipal{ID: p.ID, Kind: string(p.Kind), Name: p.Name, Groups: p.Groups},
		CSRFToken:   platform.CSRFToken(r),
		Permissions: s.runs.Permissions(caller),
	})
}

func sessionPaths() map[string]any {
	return map[string]any{
		"/api/v1/session": map[string]any{
			"get": op("getSession", "The principal the request's credential signs in, the permissions it holds, and for a web session the CSRF token that requests changing something send as X-CSRF-Token. In local mode it is the local owner, who holds every permission.",
				nil, "", map[string]any{
					"200": reply("The signed-in principal.", ref("Session")),
				}),
		},
	}
}
