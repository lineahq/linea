package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/authz"
)

// API tokens are random secrets for the CLI, MCP agents
// and CI, stored only as their SHA-256 and shown once. A personal token acts
// as the user who created it, with the groups the user had then; a service
// token acts as a service principal, which bindings name as
// "service:<id>". Tokens expire and can be revoked.

// TokenPrefix starts every API token, so a bearer token can be told from
// an identity provider's.
const TokenPrefix = "odt_"

// Token lifetimes: DefaultTokenTTL when a request names none, at most
// MaxTokenTTL.
const (
	DefaultTokenTTL = 90 * 24 * time.Hour
	MaxTokenTTL     = 365 * 24 * time.Hour
	maxTokenLabel   = 128
)

var serviceID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// APIToken is a token's record; it never holds the secret.
type APIToken struct {
	ID            string
	Principal     string
	PrincipalKind PrincipalKind
	Label         string
	CreatedBy     string
	Created       time.Time
	Expires       time.Time
	Revoked       *time.Time
}

// CreateAPITokenRequest names a new token. Service, when set, creates a
// token for the service principal "service:<Service>" (token.manage);
// empty creates the caller's personal token. TTL zero is DefaultTokenTTL.
type CreateAPITokenRequest struct {
	Label   string
	Service string
	TTL     time.Duration
}

// CreatedAPIToken is a new token with its secret, which is never shown
// again.
type CreatedAPIToken struct {
	Token  APIToken
	Secret string
}

// APITokenFilter selects tokens: the caller's own, or with All every
// principal's (token.manage).
type APITokenFilter struct {
	All bool
}

// UserID is the stable principal ID of a user an identity source knows as
// subject: a hash of both, never the subject itself.
func UserID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "user:" + hex.EncodeToString(sum[:16])
}

func (s *Service) tokenStore() (artifact.Tokens, error) {
	if s.repository == nil {
		return nil, &Error{Code: CodeUnavailable, Message: "API tokens need a workspace"}
	}
	return s.repository, nil
}

func apiToken(r artifact.TokenRecord) APIToken {
	t := APIToken{ID: r.ID, Principal: r.Principal, PrincipalKind: PrincipalKind(r.Kind), Label: r.Label, CreatedBy: r.CreatedBy,
		Created: r.Created, Expires: r.Expires}
	if r.Revoked != nil {
		at := *r.Revoked
		t.Revoked = &at
	}
	return t
}

func tokenHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateAPIToken creates a personal or service token and returns its
// secret once.
func (s *Service) CreateAPIToken(ctx context.Context, caller Caller, req CreateAPITokenRequest) (_ *CreatedAPIToken, err error) {
	defer s.audit(&caller, "CreateAPIToken", &err)()
	if err := s.begin(ctx, caller, authz.TokenOwn); err != nil {
		return nil, err
	}
	if req.Service != "" {
		if err := s.authorize(caller, authz.TokenManage); err != nil {
			return nil, err
		}
	}
	tokens, err := s.tokenStore()
	if err != nil {
		return nil, err
	}
	if req.TTL == 0 {
		req.TTL = DefaultTokenTTL
	}
	switch {
	case req.Label == "" || len(req.Label) > maxTokenLabel || strings.ContainsFunc(req.Label, unicode.IsControl):
		return nil, &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf("invalid request: a token needs a label of at most %d printable characters", maxTokenLabel)}
	case req.TTL < time.Hour || req.TTL > MaxTokenTTL:
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: a token lives at least one hour and at most 365 days"}
	case req.Service != "" && !serviceID.MatchString(req.Service):
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: a service ID is 1 to 63 lowercase letters, digits and hyphens"}
	case req.Service == "" && caller.Principal.Kind != PrincipalUser:
		return nil, &Error{Code: CodeInvalidRequest, Message: "invalid request: personal API tokens are for signed-in users; name a service"}
	}
	var id [16]byte
	var secret [32]byte
	_, _ = rand.Read(id[:])
	_, _ = rand.Read(secret[:])
	created := time.Now().UTC().Truncate(time.Second)
	out := &CreatedAPIToken{Secret: TokenPrefix + base64.RawURLEncoding.EncodeToString(secret[:])}
	record := artifact.TokenRecord{ID: hex.EncodeToString(id[:]), Hash: tokenHash(out.Secret), Label: req.Label, CreatedBy: caller.Principal.ID,
		Created: created, Expires: created.Add(req.TTL)}
	if req.Service != "" {
		record.Principal, record.Kind, record.Name = "service:"+req.Service, artifact.TokenService, req.Service
	} else {
		p := caller.Principal
		record.Principal, record.Kind, record.Name, record.Groups = p.ID, artifact.TokenUser, p.Name, slices.Clone(p.Groups)
	}
	if err := tokens.SaveToken(ctx, record); err != nil {
		return nil, classify(err, CodeStorageError)
	}
	caller.touch(record.ID)
	out.Token = apiToken(record)
	return out, nil
}

