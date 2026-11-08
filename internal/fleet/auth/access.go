package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/fleet/fleet"
)

var ErrUnauthorized = errors.New("credential is invalid, expired or revoked")
var ErrForbidden = errors.New("credential does not permit this operation")

type Principal struct{ ID, Role string }
type Authorizer interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
	Active(context.Context, []Principal) (map[string]bool, error)
}

type Static struct {
	Bearer Bearer
	Role   string
}

func (s Static) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	if !s.Bearer.Valid(r) {
		return Principal{}, ErrUnauthorized
	}
	return Principal{ID: "memory-" + s.Role, Role: s.Role}, nil
}

// Introspect recognizes the development agent token without labels.
func (s Static) Introspect(_ context.Context, tokenSHA256 [32]byte) (Introspection, error) {
	if s.Role != "agent" || subtle.ConstantTimeCompare(tokenSHA256[:], s.Bearer.digest[:]) != 1 {
		return Introspection{}, nil
	}
	return Introspection{Active: true, CredentialID: "memory-agent", Role: "agent"}, nil
}

func (s Static) Active(_ context.Context, ps []Principal) (map[string]bool, error) {
	r := map[string]bool{}
	for _, p := range ps {
		r[p.ID] = true
	}
	return r, nil
}

type Credential struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	// Labels are bounded agent-credential metadata that plugins interpret.
	Labels map[string]string `json:"labels,omitempty"`
	// RemoteConfig lets config management send configuration to agents
	// using this credential. Only agent credentials can carry it.
	RemoteConfig bool      `json:"remote_config"`
	CreatedAt    time.Time `json:"created_at"`
	// ExpiresAt is nil for an admin or reader credential that is valid
	// until revoked. Agent credentials always expire.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	Hash      []byte     `json:"-"`
}

// Active reports whether the credential is neither revoked nor expired at now.
func (c Credential) Active(now time.Time) bool {
	return c.RevokedAt == nil && (c.ExpiresAt == nil || c.ExpiresAt.After(now))
}

type CredentialPage struct {
	Items     []Credential `json:"items"`
	NextAfter string       `json:"next_after,omitempty"`
}
type CredentialRequest struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Role         string            `json:"role"`
	Labels       map[string]string `json:"labels,omitempty"`
	RemoteConfig bool              `json:"remote_config,omitempty"`
	// ExpiresAt is required for agent credentials. Omitted, an admin or
	// reader credential is valid until revoked.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Label bounds keep a credential's labels within 4 KiB of JSON.
const (
	MaxLabels          = 16
	MaxLabelKeyBytes   = 64
	MaxLabelValueBytes = 128
)

