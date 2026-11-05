package platform

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/lineahq/linea/internal/debugger/store/memory"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestAPITokensActAsTheirPrincipal: a personal token acts as its user with
// the groups the user had, a service token as its service; only token.manage
// creates service tokens or sees and revokes other principals' tokens; a
// revoked token stops working; secrets are stored only hashed.
func TestAPITokensActAsTheirPrincipal(t *testing.T) {
	data := memory.NewData()
	access, err := NewPolicy([]Binding{
		{Subject: Subject{Group: "platform"}, Role: "viewer", Scope: ScopeAll},
		{Subject: Subject{Group: "admins"}, Role: "admin", Scope: ScopeAll},
		{Subject: Subject{Principal: "service:bot"}, Role: "viewer", Scope: ScopeAll},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := openPlatform(t, data, Options{Access: access})
	ctx := context.Background()
	alice := Caller{Principal: Principal{ID: UserID("idp", "alice"), Kind: PrincipalUser, Name: "Alice", Groups: []string{"platform"}}, Client: "http"}
	root := Caller{Principal: Principal{ID: UserID("idp", "root"), Kind: PrincipalUser, Groups: []string{"admins"}}, Client: "cli"}

	personal, err := s.CreateAPIToken(ctx, alice, CreateAPITokenRequest{Label: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(personal.Secret, TokenPrefix) || personal.Token.Principal != alice.Principal.ID || personal.Token.PrincipalKind != PrincipalUser ||
		personal.Token.Expires.Sub(personal.Token.Created) != DefaultTokenTTL {
		t.Fatalf("personal token = %+v", personal.Token)
	}
	p, err := s.AuthenticateToken(ctx, personal.Secret)
	if err != nil || p.ID != alice.Principal.ID || p.Kind != PrincipalUser || p.Name != "Alice" || len(p.Groups) != 1 || p.Groups[0] != "platform" {
		t.Fatalf("personal principal = %+v, %v", p, err)
	}
	if _, err := s.CreateAPIToken(ctx, alice, CreateAPITokenRequest{Label: "ci", Service: "ci"}); ErrorCode(err) != CodeForbidden {
		t.Fatalf("viewer creating a service token = %v", err)
	}
	service, err := s.CreateAPIToken(ctx, root, CreateAPITokenRequest{Label: "pipeline", Service: "ci", TTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.AuthenticateToken(ctx, service.Secret); err != nil || p.ID != "service:ci" || p.Kind != PrincipalService || len(p.Groups) != 0 {
		t.Fatalf("service principal = %+v, %v", p, err)
	}
	for name, req := range map[string]CreateAPITokenRequest{
		"no label":   {},
		"short":      {Label: "x", TTL: time.Minute},
		"long":       {Label: "x", TTL: MaxTokenTTL + time.Hour},
		"bad id":     {Label: "x", Service: "CI/CD"},
		"label ctrl": {Label: "a\x07"},
	} {
		if _, err := s.CreateAPIToken(ctx, root, req); ErrorCode(err) != CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	bot := Caller{Principal: Principal{ID: "service:bot", Kind: PrincipalService}, Client: "mcp"}
	if _, err := s.CreateAPIToken(ctx, bot, CreateAPITokenRequest{Label: "x"}); ErrorCode(err) != CodeInvalidRequest {
		t.Errorf("a service's personal token = %v", err)
	}

	own, err := s.ListAPITokens(ctx, alice, APITokenFilter{})
	if err != nil || len(own) != 1 || own[0].ID != personal.Token.ID {
		t.Fatalf("alice's tokens = %+v, %v", own, err)
	}
	if _, err := s.ListAPITokens(ctx, alice, APITokenFilter{All: true}); ErrorCode(err) != CodeForbidden {
		t.Fatalf("viewer listing all = %v", err)
	}
	all, err := s.ListAPITokens(ctx, root, APITokenFilter{All: true})
	if err != nil || len(all) != 2 || all[0].ID != service.Token.ID {
		t.Fatalf("all tokens = %+v, %v", all, err)
	}
	if _, err := s.RevokeAPIToken(ctx, alice, service.Token.ID); ErrorCode(err) != CodeForbidden {
		t.Fatalf("viewer revoking a service token = %v", err)
	}
	if _, err := s.RevokeAPIToken(ctx, alice, "0123456789abcdef0123456789abcdef"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("unknown token = %v", err)
	}
	revoked, err := s.RevokeAPIToken(ctx, alice, personal.Token.ID)
	if err != nil || revoked.Revoked == nil {
		t.Fatalf("revoke = %+v, %v", revoked, err)
	}
	for _, secret := range []string{personal.Secret, TokenPrefix + "unknown", "not-a-token"} {
		if _, err := s.AuthenticateToken(ctx, secret); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: %v", secret, err)
		}
	}
	if _, err := s.RevokeAPIToken(ctx, root, service.Token.ID); err != nil {
		t.Fatal(err)
	}

	stored, err := s.repository.Tokens(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), personal.Secret[len(TokenPrefix):]) || strings.Contains(string(raw), service.Secret[len(TokenPrefix):]) {
		t.Error("the repository holds a token secret")
	}
	s.Close()

	plain := New(Options{Access: access})
	defer plain.Close()
	if _, err := plain.CreateAPIToken(ctx, alice, CreateAPITokenRequest{Label: "x"}); ErrorCode(err) != CodeUnavailable {
		t.Errorf("without a workspace = %v", err)
	}
}

// TestTrustedHeadersOnlyFromProxies accepts identity headers only from a
// configured proxy, and Authenticators tries each credential in turn.
func TestTrustedHeadersOnlyFromProxies(t *testing.T) {
	headers := TrustedHeaders{Proxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, Issuer: "proxy",
		User: "X-Forwarded-User", Groups: "X-Forwarded-Groups", Name: "X-Forwarded-Name"}
	request := func(remote string, h map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
		r.RemoteAddr = remote
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	alice := map[string]string{"X-Forwarded-User": "alice", "X-Forwarded-Groups": "platform, sre,platform", "X-Forwarded-Name": "Alice A."}
	p, err := headers.Authenticate(request("10.1.2.3:4567", alice))
	if err != nil || p.ID != UserID("proxy", "alice") || p.Kind != PrincipalUser || p.Name != "Alice A." || strings.Join(p.Groups, ",") != "platform,sre" {
		t.Fatalf("from the proxy = %+v, %v", p, err)
	}
	if p, err := headers.Authenticate(request("[::ffff:10.0.0.1]:80", map[string]string{"X-Forwarded-User": "bob"})); err != nil || p.Name != "bob" || p.Groups != nil {
		t.Errorf("mapped IPv4 proxy = %+v, %v", p, err)
	}
	for name, r := range map[string]*http.Request{
		"another address": request("192.168.1.1:4567", alice),
		"no user":         request("10.1.2.3:4567", map[string]string{"X-Forwarded-Groups": "admins"}),
	} {
		if _, err := headers.Authenticate(r); !errors.Is(err, ErrNoCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := headers.Authenticate(request("10.1.2.3:4567", map[string]string{"X-Forwarded-User": strings.Repeat("a", 200)})); err == nil || errors.Is(err, ErrNoCredential) {
		t.Errorf("long user = %v", err)
	}

	s := openPlatform(t, memory.NewData(), Options{Access: mustPolicy(t, Binding{Subject: Subject{Group: "platform"}, Role: "viewer", Scope: ScopeAll})})
	tok, err := s.CreateAPIToken(context.Background(), Caller{Principal: Principal{ID: "user:carol", Kind: PrincipalUser, Groups: []string{"platform"}}, Client: "test"},
		CreateAPITokenRequest{Label: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	chain := Authenticators{s.TokenAuthenticator(), headers}
	bearer := request("10.1.2.3:4567", alice)
	bearer.Header.Set("Authorization", "Bearer "+tok.Secret)
	if p, err := chain.Authenticate(bearer); err != nil || p.ID != "user:carol" {
		t.Errorf("token first = %+v, %v", p, err)
	}
	bearer.Header.Set("Authorization", "Bearer "+TokenPrefix+"revoked")
	if _, err := chain.Authenticate(bearer); !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNoCredential) {
		t.Errorf("a bad token never falls through = %v", err)
	}
	if p, err := chain.Authenticate(request("10.1.2.3:4567", alice)); err != nil || p.ID != UserID("proxy", "alice") {
		t.Errorf("headers next = %+v, %v", p, err)
	}
	if _, err := chain.Authenticate(request("192.168.1.1:4567", alice)); !errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrNoCredential) {
		t.Errorf("nothing recognized = %v", err)
	}
}

func mustPolicy(t *testing.T, bindings ...Binding) *Policy {
	t.Helper()
	p, err := NewPolicy(bindings)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
