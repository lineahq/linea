package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// RoleBinding grants role in scope to exactly one of group and principal.
// Source "config" bindings are read-only and carry no creator.
type RoleBinding struct {
	ID        string     `json:"id"`
	Source    string     `json:"source"`
	Group     string     `json:"group,omitempty"`
	Principal string     `json:"principal,omitempty"`
	Role      string     `json:"role"`
	Scope     string     `json:"scope"`
	CreatedBy string     `json:"created_by,omitempty"`
	Created   *time.Time `json:"created,omitempty"`
}

// Role is a built-in role and its permissions.
type Role struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// RoleBindings is every binding, server config ones first, the revision of the
// managed bindings and the built-in roles.
type RoleBindings struct {
	Revision uint64        `json:"revision"`
	Bindings []RoleBinding `json:"bindings"`
	Roles    []Role        `json:"roles"`
}

// RoleBindingRequest grants role in scope ("*" when empty) to group or
// principal; revision is the managed bindings' revision the caller saw.
type RoleBindingRequest struct {
	Revision  uint64 `json:"revision"`
	Group     string `json:"group,omitempty"`
	Principal string `json:"principal,omitempty"`
	Role      string `json:"role"`
	Scope     string `json:"scope,omitempty"`
}

func roleBindingsOf(l *platform.RoleBindingList) RoleBindings {
	out := RoleBindings{Revision: l.Revision, Bindings: make([]RoleBinding, 0, len(l.Bindings)), Roles: make([]Role, 0, len(l.Roles))}
	for _, b := range l.Bindings {
		out.Bindings = append(out.Bindings, RoleBinding{ID: b.ID, Source: string(b.Source), Group: b.Group, Principal: b.Principal,
			Role: b.Role, Scope: b.Scope, CreatedBy: b.CreatedBy, Created: b.Created})
	}
	for _, r := range l.Roles {
		out.Roles = append(out.Roles, Role{Name: r.Name, Permissions: r.Permissions})
	}
	return out
}

func (s *Server) listRoleBindings(w http.ResponseWriter, r *http.Request) {
	l, err := s.runs.ListRoleBindings(r.Context(), callerOf(r))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roleBindingsOf(l))
}

func (s *Server) addRoleBinding(w http.ResponseWriter, r *http.Request) {
	var req RoleBindingRequest
	if !decode(w, r, &req) {
		return
	}
	l, err := s.runs.AddRoleBinding(r.Context(), callerOf(r), platform.AddRoleBindingRequest{Revision: req.Revision, Group: req.Group,
		Principal: req.Principal, Role: req.Role, Scope: req.Scope})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, roleBindingsOf(l))
}

func (s *Server) removeRoleBinding(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()["revision"]
	var revision uint64
	err := errors.New("not one revision")
	if len(values) == 1 {
		revision, err = strconv.ParseUint(values[0], 10, 64)
	}
	if err != nil {
		writeOperationError(w, badQuery("a delete names the managed bindings' revision: ?revision=N"))
		return
	}
	l, err := s.runs.RemoveRoleBinding(r.Context(), callerOf(r), revision, r.PathValue("id"))
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roleBindingsOf(l))
}

func accessPaths() map[string]any {
	return map[string]any{
		"/api/v1/role-bindings": map[string]any{
			"get": op("listRoleBindings",
				"Every role binding: the server config's, which are read-only, then the managed ones, with the managed bindings' revision and the built-in roles (access.manage).",
				nil, "", map[string]any{
					"200": reply("The bindings and roles.", ref("RoleBindings")),
					"403": operationFailure("The caller lacks access.manage."),
					"503": operationFailure("The server keeps no saved work."),
				}),
			"post": op("addRoleBinding",
				"Grant a built-in role in scope * or project:<id> to a group or principal (access.manage). Every instance applies it within a second.",
				nil, "RoleBindingRequest", map[string]any{
					"201": reply("The bindings after the change.", ref("RoleBindings")),
					"400": operationFailure("No or two subjects, an unknown role or an invalid scope."),
					"403": operationFailure("The caller lacks access.manage."),
					"409": operationFailure("The revision is stale, or a binding already grants this."),
					"413": operationFailure("The managed bindings are at their bound."),
				}),
		},
		"/api/v1/role-bindings/{id}": map[string]any{
			"delete": op("removeRoleBinding",
				"Remove a managed role binding (access.manage). Server config bindings change only in the server config, and a caller cannot remove the binding that gives them access.manage.",
				[]any{idParam, map[string]any{"name": "revision", "in": "query", "required": true, "description": "The managed bindings' revision the caller saw.",
					"schema": map[string]any{"type": "integer", "minimum": 0}}},
				"", map[string]any{
					"200": reply("The bindings after the change.", ref("RoleBindings")),
					"400": operationFailure("Missing or invalid revision."),
					"403": operationFailure("The caller lacks access.manage, or the binding is the server config's."),
					"404": operationFailure("Unknown managed binding."),
					"409": operationFailure("The revision is stale, or the removal would take away the caller's access.manage."),
				}),
		},
	}
}
