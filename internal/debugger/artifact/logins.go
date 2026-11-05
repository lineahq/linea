package artifact

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// MaxLogins bounds the sign-ins in progress.
const MaxLogins = 1000

// LoginRecord is an OIDC sign-in in progress, from the redirect to the
// identity provider to its callback, keyed by the SHA-256 of its state.
type LoginRecord struct {
	Hash     string    `json:"hash"`
	Verifier string    `json:"verifier"`
	Nonce    string    `json:"nonce"`
	ReturnTo string    `json:"returnTo"`
	Expires  time.Time `json:"expires"`
}

// Logins holds sign-ins in progress, so an instance completes the sign-in
// another instance started. Expired ones are dropped.
type Logins interface {
	// SaveLogin records a sign-in; an existing hash is a conflict, and
	// MaxLogins unexpired sign-ins are ErrLimit.
	SaveLogin(ctx context.Context, r LoginRecord) error
	// TakeLogin deletes a sign-in and returns it, once; an unknown or
	// expired one is ErrNotFound.
	TakeLogin(ctx context.Context, hash string) (LoginRecord, error)
}

// ValidateLogin reports why a repository refuses a sign-in record.
func ValidateLogin(r LoginRecord) error {
	if h, err := hex.DecodeString(r.Hash); err != nil || len(h) != 32 || r.Hash != strings.ToLower(r.Hash) {
		return fmt.Errorf("%w: login hash must be a lowercase hex SHA-256", ErrInvalidRequest)
	}
	switch {
	case r.Verifier == "" || len(r.Verifier) > 128 || !printable(r.Verifier):
		return fmt.Errorf("%w: login verifier must be 1 to 128 printable characters", ErrInvalidRequest)
	case r.Nonce == "" || len(r.Nonce) > 128 || !printable(r.Nonce):
		return fmt.Errorf("%w: login nonce must be 1 to 128 printable characters", ErrInvalidRequest)
	case !strings.HasPrefix(r.ReturnTo, "/") || len(r.ReturnTo) > 2048 || !printable(r.ReturnTo):
		return fmt.Errorf("%w: login return path must be a local path of at most 2048 printable characters", ErrInvalidRequest)
	case r.Expires.IsZero():
		return fmt.Errorf("%w: login needs an expiry", ErrInvalidRequest)
	}
	return nil
}
