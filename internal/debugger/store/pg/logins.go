package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lineahq/linea/internal/debugger/artifact"
)

// SaveLogin records a sign-in in progress; expired ones are dropped.
func (s *Store) SaveLogin(ctx context.Context, r artifact.LoginRecord) error {
	if err := artifact.ValidateLogin(r); err != nil {
		return err
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	return s.inTx(ctx, "login", func(tx pgx.Tx) error {
		if err := lockFamily(ctx, tx, "logins"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM logins WHERE expires_at <= $1", time.Now()); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM logins").Scan(&n); err != nil {
			return err
		}
		if n >= artifact.MaxLogins {
			return fmt.Errorf("%w: at most %d sign-ins in progress", artifact.ErrLimit, artifact.MaxLogins)
		}
		_, err := tx.Exec(ctx, "INSERT INTO logins (hash, verifier, nonce, return_to, expires_at) VALUES ($1, $2, $3, $4, $5)",
			r.Hash, r.Verifier, r.Nonce, r.ReturnTo, r.Expires.UTC())
		return err
	})
}

// TakeLogin deletes a sign-in and returns it, once.
func (s *Store) TakeLogin(ctx context.Context, hash string) (artifact.LoginRecord, error) {
	if err := s.ready(ctx); err != nil {
		return artifact.LoginRecord{}, err
	}
	var r artifact.LoginRecord
	err := s.pool.QueryRow(ctx, "DELETE FROM logins WHERE hash = $1 RETURNING hash, verifier, nonce, return_to, expires_at", hash).
		Scan(&r.Hash, &r.Verifier, &r.Nonce, &r.ReturnTo, &r.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return artifact.LoginRecord{}, artifact.ErrNotFound
	}
	if err != nil {
		return artifact.LoginRecord{}, storageErr("login", err)
	}
	if !time.Now().Before(r.Expires) {
		return artifact.LoginRecord{}, artifact.ErrNotFound
	}
	r.Expires = r.Expires.UTC()
	return r, nil
}
