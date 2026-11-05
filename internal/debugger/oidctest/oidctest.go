// Package oidctest is a fake OpenID Connect provider for tests: discovery,
// signing keys, the authorization code flow with PKCE, the device flow and
// refresh tokens, all for one configurable user. It signs RS256 tokens.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// User is who signs in at the provider.
type User struct {
	Subject, Name string
	Groups        []string
}

// Provider is a running fake provider; Issuer is its URL.
type Provider struct {
	Issuer string
	// TokenTTL is the lifetime of the tokens it issues, an hour by default.
	TokenTTL time.Duration
	// PendingPolls is how many device-flow polls answer
	// authorization_pending before the user approves.
	PendingPolls int

	signer jose.Signer
	keys   jose.JSONWebKeySet

	mu      sync.Mutex
	user    User
	codes   map[string]grant
	devices map[string]*grant
	refresh map[string]grant
}

type grant struct {
	clientID, nonce, challenge, redirect string
	user                                 User
	polls                                int
}

// New starts a provider whose user is alice in the platform group.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{
		TokenTTL: time.Hour, signer: signer,
		keys:  jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: string(jose.RS256), Use: "sig"}}},
		user:  User{Subject: "alice", Name: "Alice", Groups: []string{"platform"}},
		codes: map[string]grant{}, devices: map[string]*grant{}, refresh: map[string]grant{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, p.keys) })
	mux.HandleFunc("GET /auth", p.authorize)
	mux.HandleFunc("POST /device/code", p.deviceCode)
	mux.HandleFunc("POST /token", p.token)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p.Issuer = srv.URL
	return p
}

// SetUser changes who signs in next.
func (p *Provider) SetUser(u User) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.user = u
}

// Token signs a token for the user with the audience and extra claims.
func (p *Provider) Token(t testing.TB, audience string, u User, ttl time.Duration, extra map[string]any) string {
	t.Helper()
	raw, err := p.sign(audience, "", u, ttl, extra)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (p *Provider) sign(audience, nonce string, u User, ttl time.Duration, extra map[string]any) (string, error) {
	now := time.Now()
	claims := map[string]any{"iss": p.Issuer, "sub": u.Subject, "aud": audience, "iat": now.Unix(), "exp": now.Add(ttl).Unix()}
	if u.Name != "" {
		claims["name"] = u.Name
	}
	if u.Groups != nil {
		claims["groups"] = u.Groups
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	for k, v := range extra {
		claims[k] = v
	}
	payload, _ := json.Marshal(claims)
	jws, err := p.signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": p.Issuer, "authorization_endpoint": p.Issuer + "/auth", "token_endpoint": p.Issuer + "/token",
		"device_authorization_endpoint": p.Issuer + "/device/code", "jwks_uri": p.Issuer + "/keys",
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

// authorize signs the current user in at once and redirects back with a
// code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := random()
	p.mu.Lock()
	p.codes[code] = grant{clientID: q.Get("client_id"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: redirect.String(), user: p.user}
	p.mu.Unlock()
	v := redirect.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	redirect.RawQuery = v.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (p *Provider) deviceCode(w http.ResponseWriter, r *http.Request) {
	code := random()
	p.mu.Lock()
	p.devices[code] = &grant{clientID: r.PostFormValue("client_id"), user: p.user}
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"device_code": code, "user_code": "WDJB-MJHT", "verification_uri": p.Issuer + "/device",
		"verification_uri_complete": p.Issuer + "/device?user_code=WDJB-MJHT", "expires_in": 600, "interval": 1})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	clientID, _, ok := r.BasicAuth()
	if !ok {
		clientID = r.PostFormValue("client_id")
	}
	p.mu.Lock()
	var g grant
	found := false
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		g, found = p.codes[r.PostFormValue("code")]
		delete(p.codes, r.PostFormValue("code"))
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		found = found && g.redirect == r.PostFormValue("redirect_uri") && g.challenge == base64.RawURLEncoding.EncodeToString(sum[:])
	case "urn:ietf:params:oauth:grant-type:device_code":
		d, ok := p.devices[r.PostFormValue("device_code")]
		if ok && d.polls < p.PendingPolls {
			d.polls++
			p.mu.Unlock()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
			return
		}
		if ok {
			g, found = *d, true
			delete(p.devices, r.PostFormValue("device_code"))
		}
	case "refresh_token":
		g, found = p.refresh[r.PostFormValue("refresh_token")]
		delete(p.refresh, r.PostFormValue("refresh_token"))
	}
	found = found && g.clientID == clientID
	refresh := random()
	if found {
		p.refresh[refresh] = grant{clientID: g.clientID, user: g.user}
	}
	p.mu.Unlock()
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	id, err := p.sign(clientID, g.nonce, g.user, p.TokenTTL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access, _ := p.sign(clientID, "", g.user, p.TokenTTL, nil)
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "id_token": id,
		"refresh_token": refresh, "expires_in": int(p.TokenTTL.Seconds())})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
