package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lineahq/linea/internal/debugger/engine"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// headerAuthenticator signs requests in as the user named by X-Test-User, a
// member of the group X-Test-Group or else "platform". A request without a
// user is refused; X-Test-Down stands for an unreachable identity provider.
type headerAuthenticator struct{}

func (headerAuthenticator) Authenticate(r *http.Request) (platform.Principal, error) {
	if r.Header.Get("X-Test-Down") != "" {
		return platform.Principal{}, errors.New("identity provider at 10.0.0.9 timed out")
	}
	user := r.Header.Get("X-Test-User")
	if user == "" {
		return platform.Principal{}, fmt.Errorf("%w: sign in first", platform.ErrUnauthenticated)
	}
	group := r.Header.Get("X-Test-Group")
	if group == "" {
		group = "platform"
	}
	return platform.Principal{ID: "user:" + user, Kind: platform.PrincipalUser, Name: user, Groups: []string{group}}, nil
}

// bindPlatform is a team policy granting the group "platform" one role.
func bindPlatform(t *testing.T, role string) *platform.Policy {
	t.Helper()
	p, err := platform.NewPolicy([]platform.Binding{{Subject: platform.Subject{Group: "platform"}, Role: role, Scope: platform.ScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func identityRequest(t *testing.T, method, url string, body any, header map[string]string) *http.Response {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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

// TestRequestsCarryTheAuthenticatedCaller checks that every API request is
// authenticated, that a refusal is a 401 and an unreachable provider a 503
// that hides its cause, and that the platform records the principal the
// authenticator returned with the request's ID.
func TestRequestsCarryTheAuthenticatedCaller(t *testing.T) {
	service := platform.New(platform.Options{Access: bindPlatform(t, "engineer"), Paths: platform.PathPolicy{Unrestricted: true}, Execute: func(_ context.Context, req engine.Request) (*engine.Report, error) {
		return &engine.Report{Result: &runresult.Result{RunID: req.RunID, Status: runresult.StatusCompleted}}, nil
	}})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}})

	refused := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, nil)
	if e := decodeBody[Error](t, refused, http.StatusUnauthorized); e.Error != "unauthenticated: sign in first" {
		t.Fatalf("refusal = %q", e.Error)
	}
	down := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, map[string]string{"X-Test-Down": "1"})
	if e := decodeBody[Error](t, down, http.StatusServiceUnavailable); e.Error != "authentication is unavailable; retry later" {
		t.Fatalf("an unreachable provider = %q", e.Error)
	}

	resp := identityRequest(t, http.MethodPost, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario), map[string]string{"X-Test-User": "alice"})
	id := resp.Header.Get("X-Request-Id")
	created := decodeBody[Run](t, resp, http.StatusAccepted)
	if !regexp.MustCompile(`^req-[0-9a-f]{16}$`).MatchString(id) {
		t.Fatalf("request ID = %q", id)
	}
	run, err := service.GetRun(context.Background(), platform.Caller{Principal: platform.Principal{ID: "user:alice", Kind: platform.PrincipalUser, Groups: []string{"platform"}}, Client: "test"}, created.ID)
	if err != nil || run.Caller != (platform.Actor{Principal: "user:alice", Client: "http"}) {
		t.Fatalf("run caller = %+v, %v", run.Caller, err)
	}
}

// TestLocalModeIsTheLocalOwner keeps local mode as it was: without an
// authenticator every request is the local owner's.
func TestLocalModeIsTheLocalOwner(t *testing.T) {
	s, srv := newServer(t)
	created := decodeBody[Run](t, post(t, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario)), http.StatusAccepted)
	run, err := s.runs.GetRun(context.Background(), caller, created.ID)
	if err != nil || run.Caller != platform.Local("http").Actor() {
		t.Fatalf("run caller = %+v, %v", run.Caller, err)
	}
}

// TestRolesDecideWhatAUserMayDo checks authorization through HTTP: a viewer
// reads but cannot submit, a user without a binding is refused, and both
// refusals are 403 naming the missing permission.
func TestRolesDecideWhatAUserMayDo(t *testing.T) {
	service := platform.New(platform.Options{Access: bindPlatform(t, "viewer")})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}})

	alice := map[string]string{"X-Test-User": "alice"}
	decodeBody[json.RawMessage](t, identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, alice), http.StatusOK)
	submit := identityRequest(t, http.MethodPost, srv.URL+"/api/v1/runs", inlined(t, multisignalConfig, multisignalScenario), alice)
	if e := decodeBody[Error](t, submit, http.StatusForbidden); e.Error != "forbidden: this operation needs the run.submit permission" {
		t.Fatalf("viewer submit = %q", e.Error)
	}
	mallory := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, map[string]string{"X-Test-User": "mallory", "X-Test-Group": "sales"})
	if e := decodeBody[Error](t, mallory, http.StatusForbidden); e.Error != "forbidden: this operation needs the run.read permission" {
		t.Fatalf("unbound list = %q", e.Error)
	}
}

// TestRefusedAuthenticationIsLogged: a refused request is answered with the
// request ID it is logged under, with the client, interface and reason, and
// the log never holds the credential presented.
func TestRefusedAuthenticationIsLogged(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	service := platform.New(platform.Options{Access: bindPlatform(t, "viewer")})
	t.Cleanup(service.Close)
	_, srv := newServerOptions(t, Options{Platform: service, Authenticator: headerAuthenticator{}, Logger: zap.New(core)})
	resp := identityRequest(t, http.MethodGet, srv.URL+"/api/v1/runs", nil, map[string]string{"Authorization": "Bearer odt_secret-credential"})
	resp.Body.Close()
	entries := logs.FilterMessage("authentication refused").All()
	if resp.StatusCode != http.StatusUnauthorized || len(entries) != 1 {
		t.Fatalf("status %d, %d entries", resp.StatusCode, len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["request_id"] != resp.Header.Get("X-Request-Id") || fields["interface"] != "http" || fields["reason"] != "unauthenticated: sign in first" ||
		!strings.HasPrefix(fmt.Sprint(fields["client"]), "127.0.0.1:") || strings.Contains(fmt.Sprint(fields), "secret-credential") {
		t.Fatalf("logged %v, answered %s", fields, resp.Header.Get("X-Request-Id"))
	}
}
