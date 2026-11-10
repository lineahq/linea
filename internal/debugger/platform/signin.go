package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// SignIn serves the sign-in endpoints under /auth/ for the server's sign-in
// methods, OIDC and the built-in admin account, and authenticates the web
// sessions they start and OIDC's bearer tokens.
//
//	GET  /auth/methods   the methods a sign-in page offers (SignInMethods)
//	GET  /auth/login     sign in and return to return_to: to the provider
//	                     when OIDC is the only method or method=oidc is
//	                     asked, otherwise to the web client's sign-in page
//	GET  /auth/callback  the provider's redirect (OIDC)
//	POST /auth/password  the admin password: a web session, or an API
//	                     token for the CLI (PasswordSignIn)
//	POST /auth/logout    end the session
//	GET  /auth/cli       what linea login needs (CLISignIn)
type SignIn struct {
	s        *Service
	oidc     *OIDC
	sessions *webSessions
}

// SignInOptions compose sign-in. SessionTTL and InsecureCookie apply to
// every web session; a server gives OIDC the same values.
type SignInOptions struct {
	// OIDC is the identity provider's sign-in, nil without one.
	OIDC *OIDC
	// SessionTTL is a web session's lifetime, DefaultSessionTTL when zero.
	SessionTTL time.Duration
	// InsecureCookie drops the Secure attribute, for a server reached over
	// plain HTTP on loopback only.
	InsecureCookie bool
}

// SignInPage is the web client's sign-in page; GET /auth/login sends a
// browser there when there is more than one method or no provider.
const SignInPage = "/sign-in"

// SignIn composes the sign-in endpoints. It needs a workspace, which keeps
// the sessions, and OIDC or the built-in admin account.
func (s *Service) SignIn(opts SignInOptions) (*SignIn, error) {
	switch {
	case s.repository == nil:
		return nil, errors.New("sign-in needs a workspace")
	case opts.OIDC == nil && s.admin == nil:
		return nil, errors.New("sign-in needs OIDC or the built-in admin account")
	case opts.SessionTTL < 0 || opts.SessionTTL > MaxSessionTTL:
		return nil, fmt.Errorf("a session lives at most %s", MaxSessionTTL)
	}
	if opts.SessionTTL == 0 {
		opts.SessionTTL = DefaultSessionTTL
	}
	return &SignIn{s: s, oidc: opts.OIDC, sessions: &webSessions{s: s, store: s.repository, ttl: opts.SessionTTL, insecure: opts.InsecureCookie}}, nil
}

// Authenticate accepts a session cookie, or with OIDC a bearer JWT from the
// provider; API tokens (odt_) are not its credential.
func (si *SignIn) Authenticate(r *http.Request) (Principal, error) {
	if si.oidc != nil {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if ok && strings.EqualFold(scheme, "Bearer") && !strings.HasPrefix(token, TokenPrefix) {
			return si.oidc.authenticateBearer(r.Context(), token)
		}
	}
	return si.sessions.authenticate(r)
}

// Methods are the sign-in methods this server offers.
func (si *SignIn) Methods() SignInMethods {
	m := SignInMethods{Password: si.s.admin != nil}
	if si.oidc != nil {
		m.OIDC = &OIDCMethod{Issuer: si.oidc.opts.Issuer}
	}
	return m
}

// SignInMethods is what a sign-in page offers: GET /auth/methods.
type SignInMethods struct {
	// Password is the built-in admin account's password form.
	Password bool `json:"password"`
	// OIDC is sign-in with the identity provider, absent without one.
	OIDC *OIDCMethod `json:"oidc,omitempty"`
}

// OIDCMethod names the identity provider a sign-in page links to.
type OIDCMethod struct {
	Issuer string `json:"issuer"`
}

// Handler serves the sign-in endpoints.
func (si *SignIn) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/methods", si.methods)
	mux.HandleFunc("GET /auth/login", si.login)
	mux.HandleFunc("POST /auth/password", si.password)
	mux.HandleFunc("POST /auth/logout", si.sessions.logout)
	mux.HandleFunc("GET /auth/cli", si.cli)
	if si.oidc != nil {
		mux.HandleFunc("GET /auth/callback", si.oidc.callback)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (si *SignIn) methods(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(si.Methods())
}

func (si *SignIn) login(w http.ResponseWriter, r *http.Request) {
	if si.oidc != nil && (si.s.admin == nil || r.URL.Query().Get("method") == "oidc") {
		si.oidc.login(w, r)
		return
	}
	http.Redirect(w, r, SignInPage+"?"+url.Values{"return_to": {returnTo(r)}}.Encode(), http.StatusFound)
}

// returnTo is the request's return_to when it is a path on this server,
// otherwise /.
func returnTo(r *http.Request) string {
	v := r.URL.Query().Get("return_to")
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") || strings.HasPrefix(v, "/\\") {
		return "/"
	}
	return v
}

// PasswordSignIn is the body of POST /auth/password. Client "cli" asks for
// an API token instead of a web session.
type PasswordSignIn struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Client   string `json:"client,omitempty"`
}

// PasswordToken answers a CLI's password sign-in: an API token for the
// admin account, shown once.
type PasswordToken struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// maxPasswordSignIn bounds the body of POST /auth/password.
const maxPasswordSignIn = 4 << 10

// passwordTokenLabel labels the API tokens linea login gets.
const passwordTokenLabel = "linea login"

func (si *SignIn) password(w http.ResponseWriter, r *http.Request) {
	if si.s.admin == nil {
		http.Error(w, "this server has no password sign-in", http.StatusNotFound)
		return
	}
	// A JSON body needs a CORS preflight, which this server never grants, so
	// another site cannot sign a browser in; the Origin check backs it up.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "send the sign-in as application/json", http.StatusUnsupportedMediaType)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if u, err := url.Parse(origin); err != nil || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "sign-in from another site is refused", http.StatusForbidden)
			return
		}
	}
	var in PasswordSignIn
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPasswordSignIn))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil || (in.Client != "" && in.Client != "web" && in.Client != "cli") {
		http.Error(w, `send {"user": "admin", "password": "…"}`, http.StatusBadRequest)
		return
	}
	client := r.RemoteAddr
	if host, _, err := net.SplitHostPort(client); err == nil {
		client = host
	}
	if err := si.s.admin.check(client, in.User, in.Password); err != nil {
		if si.s.logger != nil {
			si.s.logger.Warn("admin sign-in refused", zap.String("client", client), zap.String("reason", err.Error()))
		}
		if errors.Is(err, ErrAdminThrottled) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		http.Error(w, "wrong user name or password", http.StatusUnauthorized)
		return
	}
	if in.Client == "cli" {
		created, err := si.s.CreateAPIToken(r.Context(), Caller{Principal: adminPrincipal(), Client: "cli"}, CreateAPITokenRequest{Label: passwordTokenLabel})
		if err != nil {
			if si.s.logger != nil {
				si.s.logger.Warn("admin sign-in: creating the CLI's API token failed", zap.Error(err))
			}
			http.Error(w, "the server could not create an API token; try again shortly", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PasswordToken{Token: created.Secret, Expires: created.Token.Expires})
		return
	}
	if err := si.sessions.start(w, r, adminPrincipal()); err != nil {
		http.Error(w, "sign-in failed: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (si *SignIn) cli(w http.ResponseWriter, r *http.Request) {
	var out CLISignIn
	if si.oidc != nil && si.oidc.opts.CLIClientID != "" {
		out = si.oidc.cliSignIn()
	}
	out.Password = si.s.admin != nil
	if out.Issuer == "" && !out.Password {
		http.Error(w, "this server has no CLI sign-in; use an API token", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
