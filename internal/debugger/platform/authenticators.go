package platform

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"unicode"
)

// Team-mode authenticators. Each recognizes one kind of
// credential and answers ErrNoCredential for a request without it, so
// Authenticators can try the next.

// ErrNoCredential is an authenticator's answer to a request that carries
// none of its credentials. It wraps ErrUnauthenticated.
var ErrNoCredential = fmt.Errorf("%w: no credential", ErrUnauthenticated)

// Authenticators tries each authenticator in order and returns the first
// answer other than ErrNoCredential. A request no authenticator recognizes
// is unauthenticated.
type Authenticators []Authenticator

// Authenticate identifies the request's principal.
func (c Authenticators) Authenticate(r *http.Request) (Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(r)
		if !errors.Is(err, ErrNoCredential) {
			return p, err
		}
	}
	return Principal{}, fmt.Errorf("%w: sign in or present an API token", ErrUnauthenticated)
}

// TokenAuthenticator accepts the service's API tokens presented as
// "Authorization: Bearer odt_...". Other bearer tokens are not its
// credential.
func (s *Service) TokenAuthenticator() Authenticator { return tokenAuthenticator{s} }

type tokenAuthenticator struct{ s *Service }

func (a tokenAuthenticator) Authenticate(r *http.Request) (Principal, error) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !strings.HasPrefix(token, TokenPrefix) {
		return Principal{}, ErrNoCredential
	}
	return a.s.AuthenticateToken(r.Context(), token)
}

// TrustedHeaders accepts the user an authenticating proxy names in request
// headers, only from the proxies' addresses. The proxy must set or remove
// these headers on every request it forwards.
type TrustedHeaders struct {
	// Proxies are the addresses that may send identity headers; from any
	// other address the headers are ignored.
	Proxies []netip.Prefix
	// Issuer names the proxy's identity source in user IDs:
	// UserID(Issuer, user).
	Issuer string
	// User, Groups and Name are header names. User is required; Groups is
	// comma-separated; Name is for display and defaults to the user.
	User, Groups, Name string
}

// Authenticate identifies the user the proxy names.
func (a TrustedHeaders) Authenticate(r *http.Request) (Principal, error) {
	addr, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || !slices.ContainsFunc(a.Proxies, func(p netip.Prefix) bool { return p.Contains(addr.Addr().Unmap()) }) {
		return Principal{}, ErrNoCredential
	}
	user := strings.TrimSpace(r.Header.Get(a.User))
	if user == "" {
		return Principal{}, ErrNoCredential
	}
	p := Principal{ID: UserID(a.Issuer, user), Kind: PrincipalUser, Name: user}
	if a.Name != "" {
		if name := strings.TrimSpace(r.Header.Get(a.Name)); name != "" {
			p.Name = name
		}
	}
	if a.Groups != "" {
		for _, v := range r.Header.Values(a.Groups) {
			p.Groups = appendGroups(p.Groups, strings.Split(v, ",")...)
		}
	}
	if !boundedIdentity(user, p) {
		return Principal{}, fmt.Errorf("%w: the proxy's identity headers are too long or not printable", ErrUnauthenticated)
	}
	return p, nil
}

// appendGroups appends the trimmed, non-empty groups not yet in groups.
func appendGroups(groups []string, add ...string) []string {
	for _, g := range add {
		if g = strings.TrimSpace(g); g != "" && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}
	return groups
}

// boundedIdentity reports whether an identity source's user name and the
// principal built from it fit the bounds records keep.
func boundedIdentity(user string, p Principal) bool {
	return len(user) <= maxPrincipalID && !strings.ContainsFunc(user, unicode.IsControl) && validLabel(p.Name, maxPrincipalName, false) &&
		len(p.Groups) <= maxGroups && !slices.ContainsFunc(p.Groups, func(g string) bool { return !validLabel(g, maxPrincipalGroup, false) })
}
