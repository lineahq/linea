package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestRoleBindingsOverHTTP lists, adds and removes a managed binding, maps
// a stale revision to 409 and a server config binding's removal to 403, and
// refuses callers without access.manage.
func TestRoleBindingsOverHTTP(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Access: bindPlatform(t, "admin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, RemoteAccess: true, Authenticator: headerAuthenticator{}})
	request := func(method, path string, body any, header map[string]string) *http.Response {
		return identityRequest(t, method, srv.URL+path, body, header)
	}
	admin := map[string]string{"X-Test-User": "root"}
	sre := map[string]string{"X-Test-User": "bob", "X-Test-Group": "sre"}

	list := decodeBody[RoleBindings](t, request(http.MethodGet, "/api/v1/role-bindings", nil, admin), http.StatusOK)
	if list.Revision != 0 || len(list.Bindings) != 1 || list.Bindings[0].Source != "config" || list.Bindings[0].Group != "platform" || len(list.Roles) == 0 {
		t.Fatalf("list = %+v", list)
	}
	if e := decodeBody[OperationError](t, request(http.MethodGet, "/api/v1/role-bindings", nil, sre), http.StatusForbidden); e.Code != string(platform.CodeForbidden) {
		t.Errorf("sre listing = %+v", e)
	}

	added := decodeBody[RoleBindings](t, request(http.MethodPost, "/api/v1/role-bindings", RoleBindingRequest{Group: "sre", Role: "viewer"}, admin), http.StatusCreated)
	if added.Revision != 1 || len(added.Bindings) != 2 || added.Bindings[1].Scope != "*" || added.Bindings[1].Source != "managed" || added.Bindings[1].Created == nil {
		t.Fatalf("added = %+v", added)
	}
	if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/role-bindings", RoleBindingRequest{Group: "ops", Role: "viewer"}, admin), http.StatusConflict); e.Code != string(platform.CodeConflict) {
		t.Errorf("stale add = %+v", e)
	}
	if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/role-bindings", RoleBindingRequest{Revision: 1, Group: "ops", Role: "owner"}, admin), http.StatusBadRequest); e.Code != string(platform.CodeInvalidRequest) {
		t.Errorf("unknown role = %+v", e)
	}
	if e := decodeBody[OperationError](t, request(http.MethodDelete, "/api/v1/role-bindings/"+list.Bindings[0].ID+"?revision=1", nil, admin), http.StatusForbidden); e.Code != string(platform.CodeForbidden) {
		t.Errorf("removing a server config binding = %+v", e)
	}
	if e := decodeBody[OperationError](t, request(http.MethodDelete, "/api/v1/role-bindings/"+added.Bindings[1].ID, nil, admin), http.StatusBadRequest); e.Code != string(platform.CodeInvalidRequest) {
		t.Errorf("removing without a revision = %+v", e)
	}
	removed := decodeBody[RoleBindings](t, request(http.MethodDelete, "/api/v1/role-bindings/"+added.Bindings[1].ID+"?revision="+strconv.FormatUint(added.Revision, 10), nil, admin), http.StatusOK)
	if removed.Revision != 2 || len(removed.Bindings) != 1 {
		t.Errorf("removed = %+v", removed)
	}
}