// ListAPITokens lists tokens newest first, revoked and expired ones
// included.
func (s *Service) ListAPITokens(ctx context.Context, caller Caller, f APITokenFilter) (_ []APIToken, err error) {
	defer s.audit(&caller, "ListAPITokens", &err)()
	if err := s.begin(ctx, caller, authz.TokenOwn); err != nil {
		return nil, err
	}
	principal := caller.Principal.ID
	if f.All {
		if err := s.authorize(caller, authz.TokenManage); err != nil {
			return nil, err
		}
		principal = ""
	}
	tokens, err := s.tokenStore()
	if err != nil {
		return nil, err
	}
	records, err := tokens.Tokens(ctx, principal)
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	out := make([]APIToken, 0, len(records))
	for _, r := range records {
		out = append(out, apiToken(r))
	}
	return out, nil
}

// RevokeAPIToken revokes one of the caller's tokens, or with token.manage
// anyone's. It takes effect on the token's next use.
func (s *Service) RevokeAPIToken(ctx context.Context, caller Caller, id string) (_ *APIToken, err error) {
	defer s.audit(&caller, "RevokeAPIToken", &err)()
	if err := s.begin(ctx, caller, authz.TokenOwn); err != nil {
		return nil, err
	}
	tokens, err := s.tokenStore()
	if err != nil {
		return nil, err
	}
	records, err := tokens.Tokens(ctx, "")
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	i := slices.IndexFunc(records, func(r artifact.TokenRecord) bool { return r.ID == id })
	if i >= 0 && records[i].Principal != caller.Principal.ID {
		if err := s.authorize(caller, authz.TokenManage); err != nil {
			return nil, err
		}
	}
	if i < 0 {
		return nil, &Error{Code: CodeNotFound, Message: fmt.Sprintf("token %s not found", id)}
	}
	caller.touch(id)
	revoked, err := tokens.RevokeToken(ctx, id, time.Now())
	if err != nil {
		return nil, classify(err, CodeStorageError)
	}
	t := apiToken(revoked)
	return &t, nil
}

var errBadToken = fmt.Errorf("%w: the API token is invalid, expired or revoked", ErrUnauthenticated)

// AuthenticateToken returns the principal an API token acts as. An unknown,
// expired or revoked token is refused with ErrUnauthenticated; it is not an
// operation and is not audited.
func (s *Service) AuthenticateToken(ctx context.Context, secret string) (Principal, error) {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return Principal{}, errBadToken
	}
	tokens, err := s.tokenStore()
	if err != nil {
		return Principal{}, err
	}
	r, err := tokens.TokenByHash(ctx, tokenHash(secret))
	if errors.Is(err, artifact.ErrNotFound) || (err == nil && (r.Revoked != nil || !time.Now().Before(r.Expires))) {
		return Principal{}, errBadToken
	}
	if err != nil {
		return Principal{}, err
	}
	return Principal{ID: r.Principal, Kind: PrincipalKind(r.Kind), Name: r.Name, Groups: slices.Clone(r.Groups)}, nil
}
