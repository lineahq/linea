package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// OpenID Connect sign-in. People sign in to
// the web client with the authorization code flow and PKCE and get a
// server-side session: the cookie holds a random secret, the workspace its
// SHA-256 (sessions.json). Unsafe requests with the cookie also send
// X-CSRF-Token, derived from the secret and read from GET /api/v1/session,
// which another site cannot read. The CLI and MCP present the provider's
// JWTs (ID or access tokens) as bearer tokens, verified against the
// provider's keys and the accepted audiences.

// OIDC session defaults and bounds.
const (
	DefaultSessionTTL = 12 * time.Hour
	MaxSessionTTL     = 7 * 24 * time.Hour
	loginTTL          = 10 * time.Minute
	// CSRFHeader carries a session's CSRF token on unsafe requests.
	CSRFHeader = "X-CSRF-Token"
)

// Cookie names: __Host- binds the session cookie to this origin over TLS.
const (
	sessionCookie         = "__Host-linea-session"
	insecureSessionCookie = "linea-session"
	loginCookie           = "linea-login"
)

// OIDCOptions configure sign-in with one OpenID Connect provider.
type OIDCOptions struct {
	// Issuer is the provider's issuer URL; its discovery document is read
	// when OIDC is composed.
	Issuer string
	// ClientID and ClientSecret are the web client's, a confidential client
	// whose redirect URL is RedirectURL (…/auth/callback on this server).
	ClientID, ClientSecret, RedirectURL string
	// CLIClientID is a public client allowed the device flow, which
	// linea login uses; empty disables login.
	CLIClientID string
	// Audiences are the audiences a bearer token may carry; empty accepts
	// ClientID and CLIClientID.
	Audiences []string
	// Scopes are requested at sign-in; empty is openid, profile and email.
	Scopes []string
	// GroupsClaim names the claim holding the user's groups, "groups" by
	// default; NameClaim the display name, "name" by default, falling back
	// to preferred_username and email.
	GroupsClaim, NameClaim string
	// SessionTTL is a web session's lifetime, DefaultSessionTTL when zero.
	SessionTTL time.Duration
	// InsecureCookie drops the Secure attribute, for a server reached over
	// plain HTTP on loopback only.
	InsecureCookie bool
	// HTTPClient reaches the provider; nil is http.DefaultClient.
	HTTPClient *http.Client
}

// OIDC authenticates sessions and bearer tokens and serves the sign-in
// endpoints under /auth/.
type OIDC struct {
	s        *Service
	opts     OIDCOptions
	ctx      context.Context // carries HTTPClient to the provider
	oauth    oauth2.Config
	web      *oidc.IDTokenVerifier
	bearer   *oidc.IDTokenVerifier
	sessions artifact.Sessions
	// logins holds sign-ins in progress, so any instance completes one.
	logins artifact.Logins
}

// OIDC reads the provider's discovery document and composes sign-in. It
// needs a workspace, which keeps the sessions.
func (s *Service) OIDC(ctx context.Context, opts OIDCOptions) (*OIDC, error) {
	if s.repository == nil {
		return nil, errors.New("OIDC sign-in needs a workspace")
	}
	switch {
	case opts.Issuer == "" || opts.ClientID == "" || opts.RedirectURL == "":
		return nil, errors.New("OIDC needs an issuer, a client ID and a redirect URL")
	case opts.SessionTTL < 0 || opts.SessionTTL > MaxSessionTTL:
		return nil, fmt.Errorf("an OIDC session lives at most %s", MaxSessionTTL)
	}
	if opts.SessionTTL == 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	if len(opts.Scopes) == 0 {
		opts.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	} else if !slices.Contains(opts.Scopes, oidc.ScopeOpenID) {
		opts.Scopes = append([]string{oidc.ScopeOpenID}, opts.Scopes...)
	}
	if len(opts.Audiences) == 0 {
		opts.Audiences = []string{opts.ClientID}
		if opts.CLIClientID != "" {
			opts.Audiences = append(opts.Audiences, opts.CLIClientID)
		}
	}
	if opts.GroupsClaim == "" {
		opts.GroupsClaim = "groups"
	}
	if opts.NameClaim == "" {
		opts.NameClaim = "name"
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	pctx := oidc.ClientContext(context.WithoutCancel(ctx), client)
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), opts.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC provider %s: %w", opts.Issuer, err)
	}
	return &OIDC{
		s: s, opts: opts, ctx: pctx, sessions: s.repository, logins: s.repository,
		oauth: oauth2.Config{ClientID: opts.ClientID, ClientSecret: opts.ClientSecret, RedirectURL: opts.RedirectURL,
			Endpoint: provider.Endpoint(), Scopes: opts.Scopes},
		web:    provider.VerifierContext(pctx, &oidc.Config{ClientID: opts.ClientID}),
		bearer: provider.VerifierContext(pctx, &oidc.Config{SkipClientIDCheck: true}),
	}, nil
}

