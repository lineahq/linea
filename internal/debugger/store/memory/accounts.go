package memory

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/lineahq/linea/internal/debugger/artifact"
)

// The audit log, tokens and sessions have their own bounds and sit outside
// the storage limit, so a full store never locks people out.
const (
	maxAuditField     = 256
	maxAuditResources = 16
	maxTokens         = 1000
	maxTokenField     = 256
	maxTokenGroups    = 64
	tokenRetention    = 30 * 24 * time.Hour
	maxSessions       = 10000
)

func validateAudit(e artifact.AuditEvent) error {
	fields := []string{e.Principal, e.PrincipalKind, e.Client, e.Operation, e.Outcome}
	for _, f := range fields {
		if f == "" {
			return invalid("audit event needs a principal, kind, client, operation and outcome")
		}
	}
	if e.Outcome != artifact.AuditAllowed && e.Outcome != artifact.AuditDenied {
		return invalid("audit outcome must be allowed or denied")
	}
	if e.Time.IsZero() || len(e.Resources) > maxAuditResources {
		return invalid("audit event needs a time and at most 16 resources")
	}
	for _, f := range append(append(fields, e.RequestID, e.Permission, e.Scope, e.Code), e.Resources...) {
		if len(f) > maxAuditField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("audit field too long or not printable")
		}
	}
	return nil
}

func matchAudit(q artifact.AuditQuery, e artifact.AuditEvent) bool {
	return (q.Principal == "" || e.Principal == q.Principal) && (q.Operation == "" || e.Operation == q.Operation) &&
		(q.Outcome == "" || e.Outcome == q.Outcome) && (q.Scope == "" || e.Scope == q.Scope) && !e.Time.Before(q.Since)
}

// AppendAudit records an event and returns it with its sequence, one higher
// than the previous event's. Events beyond retention are dropped.
func (s *Store) AppendAudit(ctx context.Context, e artifact.AuditEvent) (artifact.AuditEvent, error) {
	if err := validateAudit(e); err != nil {
		return artifact.AuditEvent{}, err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.AuditEvent{}, err
	}
	d.auditEnd++
	e.Seq, e.Time, e.Resources = d.auditEnd, e.Time.UTC(), slices.Clone(e.Resources)
	d.audit = append(d.audit, e)
	if over := len(d.audit) - s.limits.AuditEvents; over > 0 {
		d.audit = slices.Clone(d.audit[over:])
	}
	e.Resources = slices.Clone(e.Resources)
	return e, nil
}

// AuditEvents returns the events the query selects, newest first, within
// retention.
func (s *Store) AuditEvents(ctx context.Context, q artifact.AuditQuery) ([]artifact.AuditEvent, error) {
	if q.Limit <= 0 {
		return nil, invalid("audit query needs a positive limit")
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	var oldest uint64 = 1
	if d.auditEnd > uint64(s.limits.AuditEvents) {
		oldest = d.auditEnd - uint64(s.limits.AuditEvents) + 1
	}
	if since := time.Now().Add(-s.limits.AuditAge); q.Since.Before(since) {
		q.Since = since
	}
	var out []artifact.AuditEvent
	for i := len(d.audit) - 1; i >= 0 && len(out) < q.Limit; i-- {
		e := d.audit[i]
		if e.Seq < oldest {
			break
		}
		if (q.Before == 0 || e.Seq < q.Before) && matchAudit(q, e) {
			e.Resources = slices.Clone(e.Resources)
			out = append(out, e)
		}
	}
	return out, nil
}

func validateToken(t artifact.TokenRecord) error {
	if !validID(t.ID) {
		return invalid("token ID must be 32 lowercase hex digits")
	}
	if h, err := hex.DecodeString(t.Hash); err != nil || len(h) != 32 || t.Hash != strings.ToLower(t.Hash) {
		return invalid("token hash must be a lowercase hex SHA-256")
	}
	if t.Kind != artifact.TokenUser && t.Kind != artifact.TokenService {
		return invalid("token kind must be user or service")
	}
	if t.Principal == "" || t.Label == "" || t.CreatedBy == "" || len(t.Groups) > maxTokenGroups {
		return invalid("token needs a principal, label and creator, and at most 64 groups")
	}
	if t.Created.IsZero() || !t.Expires.After(t.Created) || t.Revoked != nil {
		return invalid("token needs a creation time before its expiry and no revocation")
	}
	for _, f := range append([]string{t.Principal, t.Name, t.Label, t.CreatedBy}, t.Groups...) {
		if len(f) > maxTokenField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("token field too long or not printable")
		}
	}
	return nil
}

func cloneToken(t artifact.TokenRecord) artifact.TokenRecord {
	t.Groups = slices.Clone(t.Groups)
	if t.Revoked != nil {
		at := *t.Revoked
		t.Revoked = &at
	}
	return t
}

// pruneTokens drops tokens expired for tokenRetention at now.
func (d *Data) pruneTokens(now time.Time) {
	d.tokens = slices.DeleteFunc(d.tokens, func(r artifact.TokenRecord) bool { return now.Sub(r.Expires) >= tokenRetention })
}

