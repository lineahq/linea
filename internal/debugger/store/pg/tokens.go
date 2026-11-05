package pg

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// Tokens and sessions are outside StorageBytes, like the audit log: a full
// installation must not lock people out. Their counts bound them instead.
const (
	maxTokens      = 1000
	maxSessions    = 10000
	maxTokenField  = 256
	maxTokenGroups = 64
	// tokenRetention is how long an expired token is kept for listing.
	tokenRetention = 30 * 24 * time.Hour
)

func validToken(t artifact.TokenRecord) error {
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
	if !validText(append([]string{t.Principal, t.Name, t.Label, t.CreatedBy}, t.Groups...)...) {
		return invalid("token field is not valid text")
	}
	for _, f := range append([]string{t.Principal, t.Name, t.Label, t.CreatedBy}, t.Groups...) {
		if len(f) > maxTokenField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("token field too long or not printable")
		}
	}
	return nil
}

func validSession(r artifact.SessionRecord) error {
	if h, err := hex.DecodeString(r.Hash); err != nil || len(h) != 32 || r.Hash != strings.ToLower(r.Hash) {
		return invalid("session hash must be a lowercase hex SHA-256")
	}
	if r.Principal == "" || len(r.Groups) > maxTokenGroups {
		return invalid("session needs a principal and at most 64 groups")
	}
	if r.Created.IsZero() || !r.Expires.After(r.Created) {
		return invalid("session needs a creation time before its expiry")
	}
	if !validText(append([]string{r.Principal, r.Name}, r.Groups...)...) {
		return invalid("session field is not valid text")
	}
	for _, f := range append([]string{r.Principal, r.Name}, r.Groups...) {
		if len(f) > maxTokenField || strings.ContainsFunc(f, unicode.IsControl) {
			return invalid("session field too long or not printable")
		}
	}
	return nil
}

// lockFamily serializes the writers of one record family (tokens, sessions)
// for the transaction, so their count limit holds across instances.
func lockFamily(ctx context.Context, tx pgx.Tx, family string) error {
	_, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('linea.debugger.store.' || current_schema() || ':' || $1, 0))", family)
	return err
}

const tokenColumns = "id, hash, principal, kind, name, groups, label, created_by, created_at, expires_at, revoked_at"

func scanToken(row pgx.Row) (artifact.TokenRecord, error) {
	var t artifact.TokenRecord
	err := row.Scan(&t.ID, &t.Hash, &t.Principal, &t.Kind, &t.Name, &t.Groups, &t.Label, &t.CreatedBy, &t.Created, &t.Expires, &t.Revoked)
	t.Created, t.Expires = t.Created.UTC(), t.Expires.UTC()
	if t.Revoked != nil {
		at := t.Revoked.UTC()
		t.Revoked = &at
	}
	if len(t.Groups) == 0 {
		t.Groups = nil
	}
	return t, err
}