// TestRoleBindingRequestsAreHardened: a viewer cannot write; a body cannot
// set the creator, source or ID, nor be malformed, untyped, oversized or
// followed by a second document; and
// path IDs cannot reach anything but a managed binding.
func TestRoleBindingRequestsAreHardened(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Access: bindPlatform(t, "admin")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, RemoteAccess: true, Authenticator: headerAuthenticator{}})
	admin := map[string]string{"X-Test-User": "root"}
	viewer := map[string]string{"X-Test-User": "vic", "X-Test-Group": "viewers"}
	raw := func(method, path, contentType string, body []byte, header map[string]string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	status := func(name string, resp *http.Response, want int) {
		t.Helper()
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", name, resp.StatusCode, want)
		}
	}

	added := decodeBody[RoleBindings](t, identityRequest(t, http.MethodPost, srv.URL+"/api/v1/role-bindings", RoleBindingRequest{Group: "viewers", Role: "viewer"}, admin), http.StatusCreated)
	managed := added.Bindings[1].ID
	status("viewer listing", identityRequest(t, http.MethodGet, srv.URL+"/api/v1/role-bindings", nil, viewer), http.StatusForbidden)
	status("viewer granting itself admin", identityRequest(t, http.MethodPost, srv.URL+"/api/v1/role-bindings", RoleBindingRequest{Revision: 1, Group: "viewers", Role: "admin"}, viewer), http.StatusForbidden)
	status("viewer removing a binding", identityRequest(t, http.MethodDelete, srv.URL+"/api/v1/role-bindings/"+managed+"?revision=1", nil, viewer), http.StatusForbidden)
	status("no credential", identityRequest(t, http.MethodGet, srv.URL+"/api/v1/role-bindings", nil, nil), http.StatusUnauthorized)

	for name, body := range map[string]string{
		"a spoofed creator":   `{"revision":1,"group":"ops","role":"viewer","created_by":"user:someone-else"}`,
		"a spoofed source":    `{"revision":1,"group":"ops","role":"viewer","source":"config"}`,
		"a chosen ID":         `{"revision":1,"group":"ops","role":"viewer","id":"` + strings.Repeat("0", 32) + `"}`,
		"malformed JSON":      `{"revision":1,"group":"ops",`,
		"a string revision":   `{"revision":"1","group":"ops","role":"viewer"}`,
		"a negative revision": `{"revision":-1,"group":"ops","role":"viewer"}`,
	} {
		status(name, raw(http.MethodPost, "/api/v1/role-bindings", "application/json", []byte(body), admin), http.StatusBadRequest)
	}
	status("two documents", raw(http.MethodPost, "/api/v1/role-bindings", "application/json",
		[]byte(`{"revision":1,"group":"ops","role":"viewer"}{"revision":2,"group":"x","role":"admin"}`), admin), http.StatusBadRequest)
	revision := strconv.FormatUint(decodeBody[RoleBindings](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/role-bindings", nil, admin), http.StatusOK).Revision, 10)
	status("a form body", raw(http.MethodPost, "/api/v1/role-bindings", "application/x-www-form-urlencoded", []byte("revision=1&group=ops&role=admin"), admin), http.StatusUnsupportedMediaType)
	huge, err := json.Marshal(RoleBindingRequest{Revision: 1, Group: strings.Repeat("g", maxRequestBytes), Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	status("an oversized body", raw(http.MethodPost, "/api/v1/role-bindings", "application/json", huge, admin), http.StatusBadRequest)

	for name, id := range map[string]string{
		"a traversal":           url.PathEscape("../tokens"),
		"an encoded slash":      "a%2Fb",
		"a long ID":             strings.Repeat("a", 4096),
		"an upper-case ID":      strings.ToUpper(managed),
		"a SQL fragment":        url.PathEscape("x' OR '1'='1"),
		"a server config guess": "config-99",
	} {
		resp := identityRequest(t, http.MethodDelete, srv.URL+"/api/v1/role-bindings/"+id+"?revision="+revision, nil, admin)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("removing %s = %d, want 404 or 403", name, resp.StatusCode)
		}
	}
	for name, q := range map[string]string{"a negative revision": "-1", "a huge revision": "18446744073709551616", "text": "one", "two revisions": "1&revision=2"} {
		resp := identityRequest(t, http.MethodDelete, srv.URL+"/api/v1/role-bindings/"+managed+"?revision="+q, nil, admin)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("removing with %s = %d, want 400", name, resp.StatusCode)
		}
	}

	list := decodeBody[RoleBindings](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/role-bindings", nil, admin), http.StatusOK)
	if list.Revision != 1 || len(list.Bindings) != 2 || list.Bindings[1].Group != "viewers" || list.Bindings[1].CreatedBy != "user:root" {
		t.Fatalf("bindings after the refused requests = %+v", list)
	}
}
