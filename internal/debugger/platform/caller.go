package platform

import (
	"errors"
	"net/http"
	"strings"
	"unicode"
)

// LocalOwner is the ID of the principal of a platform composed without a
// server config: the CLI's in-process commands, and tests. A team server never
// authenticates it.
const LocalOwner = "local-owner"

// PrincipalKind says what kind of identity a principal is.
type PrincipalKind string

const (
	// PrincipalLocal is the owner of an in-process platform (LocalOwner).
	PrincipalLocal PrincipalKind = "local"
	// PrincipalUser is a person signed in to a team server.
	PrincipalUser PrincipalKind = "user"
	// PrincipalService is a non-person client of a team server, such as CI.
	PrincipalService PrincipalKind = "service"
)

// Bounds of a principal; an adapter's authenticator must stay within them.
const (
	maxPrincipalID    = 128
	maxPrincipalName  = 256
	maxPrincipalGroup = 256
	maxGroups         = 64
)

// Principal is an authenticated identity. The ID is stable and
// is what records keep; the name is for display only and never decides
// anything; groups come from the identity provider and select role bindings.
type Principal struct {
	ID     string
	Kind   PrincipalKind
	Name   string
	Groups []string
}

// LocalPrincipal is the owner of an in-process platform.
func LocalPrincipal() Principal {
	return Principal{ID: LocalOwner, Kind: PrincipalLocal, Name: LocalOwner}
}

var errInvalidCaller = errors.New("invalid request: an operation caller needs a principal and a client")

// Caller identifies who invokes an operation, through which interface and in
// which request, for authorization and audit. The composing adapter supplies
// it from authentication, never from request content.
type Caller struct {
	Principal Principal
	Client    string
	// RequestID correlates the operation with the adapter request; optional.
	RequestID string
	// Project is the project an operation creates or runs something in, and
	// whose saved work it reads; empty is the default project. Work derived
	// from a run or capture joins that run's or capture's project, and an
	// operation on an existing resource uses the resource's project.
	Project string
	// audit is the record of the operation in progress, set by its audit call.
	audit *auditRecord
}

// Local is the local owner invoking operations through client, such as "http"
// or "cli".
func Local(client string) Caller { return Caller{Principal: LocalPrincipal(), Client: client} }

// Actor is who did something, as records keep it: the principal's ID and the
// client it came through.
type Actor struct {
	Principal string `json:"principal"`
	Client    string `json:"client"`
}

// Actor is the caller as records keep it.
func (c Caller) Actor() Actor { return Actor{Principal: c.Principal.ID, Client: c.Client} }

func (c Caller) validate() error {
	p := c.Principal
	switch p.Kind {
	case PrincipalLocal, PrincipalUser, PrincipalService:
	default:
		return errInvalidCaller
	}
	if p.Kind == PrincipalLocal && p.ID != LocalOwner {
		return errInvalidCaller
	}
	if !validLabel(p.ID, maxPrincipalID, false) || !validLabel(c.Client, maxPrincipalID, false) ||
		!validLabel(p.Name, maxPrincipalName, true) || !validLabel(c.RequestID, maxPrincipalID, true) || len(p.Groups) > maxGroups {
		return errInvalidCaller
	}
	for _, g := range p.Groups {
		if !validLabel(g, maxPrincipalGroup, false) {
			return errInvalidCaller
		}
	}
	return nil
}

func (a Actor) validate() error {
	if !validLabel(a.Principal, maxPrincipalID, false) || !validLabel(a.Client, maxPrincipalID, false) {
		return errInvalidCaller
	}
	return nil
}

func validLabel(v string, limit int, optional bool) bool {
	if v == "" {
		return optional
	}
	return len(v) <= limit && !strings.ContainsFunc(v, unicode.IsControl)
}

// ErrUnauthenticated is an authenticator's refusal: the request carries no
// valid identity. Adapters answer it as unauthenticated.
var ErrUnauthenticated = errors.New("unauthenticated")

// Authenticator identifies the principal behind an HTTP request. Adapters
// authenticate every request with it; the platform authorizes the resulting
// caller. A refusal wraps ErrUnauthenticated.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// LocalAuthenticator makes every request the local owner's. It is the API
// handler's default for tests and embedding; linea-server always passes its
// configured authenticators.
type LocalAuthenticator struct{}

// Authenticate returns the local owner.
func (LocalAuthenticator) Authenticate(*http.Request) (Principal, error) {
	return LocalPrincipal(), nil
}

// Unauthenticated maps an authenticator's failure to an operation error. A
// refusal (ErrUnauthenticated) is unauthenticated with the authenticator's
// message, which must be safe to show; any other failure, such as an
// unreachable identity provider, is unavailable and hides its cause.
func Unauthenticated(err error) error {
	if errors.Is(err, ErrUnauthenticated) {
		return &Error{Code: CodeUnauthenticated, Message: err.Error(), cause: err}
	}
	return &Error{Code: CodeUnavailable, Message: "authentication is unavailable; retry later", Retryable: true, cause: err}
}