// SaveToken records a new token. Its ID and hash must be new.
func (s *Store) SaveToken(ctx context.Context, t artifact.TokenRecord) error {
	if err := validToken(t); err != nil {
		return err
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	groups := slices.Clone(t.Groups)
	if groups == nil {
		groups = []string{}
	}
	return s.inTx(ctx, "token", func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "tokens"); err != nil {
			return err
		}
		at := time.Now()
		if _, err := tx.Exec(ctx, "DELETE FROM tokens WHERE expires_at < $1", at.Add(-tokenRetention)); err != nil {
			return err
		}
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM tokens WHERE id = $1 OR hash = $2)", t.ID, t.Hash).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("%w: token exists", artifact.ErrConflict)
		}
		var live int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM tokens").Scan(&live); err != nil {
			return err
		}
		if live >= maxTokens {
			return fmt.Errorf("%w: at most %d tokens", artifact.ErrLimit, maxTokens)
		}
		_, err := tx.Exec(ctx, `INSERT INTO tokens (id, hash, principal, kind, name, groups, label, created_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, t.ID, t.Hash, t.Principal, t.Kind, t.Name, groups, t.Label, t.CreatedBy, t.Created.UTC(), t.Expires.UTC())
		return err
	})
}

// TokenByHash returns the token with the SHA-256 hash, revoked and expired
// ones included; the caller decides whether it is still valid.
func (s *Store) TokenByHash(ctx context.Context, hash string) (artifact.TokenRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.TokenRecord{}, err
	}
	t, err := scanToken(s.pool.QueryRow(ctx, "SELECT "+tokenColumns+" FROM tokens WHERE hash = $1", hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.TokenRecord{}, artifact.ErrNotFound
	}
	return t, storageErr("token", err)
}

// Tokens returns the principal's tokens, or every token for "", newest first.
func (s *Store) Tokens(ctx context.Context, principal string) ([]artifact.TokenRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT "+tokenColumns+" FROM tokens WHERE $1 = '' OR principal = $1 ORDER BY seq DESC", principal)
	if err != nil {
		return nil, storageErr("tokens", err)
	}
	defer rows.Close()
	out := []artifact.TokenRecord{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, storageErr("tokens", err)
		}
		out = append(out, t)
	}
	return out, storageErr("tokens", rows.Err())
}

// RevokeToken revokes a token at the given time and returns it. Revoking a
// revoked token changes nothing.
func (s *Store) RevokeToken(ctx context.Context, id string, at time.Time) (artifact.TokenRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.TokenRecord{}, err
	}
	var t artifact.TokenRecord
	err := s.inTx(ctx, "revoke token", func(tx pgx.Tx) error {
		var err error
		t, err = scanToken(tx.QueryRow(ctx, "SELECT "+tokenColumns+" FROM tokens WHERE id = $1 FOR UPDATE", id))
		if errors.Is(err, pgx.ErrNoRows) {
			return artifact.ErrNotFound
		}
		if err != nil || t.Revoked != nil {
			return err
		}
		at = at.UTC()
		t.Revoked = &at
		_, err = tx.Exec(ctx, "UPDATE tokens SET revoked_at = $2 WHERE id = $1", id, at)
		return err
	})
	if err != nil {
		return artifact.TokenRecord{}, err
	}
	return t, nil
}

// SaveSession records a new session. Its hash must be new. Expired sessions
// are dropped here, so an expired session is saved nowhere.
func (s *Store) SaveSession(ctx context.Context, r artifact.SessionRecord) error {
	if err := validSession(r); err != nil {
		return err
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	groups := slices.Clone(r.Groups)
	if groups == nil {
		groups = []string{}
	}
	return s.inTx(ctx, "session", func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "sessions"); err != nil {
			return err
		}
		at := time.Now()
		if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= $1", at); err != nil {
			return err
		}
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM sessions WHERE hash = $1)", r.Hash).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return fmt.Errorf("%w: session exists", artifact.ErrConflict)
		}
		var live int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&live); err != nil {
			return err
		}
		if live >= maxSessions {
			return fmt.Errorf("%w: at most %d sessions", artifact.ErrLimit, maxSessions)
		}
		if !at.Before(r.Expires) {
			return nil
		}
		_, err := tx.Exec(ctx, "INSERT INTO sessions (hash, principal, name, groups, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6)",
			r.Hash, r.Principal, r.Name, groups, r.Created.UTC(), r.Expires.UTC())
		return err
	})
}

// SessionByHash returns the session with the SHA-256 hash. Sessions expired
// before the last session write are gone; the caller checks the rest.
func (s *Store) SessionByHash(ctx context.Context, hash string) (artifact.SessionRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.SessionRecord{}, err
	}
	var r artifact.SessionRecord
	err := s.pool.QueryRow(ctx, "SELECT hash, principal, name, groups, created_at, expires_at FROM sessions WHERE hash = $1", hash).
		Scan(&r.Hash, &r.Principal, &r.Name, &r.Groups, &r.Created, &r.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.SessionRecord{}, artifact.ErrNotFound
	}
	r.Created, r.Expires = r.Created.UTC(), r.Expires.UTC()
	if len(r.Groups) == 0 {
		r.Groups = nil
	}
	return r, storageErr("session", err)
}

// DeleteSession ends the session with the hash and drops expired ones; ending
// an unknown session changes nothing.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	if err := s.ready(ctx); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, "DELETE FROM sessions WHERE hash = $1 OR expires_at <= $2", hash, time.Now())
	return storageErr("session", err)
}
