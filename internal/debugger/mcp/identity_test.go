package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// tokenAuthenticator accepts the bearer tokens it maps to users.
type tokenAuthenticator map[string]string

func (a tokenAuthenticator) Authenticate(r *http.Request) (platform.Principal, error) {
	user, ok := a[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return platform.Principal{}, fmt.Errorf("%w: unknown token", platform.ErrUnauthenticated)
	}
	return platform.Principal{ID: "user:" + user, Kind: platform.PrincipalUser, Name: user}, nil
}

type bearerTransport string

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// TestToolsRunAsTheAuthenticatedPrincipal checks that an MCP endpoint with an
// authenticator needs a bearer token, that its tools reach the platform as
// the token's principal, and that the principal's role decides what it may
// do.
func TestToolsRunAsTheAuthenticatedPrincipal(t *testing.T) {
	access, err := platform.NewPolicy([]platform.Binding{
		{Subject: platform.Subject{Principal: "user:alice"}, Role: "engineer", Scope: platform.ScopeAll},
		{Subject: platform.Subject{Principal: "user:vera"}, Role: "viewer", Scope: platform.ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	service := platform.New(platform.Options{Access: access})
	t.Cleanup(service.Close)
	server := httptest.NewServer(New(Options{Platform: service, Authenticator: tokenAuthenticator{"t-alice": "alice", "t-vera": "vera"}}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	connect := func(token string) (*sdk.ClientSession, error) {
		client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
		return client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: bearerTransport(token)}}, nil)
	}
	if _, err := connect("t-mallory"); err == nil {
		t.Fatal("an unknown token connected")
	}
	resp, err := http.Post(server.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d", resp.StatusCode)
	}

	session, err := connect("t-alice")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	r, out := callTool(t, ctx, session, "linea_run", runArgs(platform.ModeQuick))
	alice := platform.Caller{Principal: platform.Principal{ID: "user:alice", Kind: platform.PrincipalUser}, Client: "test"}
	run, err := service.GetRun(ctx, alice, runID(t, r, out))
	if err != nil || run.Caller != (platform.Actor{Principal: "user:alice", Client: "mcp"}) {
		t.Fatalf("run caller = %+v, %v", run.Caller, err)
	}

	viewer, err := connect("t-vera")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = viewer.Close() })
	r, out = callTool(t, ctx, viewer, "linea_run", runArgs(platform.ModeQuick))
	assertToolError(t, r, out, platform.CodeForbidden, false)
	if out["message"] != "forbidden: this operation needs the run.submit permission" {
		t.Fatalf("viewer run = %v", out["message"])
	}
}

// TestRefusedAuthenticationIsLogged: a refused MCP request is answered with
// the request ID it is logged under, and the log never holds the token.
func TestRefusedAuthenticationIsLogged(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	service := platform.New(platform.Options{})
	t.Cleanup(service.Close)
	server := httptest.NewServer(New(Options{Platform: service, Authenticator: tokenAuthenticator{}, Logger: zap.New(core)}))
	t.Cleanup(server.Close)
	req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer secret-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	entries := logs.FilterMessage("authentication refused").All()
	if resp.StatusCode != http.StatusUnauthorized || len(entries) != 1 {
		t.Fatalf("status %d, %d entries", resp.StatusCode, len(entries))
	}
	fields := entries[0].ContextMap()
	if id := resp.Header.Get("X-Request-Id"); id == "" || fields["request_id"] != id || fields["interface"] != "mcp" ||
		fields["reason"] != "unauthenticated: unknown token" || strings.Contains(fmt.Sprint(fields), "secret-credential") {
		t.Fatalf("logged %v, answered %s", fields, resp.Header.Get("X-Request-Id"))
	}
}

// alternating sends each request to the next of its servers, as a load
// balancer in front of instances may.
type alternating struct {
	token   string
	servers []string
	next    int
}

func (a *alternating) RoundTrip(r *http.Request) (*http.Response, error) {
	target := a.servers[a.next%len(a.servers)]
	a.next++
	r = r.Clone(r.Context())
	u := *r.URL
	u.Host = strings.TrimPrefix(target, "http://")
	r.URL, r.Host = &u, u.Host
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

// TestAnAuthenticatedEndpointKeepsNoSessions: with authentication (team
// mode), each request of one client may reach another instance.
func TestAnAuthenticatedEndpointKeepsNoSessions(t *testing.T) {
	auth := tokenAuthenticator{"t-alice": "alice"}
	var servers []string
	for range 2 {
		service := platform.New(platform.Options{})
		t.Cleanup(service.Close)
		server := httptest.NewServer(New(Options{Platform: service, Authenticator: auth}))
		t.Cleanup(server.Close)
		servers = append(servers, server.URL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: servers[0], HTTPClient: &http.Client{Transport: &alternating{token: "t-alice", servers: servers}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	for range 3 {
		if tools, err := session.ListTools(ctx, nil); err != nil || len(tools.Tools) == 0 {
			t.Fatalf("tools = %v, %v", tools, err)
		}
	}
}