// Authenticate accepts a bearer JWT from the provider or a session cookie;
// API tokens (odt_) are not its credential.
func (o *OIDC) Authenticate(r *http.Request) (Principal, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if ok && strings.EqualFold(scheme, "Bearer") && !strings.HasPrefix(token, TokenPrefix) {
		return o.authenticateBearer(r.Context(), token)
	}
	secret, ok := sessionSecret(r)
	if !ok {
		return Principal{}, ErrNoCredential
	}
	rec, err := o.sessions.SessionByHash(r.Context(), tokenHash(secret))
	if errors.Is(err, artifact.ErrNotFound) || (err == nil && !time.Now().Before(rec.Expires)) {
		return Principal{}, fmt.Errorf("%w: the session has ended; sign in again", ErrUnauthenticated)
	}
	if err != nil {
		return Principal{}, err
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrfToken(secret))) != 1 {
			return Principal{}, fmt.Errorf("%w: a signed-in request that changes something needs the %s header from GET /api/v1/session", ErrUnauthenticated, CSRFHeader)
		}
	}
	return Principal{ID: rec.Principal, Kind: PrincipalUser, Name: rec.Name, Groups: slices.Clone(rec.Groups)}, nil
}

var errBadBearer = fmt.Errorf("%w: the bearer token is invalid or expired, or not issued for this server", ErrUnauthenticated)

func (o *OIDC) authenticateBearer(ctx context.Context, raw string) (Principal, error) {
	tok, err := o.bearer.Verify(ctx, raw)
	if err != nil || !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(o.opts.Audiences, a) }) {
		return Principal{}, errBadBearer
	}
	return o.principal(tok)
}

// principal maps a verified token's claims to the user it names.
func (o *OIDC) principal(tok *oidc.IDToken) (Principal, error) {
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil || tok.Subject == "" {
		return Principal{}, errBadBearer
	}
	p := Principal{ID: UserID(tok.Issuer, tok.Subject), Kind: PrincipalUser, Name: tok.Subject}
	for _, c := range []string{o.opts.NameClaim, "preferred_username", "email"} {
		if v, ok := claims[c].(string); ok && strings.TrimSpace(v) != "" {
			p.Name = strings.TrimSpace(v)
			break
		}
	}
	switch v := claims[o.opts.GroupsClaim].(type) {
	case string:
		p.Groups = appendGroups(p.Groups, strings.Split(v, ",")...)
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok {
				p.Groups = appendGroups(p.Groups, s)
			}
		}
	}
	if !boundedIdentity(tok.Subject, p) {
		return Principal{}, fmt.Errorf("%w: the identity provider's claims are too long or not printable", ErrUnauthenticated)
	}
	return p, nil
}

// Handler serves the sign-in endpoints: GET /auth/login, GET
// /auth/callback, POST /auth/logout and GET /auth/cli.
func (o *OIDC) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", o.login)
	mux.HandleFunc("GET /auth/callback", o.callback)
	mux.HandleFunc("POST /auth/logout", o.logout)
	mux.HandleFunc("GET /auth/cli", o.cli)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (o *OIDC) login(w http.ResponseWriter, r *http.Request) {
	returnTo := r.URL.Query().Get("return_to")
	if !strings.HasPrefix(returnTo, "/") || strings.HasPrefix(returnTo, "//") || strings.HasPrefix(returnTo, "/\\") {
		returnTo = "/"
	}
	state, nonce, verifier := randomString(16), randomString(16), oauth2.GenerateVerifier()
	err := o.logins.SaveLogin(r.Context(), artifact.LoginRecord{Hash: tokenHash(state), Verifier: verifier, Nonce: nonce, ReturnTo: returnTo,
		Expires: time.Now().Add(loginTTL)})
	switch {
	case errors.Is(err, artifact.ErrLimit):
		http.Error(w, "too many sign-ins in progress; try again shortly", http.StatusServiceUnavailable)
		return
	case err != nil:
		if o.s.logger != nil {
			o.s.logger.Warn("oidc: recording a sign-in", zap.Error(err))
		}
		http.Error(w, "sign-in is unavailable; try again shortly", http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: state, Path: "/auth/", MaxAge: int(loginTTL.Seconds()),
		HttpOnly: true, Secure: !o.opts.InsecureCookie, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, o.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)), http.StatusFound)
}