// SaveToken records a new token; an existing ID or hash is a conflict.
func (s *Store) SaveToken(ctx context.Context, r artifact.TokenRecord) error {
	if err := validateToken(r); err != nil {
		return err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	r = cloneToken(r)
	r.Created, r.Expires = r.Created.UTC(), r.Expires.UTC()
	for _, o := range d.tokens {
		if o.ID == r.ID || o.Hash == r.Hash {
			return fmt.Errorf("%w: token exists", artifact.ErrConflict)
		}
	}
	now := time.Now()
	live := 0
	for _, o := range d.tokens {
		if now.Sub(o.Expires) < tokenRetention {
			live++
		}
	}
	if live >= maxTokens {
		return fmt.Errorf("%w: at most %d tokens", artifact.ErrLimit, maxTokens)
	}
	d.tokens = append(d.tokens, r)
	d.pruneTokens(now)
	return nil
}

// TokenByHash finds a token, revoked and expired ones included.
func (s *Store) TokenByHash(ctx context.Context, hash string) (artifact.TokenRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.TokenRecord{}, err
	}
	for _, r := range d.tokens {
		if r.Hash == hash {
			return cloneToken(r), nil
		}
	}
	return artifact.TokenRecord{}, artifact.ErrNotFound
}

// Tokens lists a principal's tokens, or all for "", newest first.
func (s *Store) Tokens(ctx context.Context, principal string) ([]artifact.TokenRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return nil, err
	}
	out := []artifact.TokenRecord{}
	for i := len(d.tokens) - 1; i >= 0; i-- {
		if r := d.tokens[i]; principal == "" || r.Principal == principal {
			out = append(out, cloneToken(r))
		}
	}
	return out, nil
}

// RevokeToken marks a token revoked; revoking twice changes nothing.
func (s *Store) RevokeToken(ctx context.Context, id string, at time.Time) (artifact.TokenRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.TokenRecord{}, err
	}
	i := slices.IndexFunc(d.tokens, func(r artifact.TokenRecord) bool { return r.ID == id })
	if i < 0 {
		return artifact.TokenRecord{}, artifact.ErrNotFound
	}
	if d.tokens[i].Revoked == nil {
		at = at.UTC()
		d.tokens[i].Revoked = &at
	}
	return cloneToken(d.tokens[i]), nil
}

func validateSession(r artifact.SessionRecord) error {
	if h, err := hex.DecodeString(r.Hash); err != nil || len(h) != 32 || r.Hash != strings.ToLower(r.Hash) {
		return invalid("session hash must be a lowercase hex SHA-256")
	}
	if r.Principal == "" || len(r.Groups) > maxTokenGroups {
		return invalid("session needs a principal and at most 64 groups")
	}
	if r.Created.IsZero() || !r.Expires.After(r.Created) {
		return invalid("session needs a creation time before its expiry")
	}
	for _, f := range append([]string{r.Principal, r.Name}, r.Groups...) {
		if len(f) > maxTokenField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("session field too long or not printable")
		}
	}
	return nil
}

func cloneSession(r artifact.SessionRecord) artifact.SessionRecord {
	r.Groups = slices.Clone(r.Groups)
	return r
}

// SaveSession records a new session; an existing hash is a conflict.
// Sessions that have expired are dropped.
func (s *Store) SaveSession(ctx context.Context, r artifact.SessionRecord) error {
	if err := validateSession(r); err != nil {
		return err
	}
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	r = cloneSession(r)
	r.Created, r.Expires = r.Created.UTC(), r.Expires.UTC()
	now := time.Now()
	live := 0
	for _, o := range d.sessions {
		if o.Hash == r.Hash {
			return fmt.Errorf("%w: session exists", artifact.ErrConflict)
		}
		if now.Before(o.Expires) {
			live++
		}
	}
	if live >= maxSessions {
		return fmt.Errorf("%w: at most %d sessions", artifact.ErrLimit, maxSessions)
	}
	d.sessions = append(d.sessions, r)
	d.sessions = slices.DeleteFunc(d.sessions, func(o artifact.SessionRecord) bool { return !now.Before(o.Expires) })
	return nil
}

// SessionByHash finds a session; an expired one that has not yet been dropped
// is included, and the caller decides whether it is still valid.
func (s *Store) SessionByHash(ctx context.Context, hash string) (artifact.SessionRecord, error) {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return artifact.SessionRecord{}, err
	}
	for _, r := range d.sessions {
		if r.Hash == hash {
			return cloneSession(r), nil
		}
	}
	return artifact.SessionRecord{}, artifact.ErrNotFound
}

// DeleteSession ends a session; an unknown one is not an error.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := s.ready(ctx.Err()); err != nil {
		return err
	}
	d.sessions = slices.DeleteFunc(d.sessions, func(r artifact.SessionRecord) bool { return r.Hash == hash })
	now := time.Now()
	d.sessions = slices.DeleteFunc(d.sessions, func(o artifact.SessionRecord) bool { return !now.Before(o.Expires) })
	return nil
}
