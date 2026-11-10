package platform

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Web sessions, whichever way a person signed in (OIDC or the built-in
// admin password). The cookie holds a random secret, the workspace its
// SHA-256. Unsafe requests with the cookie also send X-CSRF-Token, derived
// from the secret and read from GET /api/v1/session, which another site
// cannot read.

var errSessionUnavailable = errors.New("the server could not keep the session")

type webSessions struct {
	s     *Service
	store artifact.Sessions
	ttl   time.Duration
	// insecure drops the Secure attribute and the __Host- prefix, for a
	// server reached over plain HTTP on loopback only.
	insecure bool
}

// authenticate accepts a session cookie.
func (w *webSessions) authenticate(r *http.Request) (Principal, error) {
	secret, ok := sessionSecret(r)
	if !ok {
		return Principal{}, ErrNoCredential
	}
	rec, err := w.store.SessionByHash(r.Context(), tokenHash(secret))
	if errors.Is(err, artifact.ErrNotFound) || (err == nil && !time.Now().Before(rec.Expires)) {
		return Principal{}, fmt.Errorf("%w: the session has ended; sign in again", ErrUnauthenticated)
	}
	if err != nil {
		return Principal{}, err
	}
	if err := w.s.enabledPrincipal(rec.Principal); err != nil {
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

// start keeps a session for p and sets its cookie.
func (w *webSessions) start(rw http.ResponseWriter, r *http.Request, p Principal) error {
	secret := randomString(32)
	now := time.Now().UTC().Truncate(time.Second)
	rec := artifact.SessionRecord{Hash: tokenHash(secret), Principal: p.ID, Name: p.Name, Groups: p.Groups,
		Created: now, Expires: now.Add(w.ttl)}
	if err := w.store.SaveSession(r.Context(), rec); err != nil {
		if w.s.logger != nil {
			w.s.logger.Warn("saving a sign-in session failed", zap.Error(err))
		}
		return errSessionUnavailable
	}
	http.SetCookie(rw, w.cookie(secret, int(w.ttl.Seconds())))
	return nil
}

// logout ends the request's session, if it has one, and clears the cookie.
func (w *webSessions) logout(rw http.ResponseWriter, r *http.Request) {
	secret, ok := sessionSecret(r)
	if ok {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrfToken(secret))) != 1 {
			http.Error(rw, "signing out needs the "+CSRFHeader+" header from GET /api/v1/session", http.StatusForbidden)
			return
		}
		if err := w.store.DeleteSession(r.Context(), tokenHash(secret)); err != nil {
			http.Error(rw, "the server could not end the session", http.StatusServiceUnavailable)
			return
		}
	}
	http.SetCookie(rw, w.cookie("", -1))
	rw.WriteHeader(http.StatusNoContent)
}

func (w *webSessions) cookie(value string, maxAge int) *http.Cookie {
	name := sessionCookie
	if w.insecure {
		name = insecureSessionCookie
	}
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: !w.insecure, SameSite: http.SameSiteLaxMode}
}