func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "sign-in failed at the identity provider: "+e, http.StatusUnauthorized)
		return
	}
	state := q.Get("state")
	c, err := r.Cookie(loginCookie)
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "sign-in failed: this browser did not start it; sign in again", http.StatusBadRequest)
		return
	}
	p, err := o.logins.TakeLogin(r.Context(), tokenHash(state))
	switch {
	case errors.Is(err, artifact.ErrNotFound):
		http.Error(w, "sign-in failed: it took too long; sign in again", http.StatusBadRequest)
		return
	case err != nil:
		if o.s.logger != nil {
			o.s.logger.Warn("oidc: reading a sign-in", zap.Error(err))
		}
		http.Error(w, "sign-in is unavailable; try again shortly", http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: !o.opts.InsecureCookie, SameSite: http.SameSiteLaxMode})
	token, err := o.oauth.Exchange(o.requestContext(r), q.Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		http.Error(w, "sign-in failed: the identity provider refused the code", http.StatusUnauthorized)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	tok, err := o.web.Verify(o.requestContext(r), raw)
	if err != nil || subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(p.Nonce)) != 1 {
		http.Error(w, "sign-in failed: the identity provider's ID token is invalid", http.StatusUnauthorized)
		return
	}
	principal, err := o.principal(tok)
	if err != nil {
		http.Error(w, "sign-in failed: "+err.Error(), http.StatusUnauthorized)
		return
	}
	secret := randomString(32)
	now := time.Now().UTC().Truncate(time.Second)
	rec := artifact.SessionRecord{Hash: tokenHash(secret), Principal: principal.ID, Name: principal.Name, Groups: principal.Groups,
		Created: now, Expires: now.Add(o.opts.SessionTTL)}
	if err := o.sessions.SaveSession(r.Context(), rec); err != nil {
		if o.s.logger != nil {
			o.s.logger.Warn("saving a sign-in session failed", zap.Error(err))
		}
		http.Error(w, "sign-in failed: the server could not keep the session", http.StatusServiceUnavailable)
		return
	}
	http.SetCookie(w, o.cookie(secret, int(o.opts.SessionTTL.Seconds())))
	http.Redirect(w, r, p.ReturnTo, http.StatusSeeOther)
}

func (o *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	secret, ok := sessionSecret(r)
	if ok {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrfToken(secret))) != 1 {
			http.Error(w, "signing out needs the "+CSRFHeader+" header from GET /api/v1/session", http.StatusForbidden)
			return
		}
		if err := o.sessions.DeleteSession(r.Context(), tokenHash(secret)); err != nil {
			http.Error(w, "the server could not end the session", http.StatusServiceUnavailable)
			return
		}
	}
	http.SetCookie(w, o.cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// CLISignIn is what linea login needs to sign in with the device
// flow: GET /auth/cli.
type CLISignIn struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

func (o *OIDC) cli(w http.ResponseWriter, _ *http.Request) {
	if o.opts.CLIClientID == "" {
		http.Error(w, "this server has no CLI sign-in; use an API token", http.StatusNotFound)
		return
	}
	scopes := o.opts.Scopes
	if !slices.Contains(scopes, oidc.ScopeOfflineAccess) {
		scopes = append(slices.Clone(scopes), oidc.ScopeOfflineAccess)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(CLISignIn{Issuer: o.opts.Issuer, ClientID: o.opts.CLIClientID, Scopes: scopes})
}

func (o *OIDC) requestContext(r *http.Request) context.Context {
	return oidc.ClientContext(r.Context(), oauth2HTTPClient(o.ctx))
}

func oauth2HTTPClient(ctx context.Context) *http.Client {
	if c, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok {
		return c
	}
	return http.DefaultClient
}

func (o *OIDC) cookie(value string, maxAge int) *http.Cookie {
	name := sessionCookie
	if o.opts.InsecureCookie {
		name = insecureSessionCookie
	}
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: !o.opts.InsecureCookie, SameSite: http.SameSiteLaxMode}
}

// sessionSecret is the request's session cookie, under either name.
func sessionSecret(r *http.Request) (string, bool) {
	for _, name := range []string{sessionCookie, insecureSessionCookie} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value, true
		}
	}
	return "", false
}

// CSRFToken is the CSRF token of the request's session, empty without one.
// Only a page that can read GET /api/v1/session learns it.
func CSRFToken(r *http.Request) string {
	if secret, ok := sessionSecret(r); ok {
		return csrfToken(secret)
	}
	return ""
}

func csrfToken(secret string) string {
	sum := sha256.Sum256([]byte("csrf\x00" + secret))
	return hex.EncodeToString(sum[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