// Introspection answers whether a presented token is an active agent
// credential. Inactive answers carry no other fields.
type Introspection struct {
	Active       bool              `json:"active"`
	CredentialID string            `json:"credential_id,omitempty"`
	Role         string            `json:"role,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	ExpiresAt    *time.Time        `json:"expires_at,omitempty"`
}

// Introspector resolves the SHA-256 of a presented token. Plugins hash the
// bearer a Collector presented to them; core never receives the token.
type Introspector interface {
	Introspect(ctx context.Context, tokenSHA256 [32]byte) (Introspection, error)
}
type Issued struct {
	Credential Credential `json:"credential"`
	Token      string     `json:"token"`
}
type CredentialRepository interface {
	Credential(context.Context, string) (Credential, error)
	ListCredentials(context.Context, string, int) (CredentialPage, error)
	InsertCredential(context.Context, string, Credential) error
	RevokeCredential(context.Context, string, string) error
	// SetCredentialRemoteConfig changes an agent credential's remote
	// config opt-in.
	SetCredentialRemoteConfig(ctx context.Context, actor, id string, enabled bool) (Credential, error)
	ActiveCredentialIDs(context.Context, []string) (map[string]bool, error)
	CredentialByHash(context.Context, []byte) (Credential, error)
}
type Service struct{ Repository CredentialRepository }

// NameRule is the reason for a name SafeName refuses.
const NameRule = "name must be 1 to 128 bytes without control characters or leading or trailing spaces"

// LabelsRule is the reason for labels ValidLabels refuses.
var LabelsRule = fmt.Sprintf("labels allow at most %d entries, keys of letters, digits and . _ - / up to %d bytes, and printable values up to %d bytes", MaxLabels, MaxLabelKeyBytes, MaxLabelValueBytes)

func SafeName(name string) bool {
	return len(name) > 0 && len(name) <= 128 && strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}

func Generate(req CredentialRequest, now time.Time) (Issued, error) {
	switch {
	case !fleet.ValidID(req.ID):
		return Issued{}, fleet.Invalid("id " + fleet.IDRule)
	case !SafeName(req.Name):
		return Issued{}, fleet.Invalid(NameRule)
	case req.Role != "admin" && req.Role != "reader" && req.Role != "agent":
		return Issued{}, fleet.Invalid("role must be admin, reader or agent")
	case req.ExpiresAt == nil && req.Role == "agent":
		return Issued{}, fleet.Invalid("expires_at is required for agent credentials")
	case req.ExpiresAt != nil && (!req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(365*24*time.Hour))):
		return Issued{}, fleet.Invalid("expires_at must be in the future and at most 365 days away")
	case !ValidLabels(req.Labels):
		return Issued{}, fleet.Invalid(LabelsRule)
	case req.Role != "agent" && (len(req.Labels) > 0 || req.RemoteConfig):
		return Issued{}, fleet.Invalid("labels and remote_config are for agent credentials only")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Issued{}, fleet.ErrUnavailable
	}
	token := "of1_" + req.ID + "_" + hex.EncodeToString(secret)
	hash := sha256.Sum256([]byte(token))
	c := Credential{ID: req.ID, Name: req.Name, Role: req.Role, RemoteConfig: req.RemoteConfig, CreatedAt: now.UTC(), Hash: hash[:]}
	if req.ExpiresAt != nil {
		expires := req.ExpiresAt.UTC()
		c.ExpiresAt = &expires
	}
	if len(req.Labels) > 0 {
		c.Labels = maps.Clone(req.Labels)
	}
	return Issued{Credential: c, Token: token}, nil
}

// TokenRule describes a credential token a deployment generates itself.
const TokenRule = "token must be of1_, 32 lowercase hexadecimal characters, _ and 64 lowercase hexadecimal characters"

// TokenID returns the credential ID a well-formed token names.
func TokenID(token string) (string, bool) {
	if len(token) != 101 || !strings.HasPrefix(token, "of1_") || token[36] != '_' {
		return "", false
	}
	id, secret := token[4:36], token[37:]
	if !fleet.ValidID(id) {
		return "", false
	}
	b, err := hex.DecodeString(secret)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != secret {
		return "", false
	}
	return id, true
}

// Install is Generate for a token the deployment generated instead of fleet,
// so the deployment can hand the same token to its clients (bootstrap).
func Install(req CredentialRequest, token string, now time.Time) (Issued, error) {
	id, ok := TokenID(token)
	if !ok {
		return Issued{}, fleet.Invalid(TokenRule)
	}
	req.ID = id
	i, err := Generate(req, now)
	if err != nil {
		return Issued{}, err
	}
	hash := sha256.Sum256([]byte(token))
	i.Credential.Hash = hash[:]
	i.Token = token
	return i, nil
}

func (s *Service) Issue(ctx context.Context, actor string, req CredentialRequest) (Issued, error) {
	i, err := Generate(req, time.Now())
	if err != nil {
		return Issued{}, err
	}
	if err := s.Repository.InsertCredential(ctx, actor, i.Credential); err != nil {
		return Issued{}, err
	}
	return i, nil
}
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return Principal{}, ErrUnauthorized
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) != 101 || !strings.HasPrefix(token, "of1_") || token[36] != '_' {
		return Principal{}, ErrUnauthorized
	}
	id := token[4:36]
	if !fleet.ValidID(id) {
		return Principal{}, ErrUnauthorized
	}
	c, err := s.Repository.Credential(ctx, id)
	if errors.Is(err, fleet.ErrNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	hash := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(hash[:], c.Hash) != 1 || !c.Active(time.Now()) {
		return Principal{}, ErrUnauthorized
	}
	return Principal{ID: c.ID, Role: c.Role}, nil
}

// ValidLabels bounds label count, keys and printable values.
func ValidLabels(labels map[string]string) bool {
	if len(labels) > MaxLabels {
		return false
	}
	for k, v := range labels {
		if len(k) > MaxLabelKeyBytes || !fleet.ValidKey(k) || len(v) > MaxLabelValueBytes || !fleet.ValidValue(v) {
			return false
		}
	}
	return true
}

func (s *Service) Introspect(ctx context.Context, tokenSHA256 [32]byte) (Introspection, error) {
	c, err := s.Repository.CredentialByHash(ctx, tokenSHA256[:])
	if errors.Is(err, fleet.ErrNotFound) {
		return Introspection{}, nil
	}
	if err != nil {
		return Introspection{}, err
	}
	return introspection(c, time.Now()), nil
}

// introspection exposes only active agent credentials.
func introspection(c Credential, now time.Time) Introspection {
	if c.Role != "agent" || !c.Active(now) || c.ExpiresAt == nil {
		return Introspection{}
	}
	expires := *c.ExpiresAt
	return Introspection{Active: true, CredentialID: c.ID, Role: c.Role, Labels: maps.Clone(c.Labels), ExpiresAt: &expires}
}

func (s *Service) Active(ctx context.Context, ps []Principal) (map[string]bool, error) {
	ids := make([]string, 0, len(ps))
	seen := make(map[string]bool, len(ps))
	for _, p := range ps {
		if !seen[p.ID] {
			ids = append(ids, p.ID)
			seen[p.ID] = true
		}
	}
	return s.Repository.ActiveCredentialIDs(ctx, ids)
}
