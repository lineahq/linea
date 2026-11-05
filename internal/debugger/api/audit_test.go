package api

import (
	"net/http"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestAuditEventsRecordTheHTTPRequest reads the audit log over HTTP: each
// request is one event with the authenticated principal and the request ID
// the response carries; a viewer is refused the log and that is recorded.
func TestAuditEventsRecordTheHTTPRequest(t *testing.T) {
	access, err := platform.NewPolicy([]platform.Binding{
		{Subject: platform.Subject{Group: "platform"}, Role: "viewer", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Group: "admins"}, Role: "admin", Scope: platform.ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := platform.New(platform.Options{Access: access})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}})

	alice := map[string]string{"X-Test-User": "alice"}
	list := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, alice)
	requestID := list.Header.Get("X-Request-Id")
	decodeBody[RunPage](t, list, http.StatusOK)
	refused := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/audit-events", nil, alice)
	if e := decodeBody[Error](t, refused, http.StatusForbidden); e.Error != "forbidden: this operation needs the audit.read permission" {
		t.Fatalf("viewer audit = %q", e.Error)
	}

	admin := map[string]string{"X-Test-User": "root", "X-Test-Group": "admins"}
	page := decodeBody[AuditPage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/audit-events?principal=user:alice&limit=1", nil, admin), http.StatusOK)
	if len(page.Events) != 1 || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}
	denied := page.Events[0]
	if denied.Operation != "ListAuditEvents" || denied.Outcome != "denied" || denied.Client != "http" || denied.PrincipalKind != "user" || denied.Resources == nil {
		t.Errorf("denied event = %+v", denied)
	}
	page = decodeBody[AuditPage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/audit-events?principal=user:alice&cursor="+page.NextCursor, nil, admin), http.StatusOK)
	if len(page.Events) != 1 || page.Events[0].Operation != "ListRuns" || page.Events[0].RequestID != requestID || page.Events[0].Outcome != "allowed" {
		t.Errorf("second page = %+v (request %s)", page, requestID)
	}
	for _, query := range []string{"limit=x", "since=yesterday", "outcome=maybe", "cursor=0"} {
		resp := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/audit-events?"+query, nil, admin)
		if e := decodeBody[OperationError](t, resp, http.StatusBadRequest); e.Code != string(platform.CodeInvalidRequest) {
			t.Errorf("%s = %+v", query, e)
		}
	}
}
