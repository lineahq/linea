package api

import (
	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"testing"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// TestTokensOverHTTP creates, uses, lists and revokes an API token on a
// team server, which answers any Host; a local server still refuses one.
func TestTokensOverHTTP(t *testing.T) {
	service, err := platform.Open(memory.New(artifact.Limits{}), platform.Options{Access: bindPlatform(t, "viewer")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, RemoteAccess: true,
		Authenticator: platform.Authenticators{service.TokenAuthenticator(), headerAuthenticator{}}})
	request := func(method, path string, body any, header map[string]string) *http.Response {
		resp := identityRequest(t, method, srv.URL+path, body, header)
		return resp
	}
	alice := map[string]string{"X-Test-User": "alice"}
	created := decodeBody[CreatedToken](t, request(http.MethodPost, "/api/v1/tokens", TokenRequest{Label: "laptop", ExpiresInDays: 7}, alice), http.StatusCreated)
	if created.Secret == "" || created.Token.Principal != "user:alice" || created.Token.PrincipalKind != "user" || created.Token.Expires.Sub(created.Token.Created).Hours() != 7*24 {
		t.Fatalf("created = %+v", created)
	}
	bearer := map[string]string{"Authorization": "Bearer " + created.Secret}
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tokens", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "debugger.example.com"
	req.Header.Set("Authorization", bearer["Authorization"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if list := decodeBody[[]APIToken](t, resp, http.StatusOK); len(list) != 1 || list[0].ID != created.Token.ID || list[0].Revoked != nil {
		t.Fatalf("list = %+v", list)
	}
	if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/tokens", TokenRequest{Label: "ci", Service: "ci"}, bearer), http.StatusForbidden); e.Code != string(platform.CodeForbidden) {
		t.Errorf("service token as viewer = %+v", e)
	}
	for _, body := range []TokenRequest{{Label: "x", ExpiresInDays: 366}, {Label: "x", ExpiresInDays: -1}, {Label: "x", ExpiresInDays: 1 << 50}} {
		if e := decodeBody[OperationError](t, request(http.MethodPost, "/api/v1/tokens", body, alice), http.StatusBadRequest); e.Code != string(platform.CodeInvalidRequest) {
			t.Errorf("%+v = %+v", body, e)
		}
	}
	revoked := decodeBody[APIToken](t, request(http.MethodDelete, "/api/v1/tokens/"+created.Token.ID, nil, bearer), http.StatusOK)
	if revoked.Revoked == nil {
		t.Fatalf("revoked = %+v", revoked)
	}
	if e := decodeBody[Error](t, request(http.MethodGet, "/api/v1/tokens", nil, bearer), http.StatusUnauthorized); e.Error != "unauthenticated: the API token is invalid, expired or revoked" {
		t.Errorf("revoked token = %+v", e)
	}

	_, local := newServerOptions(t, Options{Platform: service})
	req, err = http.NewRequest(http.MethodGet, local.URL+"/api/v1/runs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "debugger.example.com"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("local mode answered Host debugger.example.com: %d", resp.StatusCode)
	}
}
