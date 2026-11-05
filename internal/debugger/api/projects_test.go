package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestProjectsOverHTTP creates, updates and archives a project, shares the
// built-in runtime with it, maps a stale revision to 409, and refuses a
// request that names two projects or a viewer that manages them.
func TestProjectsOverHTTP(t *testing.T) {
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
	viewer := map[string]string{"X-Test-User": "vic", "X-Test-Group": "viewers"}

	list := decodeBody[Projects](t, request(http.MethodGet, "/api/v1/projects", nil, admin), http.StatusOK)
	ids := map[string]bool{}
	for _, p := range list.Projects {
		ids[p.ID] = p.BuiltIn
	}
	if !ids[artifact.DefaultProject] || !ids[artifact.LibraryProject] {
		t.Fatalf("built-in projects = %+v", list.Projects)
	}

	created := decodeBody[Projects](t, request(http.MethodPost, "/api/v1/projects", ProjectRequest{Revision: list.Revision, ID: "payments", Name: "Payments",
		ProjectSettings: ProjectSettings{RetentionRuns: 50}}, admin), http.StatusCreated)
	var payments *Project
	for i := range created.Projects {
		if created.Projects[i].ID == "payments" {
			payments = &created.Projects[i]
		}
	}
	if payments == nil || payments.Name != "Payments" || payments.RetentionRuns != 50 || payments.CreatedBy != "user:root" {
		t.Fatalf("created = %+v", created.Projects)
	}
	if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/projects", ProjectRequest{Revision: list.Revision, ID: "checkout", Name: "Checkout"}, admin), http.StatusConflict); e.Code != string(platform.CodeConflict) {
		t.Errorf("stale create = %+v", e)
	}
	updated := decodeBody[Projects](t, request(http.MethodPut, "/api/v1/projects/payments", ProjectRequest{Revision: created.Revision, Name: "Payments team"}, admin), http.StatusOK)
	shared := decodeBody[Sharing](t, request(http.MethodPut, "/api/v1/runtimes/"+platform.BuiltInRuntime+"/availability", SharingRequest{Revision: updated.Revision, Projects: []string{"payments"}}, admin), http.StatusOK)
	if len(shared.Projects) != 1 || shared.Projects[0] != "payments" || shared.SetBy != "user:root" {
		t.Errorf("availability = %+v", shared)
	}
	archived := decodeBody[Projects](t, request(http.MethodPost, "/api/v1/projects/payments/archive", ProjectRevision{Revision: shared.Revision}, admin), http.StatusOK)
	for _, p := range archived.Projects {
		if p.ID == "payments" && (!p.Archived || p.Name != "Payments team") {
			t.Errorf("archived = %+v", p)
		}
	}

	if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/projects", ProjectRequest{Revision: archived.Revision, ID: "ops", Name: "Ops"}, viewer), http.StatusForbidden); e.Code != string(platform.CodeForbidden) {
		t.Errorf("viewer create = %+v", e)
	}
	if resp := request(http.MethodGet, "/api/v1/runs?project=payments&project=default", nil, admin); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("two projects: %s", resp.Status)
	}
	if page := decodeBody[LibraryPage](t, request(http.MethodGet, "/api/v1/library", nil, admin), http.StatusOK); len(page.Items) != 0 {
		t.Errorf("library = %+v", page)
	}
	if resp := request(http.MethodPost, "/api/v1/artifacts/"+strings.Repeat("0", 32)+"/copies?project=default", CopyRequest{}, admin); resp.StatusCode != http.StatusNotFound {
		t.Errorf("copying a missing artifact: %s", resp.Status)
	}
}
